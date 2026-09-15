package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseListenAddr(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{":8080", "", 8080},
		{"127.0.0.1:9090", "127.0.0.1", 9090},
		{"", "", 8080},
	}
	for _, tc := range cases {
		h, p, err := parseListenAddr(tc.in)
		if err != nil {
			t.Fatalf("addr=%q: %v", tc.in, err)
		}
		if h != tc.wantHost || p != tc.wantPort {
			t.Fatalf("addr=%q got %q %d want %q %d", tc.in, h, p, tc.wantHost, tc.wantPort)
		}
	}
	if _, _, err := parseListenAddr("not-an-addr"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildConfigEmptyAPIKeys(t *testing.T) {
	cfg, path, err := buildConfig(":18080")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cfg.AuthDir)
	if cfg.Port != 18080 {
		t.Fatalf("port=%d", cfg.Port)
	}
	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys=%v want empty", cfg.APIKeys)
	}
	if path == "" || cfg.AuthDir == "" {
		t.Fatal("missing paths")
	}
	if !cfg.RemoteManagement.DisableControlPanel {
		t.Fatal("expected control panel disabled")
	}
	authPath := filepath.Join(cfg.AuthDir, nvidiaAuthFileName)
	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("nvidia auth file: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"nvidia"`) {
		t.Fatalf("auth file contents=%q", raw)
	}
}

func TestNvidiaModelHookReregisters(t *testing.T) {
	models := nvidia.RegistryModels()
	if len(models) == 0 {
		t.Fatal("empty registry models")
	}
	const clientID = "nvidia-local.json"
	reg := cliproxy.GlobalModelRegistry()
	reg.RegisterClient(clientID, nvidiaProvider, models)
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	reg.UnregisterClient(clientID)
	hook := &nvidiaModelHook{getModels: func() []*cliproxy.ModelInfo { return models }}
	hook.OnModelsUnregistered(context.Background(), nvidiaProvider, clientID)

	got := reg.GetAvailableModelsByProvider(nvidiaProvider)
	if len(got) == 0 {
		t.Fatal("models not restored after unregister hook")
	}
}

func TestBindNvidiaRuntimeRegistersExecutor(t *testing.T) {
	models := nvidia.RegistryModels()
	core := coreauth.NewManager(nil, nil, nil)
	exec := nvidia.NewExecutor(nvidia.Options{})
	if _, err := core.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID:       "nvidia-test",
		Provider: nvidiaProvider,
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	n := bindNvidiaRuntime(core, exec, models)
	if n != 1 {
		t.Fatalf("auth count=%d", n)
	}
	got, ok := core.Executor(nvidiaProvider)
	if !ok || got != exec {
		t.Fatalf("executor not bound: ok=%v got=%T", ok, got)
	}
	t.Cleanup(func() {
		cliproxy.GlobalModelRegistry().UnregisterClient("nvidia-test")
	})
}

func TestBindNvidiaRuntimePrimesModelsBeforeAuthLoad(t *testing.T) {
	models := nvidia.RegistryModels()
	reg := cliproxy.GlobalModelRegistry()
	reg.UnregisterClient(nvidiaAuthFileName)
	t.Cleanup(func() { reg.UnregisterClient(nvidiaAuthFileName) })

	core := coreauth.NewManager(nil, nil, nil)
	bindNvidiaRuntime(core, nvidia.NewExecutor(nvidia.Options{}), models)

	got := reg.GetAvailableModelsByProvider(nvidiaProvider)
	if len(got) == 0 {
		t.Fatal("models unavailable before auth load")
	}
}

// requestLogger must record /v1/messages calls for custom non-messages_only
// providers (CLIProxyAPI's compat client records nothing) and must not touch
// any other path.
func TestRequestLoggerRecordsCustomMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GNVIPI_STATS_FILE", filepath.Join(os.TempDir(), "gnvipi-capture-test-stats.jsonl"))
	p := &modelPrefs{providers: []customProvider{
		{Name: "b-ai-test", Model: "qwen3.8-flash"},
		{Name: "mw-test", Model: "moe", MessagesOnly: true},
	}}
	engine := gin.New()
	engine.Use(requestLogger(p))
	handler := func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.String(http.StatusOK, `{"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}
	engine.POST("/v1/messages", handler)
	engine.POST("/v1/chat/completions", handler)

	post := func(path, model string) {
		body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
	for _, tc := range []struct{ path, model string }{
		{"/v1/messages", "b-ai-test/qwen3.8-flash"}, // recorded
		{"/v1/messages", "mw-test/moe"},             // messages_only -> other recorder
		{"/v1/chat/completions", "b-ai-test/qwen3.8-flash"},
		{"/v1/messages", "nvidia/gpt-oss-120b"}, // not a custom provider
	} {
		post(tc.path, tc.model)
	}

	const want = "b-ai-test/qwen3.8-flash"
	var got *nvidia.ModelStats
	snap := nvidia.GlobalStats.Snapshot()
	for i := range snap {
		if snap[i].Model == want {
			got = &snap[i]
		}
	}
	if got == nil {
		t.Fatalf("no stats row for %q", want)
	}
	if got.Requests != 1 {
		t.Fatalf("requests=%d, want 1 (must not double-count other paths)", got.Requests)
	}
	if got.InputTok != 7 || got.OutputTok != 3 {
		t.Fatalf("tokens=%d/%d, want 7/3 scraped from captured body", got.InputTok, got.OutputTok)
	}
}
