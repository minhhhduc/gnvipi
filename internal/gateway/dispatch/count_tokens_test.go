package dispatch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	outboundchat "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
	outboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
	gwpolicy "github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func newCountTokensRegistry() *adapter.Registry {
	r := adapter.NewRegistry()
	r.RegisterInbound(inboundanthropic.New())
	r.RegisterOutbound(outboundanthropic.New())
	r.RegisterOutbound(outboundchat.New())
	r.RegisterOutbound(outboundresponses.New())
	return r
}

func TestCountTokens_AnthropicUpstreamSameProtocolRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path = %s, want /v1/messages/count_tokens", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "anthropic-key" {
			t.Errorf("upstream X-Api-Key = %s, want anthropic-key", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"input_tokens":42}`)); writeErr != nil {
			t.Error(writeErr)
		}
	}))
	defer upstream.Close()

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:         "anthropic-vendor",
			BaseURL:      upstream.URL,
			APIKey:       "anthropic-key",
			Model:        "claude-3-opus",
			Alias:        "claude-alias",
			MessagesOnly: true,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		t.Error("route was not intercepted by dispatcher")
	})

	body := `{"model":"claude-alias","messages":[{"role":"user","content":"Hello Anthropic"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if !upstreamCalled {
		t.Fatal("expected upstream to be called for Anthropic model")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "" {
		t.Fatalf("expected no X-Gateway-Token-Estimate header for Anthropic upstream, got %q", estimate)
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.InputTokens != 42 {
		t.Fatalf("input_tokens = %d, want 42", resp.InputTokens)
	}
}

func TestCountTokens_NonAnthropicProviderReturnsLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		t.Error("upstream should NOT be called for non-Anthropic count_tokens estimate")
	}))
	defer upstream.Close()

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:    "openai-vendor",
			BaseURL: upstream.URL,
			APIKey:  "openai-key",
			Model:   "gpt-4o",
			Alias:   "gpt-4o-alias",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		t.Error("route was not intercepted by dispatcher")
	})

	body := `{"model":"gpt-4o-alias","messages":[{"role":"user","content":"Explain quantum mechanics in simple terms"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if upstreamCalled {
		t.Fatal("upstream must NOT be called for non-Anthropic model count_tokens")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, w.Body.String())
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive input_tokens, got %d", resp.InputTokens)
	}
}

func TestCountTokens_OpenAIResponsesProviderReturnsLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		t.Error("upstream should NOT be called")
	}))
	defer upstream.Close()

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:          "responses-vendor",
			BaseURL:       upstream.URL,
			APIKey:        "responses-key",
			Model:         "gpt-4o-realtime",
			Alias:         "responses-alias",
			ResponsesOnly: true,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		t.Error("route was not intercepted by dispatcher")
	})

	body := `{"model":"responses-alias","messages":[{"role":"user","content":"Hello responses model"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if upstreamCalled {
		t.Fatal("upstream must not be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive input_tokens, got %d", resp.InputTokens)
	}
}

func TestCountTokens_InvalidRequestBodyReturnsAnthropicError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:    "openai-vendor",
			BaseURL: "https://example.invalid",
			APIKey:  "key",
			Model:   "gpt-4o",
			Alias:   "gpt-alias",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		t.Error("invalid request should not reach downstream")
	})

	// Invalid role "invalid_role" should be rejected by inbound Anthropic adapter
	body := `{"model":"gpt-alias","messages":[{"role":"invalid_role","content":"hello"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error envelope: %v; body = %s", err, w.Body.String())
	}
	if errResp.Type != "error" || errResp.Error.Type != "invalid_request_error" {
		t.Fatalf("unexpected error response shape: %+v", errResp)
	}
}

func TestCountTokens_FallthroughNvidiaModelPreservesLegacyFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:    "vendor",
			BaseURL: "https://example.invalid",
			Model:   "other",
			Alias:   "other",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	reachedDownstream := false
	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		reachedDownstream = true
		body, readErr := io.ReadAll(c.Request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		c.Data(http.StatusOK, "application/json", body)
	})

	// Unregistered model (e.g. nvidia model) falls through to next middleware
	body := `{"model":"deepseek-ai/deepseek-v4-flash-0731","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if !reachedDownstream {
		t.Fatal("expected fallthrough route to reach downstream handler")
	}
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("downstream received modified payload: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCountTokens_NilRegistryUsesDefaultAnthropicAdapter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:    "openai-vendor",
			BaseURL: "https://example.invalid",
			APIKey:  "openai-key",
			Model:   "gpt-4o",
			Alias:   "gpt-alias",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = nil // explicitly nil registry

	engine := gin.New()
	engine.Use(d.Handle)
	engine.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		t.Error("route was not intercepted by dispatcher")
	})

	body := `{"model":"gpt-alias","messages":[{"role":"user","content":"Count my tokens"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive input_tokens, got %d", resp.InputTokens)
	}
}

func TestCountTokens_ProviderMaxConcurrencyOccupiedSucceedsImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Upstream server blocks normal chat completion request to keep MaxConcurrency slot occupied.
	started := make(chan struct{})
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-block
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)); writeErr != nil {
			t.Error(writeErr)
		}
	}))
	defer upstream.Close()

	prov := provider.Provider{
		ID:             "openai-vendor",
		Name:           "openai-vendor",
		BaseURL:        upstream.URL,
		Protocol:       ir.ProtocolOpenAIChat,
		Enabled:        true,
		MaxConcurrency: 1,
		Models: []provider.ModelMapping{
			{Alias: "gpt-4o-alias", UpstreamModel: "gpt-4o"},
		},
	}
	resolver, err := routing.NewRegistryResolver([]provider.Provider{prov}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)

	// Launch an in-flight request to fill the single concurrency slot of openai-vendor.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		chatReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o-alias","messages":[{"role":"user","content":"hold"}]}`))
		chatRec := httptest.NewRecorder()
		engine.ServeHTTP(chatRec, chatReq)
	}()

	// Wait until the in-flight request has acquired the concurrency slot and reached upstream.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(block)
		wg.Wait()
		t.Fatal("timed out waiting for in-flight request to start")
	}

	// While the in-flight request completely occupies the provider's MaxConcurrency slot,
	// issue a non-Anthropic count_tokens request. It should succeed immediately without acquiring
	// or blocking on the limiter slot.
	countReqBody := `{"model":"gpt-4o-alias","messages":[{"role":"user","content":"Count this text"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(countReqBody))
	rc := core.NewRequestContext("test-req-id")
	req = req.WithContext(core.WithRequestContext(req.Context(), rc))

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// Clean up the in-flight request.
	close(block)
	wg.Wait()

	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}
	if rc.FirstByte.IsZero() {
		t.Fatal("expected rc.FirstByte to be non-zero")
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, w.Body.String())
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive input_tokens, got %d", resp.InputTokens)
	}
}

func TestCountTokens_AnthropicUpstreamCountTokensDisabledByFeaturePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		t.Error("upstream should NOT be called when count_tokens is disabled by feature policy")
	}))
	defer upstream.Close()

	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{
		{
			Name:         "anthropic-vendor",
			BaseURL:      upstream.URL,
			APIKey:       "anthropic-key",
			Model:        "claude-3-opus",
			Alias:        "claude-alias",
			MessagesOnly: true,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()
	disabled := false
	d.FeaturePolicy = gwpolicy.Config{
		ProviderOverrides: map[string]gwpolicy.CapabilityOverrides{
			"anthropic-vendor": {CountTokens: &disabled},
		},
	}

	engine := gin.New()
	engine.Use(d.Handle)

	body := `{"model":"claude-alias","messages":[{"role":"user","content":"Hello Anthropic"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if upstreamCalled {
		t.Fatal("upstream must NOT be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}
}

func TestCountTokens_AnthropicUpstreamCountTokensDisabledByExtension(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		t.Error("upstream should NOT be called when count_tokens is disabled by extension")
	}))
	defer upstream.Close()

	prov := provider.Provider{
		ID:       "anthropic-ext",
		Name:     "anthropic-ext",
		BaseURL:  upstream.URL,
		Protocol: ir.ProtocolAnthropicMessages,
		Enabled:  true,
		Models: []provider.ModelMapping{
			{Alias: "claude-ext-alias", UpstreamModel: "claude-3"},
		},
		Extensions: map[string]json.RawMessage{
			"capabilities": json.RawMessage(`{"count_tokens":false}`),
		},
	}
	resolver, err := routing.NewRegistryResolver([]provider.Provider{prov}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := New(resolver, nil)
	d.Registry = newCountTokensRegistry()

	engine := gin.New()
	engine.Use(d.Handle)

	body := `{"model":"claude-ext-alias","messages":[{"role":"user","content":"Hello Anthropic"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if upstreamCalled {
		t.Fatal("upstream must NOT be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if estimate := w.Header().Get("X-Gateway-Token-Estimate"); estimate != "true" {
		t.Fatalf("X-Gateway-Token-Estimate = %q, want \"true\"", estimate)
	}
}
