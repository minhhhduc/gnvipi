package dispatch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	inboundchat "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openaichat"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	outboundchat "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func setupTestRegistry() *adapter.Registry {
	r := adapter.NewRegistry()
	r.RegisterInbound(inboundanthropic.New())
	r.RegisterInbound(inboundchat.New())
	r.RegisterOutbound(outboundanthropic.New())
	r.RegisterOutbound(outboundchat.New())
	return r
}

func mustWriteString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Logf("mustWriteString: %v", err)
	}
}

func TestFallbackTriggersOnUpstreamErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primaryCalls, fallbackCalls atomic.Int32

			primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				mustWriteString(t, w, `{"error":"upstream failure"}`)
			}))
			defer primaryServer.Close()

			fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				mustWriteString(t, w, `{"choices":[{"message":{"content":"fallback-ok"}}],"model":"fallback-model"}`)
			}))
			defer fallbackServer.Close()

			p1 := provider.Provider{
				ID:       "prov-primary",
				Name:     "prov-primary",
				BaseURL:  primaryServer.URL + "/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     provider.Auth{Scheme: provider.AuthNone},
				Enabled:  true,
				Extensions: map[string]json.RawMessage{
					"retry": json.RawMessage(`{"max_attempts":1}`),
				},
				Models: []provider.ModelMapping{
					{
						Alias:         "chat-primary",
						UpstreamModel: "chat-m1",
						Fallbacks:     []string{"chat-backup"},
					},
				},
			}
			p2 := provider.Provider{
				ID:       "prov-backup",
				Name:     "prov-backup",
				BaseURL:  fallbackServer.URL + "/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     provider.Auth{Scheme: provider.AuthNone},
				Enabled:  true,
				Models: []provider.ModelMapping{
					{
						Alias:         "chat-backup",
						UpstreamModel: "chat-m2",
					},
				},
			}

			resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
			if err != nil {
				t.Fatalf("NewRegistryResolver: %v", err)
			}

			d := New(resolver, nil)
			engine := gin.New()
			engine.POST("/v1/chat/completions", d.Handle)

			body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if primaryCalls.Load() != 1 {
				t.Errorf("primaryCalls = %d, want 1", primaryCalls.Load())
			}
			if fallbackCalls.Load() != 1 {
				t.Errorf("fallbackCalls = %d, want 1", fallbackCalls.Load())
			}
			if w.Code != http.StatusOK {
				t.Fatalf("response code = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if fallbackHeader := w.Header().Get("X-Gateway-Fallback"); fallbackHeader != "chat-backup" {
				t.Errorf("X-Gateway-Fallback = %q, want %q", fallbackHeader, "chat-backup")
			}
			if !strings.Contains(w.Body.String(), "fallback-ok") {
				t.Errorf("body missing fallback-ok: %s", w.Body.String())
			}
		})
	}
}

