package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func TestGatewayV2RetainsPersistedProviderPolicies(t *testing.T) {
	log := &upstreamLog{}
	proxy := characterizationProxy(t, log, `{"id":"policy-response"}`)
	catalog := provider.Document{Providers: []provider.Provider{{
		ID: "stable-provider-id", Name: "display-name", Enabled: true, Protocol: ir.ProtocolOpenAIChat,
		BaseURL: "http://127.0.0.1:1/base", ProxyURL: proxy,
		Auth:    provider.Auth{Scheme: provider.AuthBearer, Key: "policy-key", HeaderName: "X-Upstream-Key"},
		Headers: map[string]string{"X-Static-Policy": "required"}, ForwardHeaders: []string{"X-Correlation"},
		EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/custom-chat"},
		Models:        []provider.ModelMapping{{Alias: "policy/chat", UpstreamModel: "upstream-model"}, {Alias: "policy/hidden", UpstreamModel: "hidden-model", Hidden: true}},
	}}}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := provider.WriteDocument(path, catalog); err != nil {
		t.Fatal(err)
	}
	prefs := &modelPrefs{}
	if err := prefs.load(path); err != nil {
		t.Fatal(err)
	}
	handler, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(gatewayRequestContext(), handler)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("configured alias fell through") })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"policy/chat","messages":[]}`))
	request.Header.Set("X-Correlation", "caller-correlation")
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, request)
	if out.Code != http.StatusOK || out.Body.String() != `{"id":"policy-response"}` {
		t.Fatalf("v2 provider policy was lost: %d %s", out.Code, out.Body.String())
	}
	call := log.last(t)
	if call.URL != "http://127.0.0.1:1/base/custom-chat" || call.Header.Get("X-Upstream-Key") != "Bearer policy-key" || call.Header.Get("Authorization") != "" || call.Header.Get("X-Static-Policy") != "required" || call.Header.Get("X-Correlation") != "caller-correlation" {
		t.Fatalf("persisted policy did not reach upstream: %s %v", call.URL, call.Header)
	}
	hidden := httptest.NewRecorder()
	engine.ServeHTTP(hidden, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"policy/hidden"}`)))
	if hidden.Code != http.StatusNotFound || log.count() != 1 {
		t.Fatalf("hidden v2 mapping escaped routing policy: %d %s; upstream calls=%d", hidden.Code, hidden.Body.String(), log.count())
	}
}

func cycleModelStats() nvidia.ModelStats {
	rows := nvidia.GlobalStats.Snapshot()
	for i := range rows {
		if rows[i].Model == "cycle" {
			return rows[i]
		}
	}
	return nvidia.ModelStats{}
}

func TestGatewayV2CycleRecordsCommittedOutcomeOnce(t *testing.T) {
	before := cycleModelStats()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"model":"first"`) {
			w.WriteHeader(http.StatusTooManyRequests)
			if _, err := io.WriteString(w, `{"error":{"message":"retry","type":"rate_limit_error"}}`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := io.WriteString(w, `{"id":"winner","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	prefs := &modelPrefs{providers: []customProvider{{Name: "vendor", Model: "first", BaseURL: upstream.URL}, {Name: "vendor", Model: "second", BaseURL: upstream.URL}}, cycle: &cycleConfig{Name: "cycle", Models: []string{"vendor/first", "vendor/second"}}}
	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(gatewayRequestContext(), cycleRouter(prefs, engine), dispatcher)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("cycle custom alias reached downstream") })
	out := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cycle","messages":[{"role":"user","content":"ping"}]}`))
	request.Header.Set(core.RequestIDHeader, "cycle-logical-request")
	engine.ServeHTTP(out, request)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"id":"winner"`) || out.Header().Get(core.RequestIDHeader) != "cycle-logical-request" {
		t.Fatalf("cycle did not commit its successful response: %d %s", out.Code, out.Body.String())
	}
	after := cycleModelStats()
	if after.Requests-before.Requests != 1 || after.Errors-before.Errors != 0 || after.InputTok-before.InputTok != 3 || after.OutputTok-before.OutputTok != 5 {
		t.Fatalf("cycle logical accounting kept a failed attempt or lost final usage: before=%+v after=%+v", before, after)
	}
}
