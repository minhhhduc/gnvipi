package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"glm52-nvidia/internal/provider/nvidia"

	"github.com/gin-gonic/gin"
)

// requestLogger must record /v1/messages calls for custom non-messages_only
// providers (CLIProxyAPI's compat client records nothing) and must not touch
// any other path.
func TestRequestLoggerRecordsCustomMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GLM52_STATS_FILE", filepath.Join(os.TempDir(), "glm52-nvidia-capture-test-stats.jsonl"))
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