func TestFallbackConnectOrTimeoutError(t *testing.T) {
	// Create a listener and immediately close it to simulate connection refused
	closedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closedServer.URL
	closedServer.Close()

	var fallbackCalls atomic.Int32
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"recovered"}}]}`)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-dead",
		Name:     "prov-dead",
		BaseURL:  closedURL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-primary",
				UpstreamModel: "chat-m1",
				Fallbacks:     []string{"chat-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-backup",
		Name:     "prov-backup",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-backup",
				UpstreamModel: "chat-m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if fallbackCalls.Load() != 1 {
		t.Errorf("fallbackCalls = %d, want 1", fallbackCalls.Load())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if fallbackHeader := w.Header().Get("X-Gateway-Fallback"); fallbackHeader != "chat-backup" {
		t.Errorf("X-Gateway-Fallback = %q, want %q", fallbackHeader, "chat-backup")
	}
}

func TestFallbackNeverAttemptedPostFirstByte(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Write first byte / chunk to downstream
		mustWriteString(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"first-byte\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Then abort connection
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil && conn != nil {
				if closeErr := conn.Close(); closeErr != nil {
					t.Logf("conn.Close: %v", closeErr)
				}
			}
		}
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback\"}}]}\n\n")
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-stream",
		Name:     "prov-stream",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-primary",
				UpstreamModel: "chat-m1",
				Fallbacks:     []string{"chat-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-backup",
		Name:     "prov-backup",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-backup",
				UpstreamModel: "chat-m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"chat-primary","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if primaryCalls.Load() != 1 {
		t.Errorf("primaryCalls = %d, want 1", primaryCalls.Load())
	}
	if fallbackCalls.Load() != 0 {
		t.Errorf("fallbackCalls = %d, want 0 (fallback must never be attempted post-first-byte)", fallbackCalls.Load())
	}
	if fallbackHeader := w.Header().Get("X-Gateway-Fallback"); fallbackHeader != "" {
		t.Errorf("X-Gateway-Fallback header should NOT be present post-first-byte failure, got %q", fallbackHeader)
	}
}

func Test4xxErrorsReturnDirectlyWithoutFallback(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primaryCalls, fallbackCalls atomic.Int32

			primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				mustWriteString(t, w, `{"error":{"message":"client error","type":"invalid_request"}}`)
			}))
			defer primaryServer.Close()

			fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				mustWriteString(t, w, `{"choices":[{"message":{"content":"should not be reached"}}]}`)
			}))
			defer fallbackServer.Close()

			p1 := provider.Provider{
				ID:       "prov-client-err",
				Name:     "prov-client-err",
				BaseURL:  primaryServer.URL + "/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     provider.Auth{Scheme: provider.AuthNone},
				Enabled:  true,
				Models: []provider.ModelMapping{
					{
						Alias:         "chat-primary",
						UpstreamModel: "chat-m1",
						Fallbacks:     []string{"chat-backup"},
					},
				},
			}
			p2 := provider.Provider{
				ID:       "prov-backup",
				Name:     "prov-backup",
				BaseURL:  fallbackServer.URL + "/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     provider.Auth{Scheme: provider.AuthNone},
				Enabled:  true,
				Models: []provider.ModelMapping{
					{
						Alias:         "chat-backup",
						UpstreamModel: "chat-m2",
					},
				},
			}

			resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
			if err != nil {
				t.Fatalf("NewRegistryResolver: %v", err)
			}

			d := New(resolver, nil)
			engine := gin.New()
			engine.POST("/v1/chat/completions", d.Handle)

			body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if primaryCalls.Load() != 1 {
				t.Errorf("primaryCalls = %d, want 1", primaryCalls.Load())
			}
			if fallbackCalls.Load() != 0 {
				t.Errorf("fallbackCalls = %d, want 0 (4xx client errors must return directly without fallback)", fallbackCalls.Load())
			}
			if w.Code != status {
				t.Fatalf("response code = %d, want %d", w.Code, status)
			}
			if fallbackHeader := w.Header().Get("X-Gateway-Fallback"); fallbackHeader != "" {
				t.Errorf("X-Gateway-Fallback header should NOT be present on direct 4xx return, got %q", fallbackHeader)
			}
		})
	}
}

func TestCrossProtocolFallback(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32

	// Primary: Anthropic protocol (/v1/messages) returning 502
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		mustWriteString(t, w, `{"type":"error","error":{"type":"api_error","message":"anthropic upstream down"}}`)
	}))
	defer primaryServer.Close()

	// Fallback: OpenAI Chat protocol (/v1/chat/completions) returning 200 OK
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := `{
			"id": "chatcmpl_cross_fb",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Hello from OpenAI fallback!"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18}
		}`
		mustWriteString(t, w, resp)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-anthropic",
		Name:     "prov-anthropic",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolAnthropicMessages,
		Auth:     provider.Auth{Scheme: provider.AuthAPIKey, Key: "anthropic-key"},
		Enabled:  true,
		Extensions: map[string]json.RawMessage{
			"retry": json.RawMessage(`{"max_attempts":1}`),
		},
		Models: []provider.ModelMapping{
			{
				Alias:         "claude-primary",
				UpstreamModel: "claude-3-5-sonnet",
				Fallbacks:     []string{"openai-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-openai",
		Name:     "prov-openai",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "openai-key"},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "openai-backup",
				UpstreamModel: "gpt-4o",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	d.Registry = setupTestRegistry()

	engine := gin.New()
	engine.POST("/v1/messages", d.Handle)

	// Client sends Anthropic /v1/messages request
	body := `{"model":"claude-primary","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if primaryCalls.Load() != 1 {
		t.Errorf("primaryCalls = %d, want 1", primaryCalls.Load())
	}
	if fallbackCalls.Load() != 1 {
		t.Errorf("fallbackCalls = %d, want 1", fallbackCalls.Load())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify X-Gateway-Fallback header is present with fallback alias
	if fallbackHeader := w.Header().Get("X-Gateway-Fallback"); fallbackHeader != "openai-backup" {
		t.Errorf("X-Gateway-Fallback = %q, want %q", fallbackHeader, "openai-backup")
	}

	// Verify response body is translated to Anthropic format
	var anthropicResp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &anthropicResp); err != nil {
		t.Fatalf("failed to parse Anthropic response: %v; raw body: %s", err, w.Body.String())
	}
	if anthropicResp.Type != "message" || anthropicResp.Role != "assistant" {
		t.Errorf("unexpected Anthropic response structure: %+v", anthropicResp)
	}
	if len(anthropicResp.Content) == 0 || anthropicResp.Content[0].Text != "Hello from OpenAI fallback!" {
		t.Errorf("unexpected content in translated response: %+v", anthropicResp.Content)
	}
}

func TestPrimarySuccessDoesNotSetFallbackHeader(t *testing.T) {
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"primary-ok"}}]}`)
	}))
	defer primaryServer.Close()

	p := provider.Provider{
		ID:       "prov-1",
		Name:     "prov-1",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-primary",
				UpstreamModel: "chat-m1",
				Fallbacks:     []string{"chat-backup"},
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", w.Code)
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "" {
		t.Errorf("X-Gateway-Fallback should be empty on primary success, got %q", fb)
	}
}

func TestCrossProtocolStreamingFallback(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		mustWriteString(t, w, `{"type":"error","error":{"type":"api_error","message":"anthropic down"}}`)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		events := []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Streamed "},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"reply"},"finish_reason":"stop"}]}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, ev := range events {
			mustWriteString(t, w, ev)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-anthropic-stream",
		Name:     "prov-anthropic-stream",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolAnthropicMessages,
		Auth:     provider.Auth{Scheme: provider.AuthAPIKey, Key: "k1"},
		Enabled:  true,
		Extensions: map[string]json.RawMessage{
			"retry": json.RawMessage(`{"max_attempts":1}`),
		},
		Models: []provider.ModelMapping{
			{
				Alias:         "claude-stream",
				UpstreamModel: "claude-3-5-sonnet",
				Fallbacks:     []string{"openai-stream-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-openai-stream",
		Name:     "prov-openai-stream",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "k2"},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "openai-stream-backup",
				UpstreamModel: "gpt-4o",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	d.Registry = setupTestRegistry()

	engine := gin.New()
	engine.POST("/v1/messages", d.Handle)

	body := `{"model":"claude-stream","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if primaryCalls.Load() != 1 {
		t.Errorf("primaryCalls = %d, want 1", primaryCalls.Load())
	}
	if fallbackCalls.Load() != 1 {
		t.Errorf("fallbackCalls = %d, want 1", fallbackCalls.Load())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "openai-stream-backup" {
		t.Errorf("X-Gateway-Fallback = %q, want %q", fb, "openai-stream-backup")
	}
	if !strings.Contains(w.Body.String(), "content_block_delta") || !strings.Contains(w.Body.String(), "Streamed ") {
		t.Errorf("expected Anthropic SSE stream in body, got: %s", w.Body.String())
	}
}

func TestIsFallbackTriggerStatus(t *testing.T) {
	// Defaults: 408, 429, 500-599
	defaultTriggers := []int{408, 429, 500, 501, 502, 503, 504, 505, 599}
	for _, code := range defaultTriggers {
		if !isFallbackTriggerStatus(code) {
			t.Errorf("expected code %d to trigger fallback by default", code)
		}
	}

	defaultNonTriggers := []int{200, 201, 301, 400, 401, 403, 404, 499, 600}
	for _, code := range defaultNonTriggers {
		if isFallbackTriggerStatus(code) {
			t.Errorf("expected code %d NOT to trigger fallback by default", code)
		}
	}

	// Configured overrides
	customList := []int{400, 404}
	if !isFallbackTriggerStatus(400, customList) {
		t.Errorf("expected 400 to trigger with custom list")
	}
	if !isFallbackTriggerStatus(404, customList) {
		t.Errorf("expected 404 to trigger with custom list")
	}
	if isFallbackTriggerStatus(500, customList) {
		t.Errorf("expected 500 NOT to trigger with custom list [400, 404]")
	}
	if isFallbackTriggerStatus(429, customList) {
		t.Errorf("expected 429 NOT to trigger with custom list [400, 404]")
	}

	// Empty configured list falls back to default
	if !isFallbackTriggerStatus(500, []int{}) {
		t.Errorf("expected empty custom list to fall back to default statuses")
	}
}

type testCompletionRecorder struct {
	mu          sync.Mutex
	completions []*core.Completion
}

func (r *testCompletionRecorder) Record(c *core.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completions = append(r.completions, c)
}

func (r *testCompletionRecorder) getCompletions() []*core.Completion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*core.Completion(nil), r.completions...)
}

func TestFallbackPerMappingConfigurableStatuses(t *testing.T) {
	var primaryStatus atomic.Int32
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := int(primaryStatus.Load())
		w.WriteHeader(status)
		mustWriteString(t, w, `{"error":{"message":"primary failure"}}`)
	}))
	defer primaryServer.Close()

	var fallbackCalls atomic.Int32
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"fallback-ok"}}]}`)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-p1",
		Name:     "prov-p1",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:            "primary-model",
				UpstreamModel:    "m1",
				Fallbacks:        []string{"backup-model"},
				FallbackStatuses: []int{http.StatusServiceUnavailable}, // Only 503 triggers fallback
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-p2",
		Name:     "prov-p2",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "backup-model",
				UpstreamModel: "m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	// Case 1: Primary returns 500 (Internal Server Error)
	// Since mapping specifies only 503, 500 should NOT trigger fallback.
	primaryStatus.Store(http.StatusInternalServerError)
	fallbackCalls.Store(0)

	body := `{"model":"primary-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no fallback for 500)", w.Code)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback was called %d times; want 0", fallbackCalls.Load())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "" {
		t.Fatalf("unexpected X-Gateway-Fallback header: %s", fb)
	}

	// Case 2: Primary returns 503 (Service Unavailable)
	// Since 503 is in FallbackStatuses, fallback should trigger.
	primaryStatus.Store(http.StatusServiceUnavailable)
	fallbackCalls.Store(0)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback succeeded)", w.Code)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("fallbackCalls = %d, want 1", fallbackCalls.Load())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "backup-model" {
		t.Fatalf("X-Gateway-Fallback = %q, want %q", fb, "backup-model")
	}
}

func TestFallbackProviderExtensionsConfigurableStatuses(t *testing.T) {
	var primaryStatus atomic.Int32
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := int(primaryStatus.Load())
		w.WriteHeader(status)
		mustWriteString(t, w, `{"error":{"message":"primary failure"}}`)
	}))
	defer primaryServer.Close()

	var fallbackCalls atomic.Int32
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"fallback-ok"}}]}`)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-ext-p1",
		Name:     "prov-ext-p1",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Extensions: map[string]json.RawMessage{
			"fallback_statuses": json.RawMessage(`[408, 504]`),
		},
		Models: []provider.ModelMapping{
			{
				Alias:         "ext-primary",
				UpstreamModel: "m1",
				Fallbacks:     []string{"ext-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-ext-p2",
		Name:     "prov-ext-p2",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "ext-backup",
				UpstreamModel: "m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	d := New(resolver, nil)
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	// Case 1: Primary returns 502 (Bad Gateway).
	// Since extension specifies [408, 504], 502 should NOT trigger fallback.
	primaryStatus.Store(http.StatusBadGateway)
	fallbackCalls.Store(0)

	body := `{"model":"ext-primary","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no fallback for 502)", w.Code)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallbackCalls = %d, want 0", fallbackCalls.Load())
	}

	// Case 2: Primary returns 408 (Request Timeout).
	// 408 is in configured fallback_statuses, so fallback should trigger.
	primaryStatus.Store(http.StatusRequestTimeout)
	fallbackCalls.Store(0)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback succeeded)", w.Code)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("fallbackCalls = %d, want 1", fallbackCalls.Load())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "ext-backup" {
		t.Fatalf("X-Gateway-Fallback = %q, want %q", fb, "ext-backup")
	}
}

func TestFallbackStatsRecording(t *testing.T) {
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		mustWriteString(t, w, `{"error":{"message":"internal error"}}`)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"recovered"}}]}`)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-p1",
		Name:     "prov-p1",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-primary",
				UpstreamModel: "m1",
				Fallbacks:     []string{"chat-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-backup",
		Name:     "prov-backup",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-backup",
				UpstreamModel: "m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	recorder := &testCompletionRecorder{}
	d := New(resolver, &relay.Options{Recorder: recorder})
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "chat-backup" {
		t.Fatalf("X-Gateway-Fallback = %q, want %q", fb, "chat-backup")
	}

	completions := recorder.getCompletions()
	if len(completions) != 1 {
		t.Fatalf("expected 1 completion recorded, got %d", len(completions))
	}
	c := completions[0]
	if c.Request == nil {
		t.Fatalf("c.Request is nil")
	}
	if c.Request.ClientModel != "chat-backup" {
		t.Errorf("ClientModel = %q, want %q (final fallback alias)", c.Request.ClientModel, "chat-backup")
	}
	if c.Request.ResolvedProvider != "prov-backup" {
		t.Errorf("ResolvedProvider = %q, want %q", c.Request.ResolvedProvider, "prov-backup")
	}
	if c.Err != nil {
		t.Errorf("unexpected error in completion: %v", c.Err)
	}
}

func TestFallbackAllFailedStatsRecording(t *testing.T) {
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		mustWriteString(t, w, `{"error":{"message":"primary fail"}}`)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		mustWriteString(t, w, `{"error":{"message":"backup fail"}}`)
	}))
	defer fallbackServer.Close()

	p1 := provider.Provider{
		ID:       "prov-p1",
		Name:     "prov-p1",
		BaseURL:  primaryServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-primary",
				UpstreamModel: "m1",
				Fallbacks:     []string{"chat-backup"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-backup",
		Name:     "prov-backup",
		BaseURL:  fallbackServer.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "chat-backup",
				UpstreamModel: "m2",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	recorder := &testCompletionRecorder{}
	d := New(resolver, &relay.Options{Recorder: recorder})
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "chat-backup" {
		t.Fatalf("X-Gateway-Fallback = %q, want %q", fb, "chat-backup")
	}

	completions := recorder.getCompletions()
	if len(completions) != 1 {
		t.Fatalf("expected 1 completion recorded, got %d", len(completions))
	}
	c := completions[0]
	if c.Request.ClientModel != "chat-backup" {
		t.Errorf("ClientModel = %q, want %q", c.Request.ClientModel, "chat-backup")
	}
	if c.Err == nil {
		t.Errorf("expected completion error for failed attempt")
	}
}

func TestFallbackMultiHopDispatch(t *testing.T) {
	var aCalls, bCalls, cCalls atomic.Int32

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		mustWriteString(t, w, `{"error":{"message":"a failed"}}`)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		mustWriteString(t, w, `{"error":{"message":"b failed"}}`)
	}))
	defer serverB.Close()

	serverC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"choices":[{"message":{"content":"c recovered"}}]}`)
	}))
	defer serverC.Close()

	p1 := provider.Provider{
		ID:       "prov-a",
		Name:     "prov-a",
		BaseURL:  serverA.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "model-a",
				UpstreamModel: "ma",
				Fallbacks:     []string{"model-b"},
			},
		},
	}
	p2 := provider.Provider{
		ID:       "prov-b",
		Name:     "prov-b",
		BaseURL:  serverB.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Extensions: map[string]json.RawMessage{
			"retry": json.RawMessage(`{"max_attempts":1}`),
		},
		Models: []provider.ModelMapping{
			{
				Alias:         "model-b",
				UpstreamModel: "mb",
				Fallbacks:     []string{"model-c"},
			},
		},
	}
	p3 := provider.Provider{
		ID:       "prov-c",
		Name:     "prov-c",
		BaseURL:  serverC.URL + "/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     provider.Auth{Scheme: provider.AuthNone},
		Enabled:  true,
		Models: []provider.ModelMapping{
			{
				Alias:         "model-c",
				UpstreamModel: "mc",
			},
		},
	}

	resolver, err := routing.NewRegistryResolver([]provider.Provider{p1, p2, p3}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver: %v", err)
	}

	recorder := &testCompletionRecorder{}
	d := New(resolver, &relay.Options{Recorder: recorder})
	engine := gin.New()
	engine.POST("/v1/chat/completions", d.Handle)

	body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if aCalls.Load() != 1 {
		t.Errorf("aCalls = %d, want 1", aCalls.Load())
	}
	if bCalls.Load() != 1 {
		t.Errorf("bCalls = %d, want 1", bCalls.Load())
	}
	if cCalls.Load() != 1 {
		t.Errorf("cCalls = %d, want 1", cCalls.Load())
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if fb := w.Header().Get("X-Gateway-Fallback"); fb != "model-c" {
		t.Errorf("X-Gateway-Fallback = %q, want %q", fb, "model-c")
	}

	completions := recorder.getCompletions()
	if len(completions) != 1 {
		t.Fatalf("expected 1 completion recorded, got %d", len(completions))
	}
	c := completions[0]
	if c.Request.ClientModel != "model-c" {
		t.Errorf("ClientModel = %q, want %q", c.Request.ClientModel, "model-c")
	}
	if c.Request.ResolvedProvider != "prov-c" {
		t.Errorf("ResolvedProvider = %q, want %q", c.Request.ResolvedProvider, "prov-c")
	}
}
