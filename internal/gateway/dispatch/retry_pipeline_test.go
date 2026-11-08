package dispatch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/reliability"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func TestTranslationRetryPipeline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		modelName     = "trans-retry-model"
		providerID    = "trans-retry-provider"
		clientPayload = `{"model":"trans-retry-model","messages":[{"role":"user","content":"hello retry"}]}`
		anthropicOK   = `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"hello from anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	)

	initialBackoff := time.Millisecond
	disableJitter := true
	reliability.RegisterProviderRetry(providerID, reliability.ProviderRetryConfig{
		InitialBackoff: &initialBackoff,
		DisableJitter:  &disableJitter,
	})
	defer reliability.ClearProviderRetries()

	setupDispatcher := func(t *testing.T, upstreamURL string, recorder core.Recorder) *gin.Engine {
		t.Helper()
		prov := &provider.Provider{
			ID:       providerID,
			Name:     "Translation Retry Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstreamURL,
			Auth:     provider.Auth{Scheme: provider.AuthAPIKey, Key: "secret-key"},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				modelName: {
					Kind:             routing.Custom,
					Provider:         prov,
					UpstreamModel:    "claude-3",
					UpstreamProtocol: ir.ProtocolAnthropicMessages,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)
		return engine
	}

	t.Run("Transient upstream 502, 503, 429 retry and succeed", func(t *testing.T) {
		for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				upstream := gatewaytest.New(t)
				upstream.Script(gatewaytest.RouteMessages,
					gatewaytest.Step{Status: status, Body: `{"error":{"message":"temporary error"}}`},
					gatewaytest.Step{Status: http.StatusOK, Body: anthropicOK},
				)

				recorder := newMatrixRecorder()
				engine := setupDispatcher(t, upstream.URL(), recorder)

				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(clientPayload))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				engine.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
				}

				var chatResp struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "hello from anthropic" {
					t.Fatalf("unexpected translated response: %s", w.Body.String())
				}

				requests := upstream.RequestsFor(gatewaytest.RouteMessages)
				if len(requests) != 2 {
					t.Fatalf("expected 2 requests to upstream, got %d", len(requests))
				}
			})
		}
	})

	t.Run("Transient failure retries and succeeds for streaming translation", func(t *testing.T) {
		const (
			streamPayload = `{"model":"trans-retry-model","messages":[{"role":"user","content":"hello stream"}],"stream":true}`
			sseEvents     = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed hello\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		)

		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages,
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":{"message":"stream 502"}}`},
			gatewaytest.Step{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "text/event-stream"},
				Body:    sseEvents,
			},
		)

		recorder := newMatrixRecorder()
		engine := setupDispatcher(t, upstream.URL(), recorder)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(streamPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "streamed hello") {
			t.Fatalf("expected stream body to contain text delta, got: %s", w.Body.String())
		}

		requests := upstream.RequestsFor(gatewaytest.RouteMessages)
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests to upstream, got %d", len(requests))
		}
	})

	t.Run("Body replay across attempts in translation", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages,
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":{"message":"temporary error"}}`},
			gatewaytest.Step{Status: http.StatusOK, Body: anthropicOK},
		)

		recorder := newMatrixRecorder()
		engine := setupDispatcher(t, upstream.URL(), recorder)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(clientPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}

		requests := upstream.RequestsFor(gatewaytest.RouteMessages)
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests, got %d", len(requests))
		}

		var req1, req2 map[string]json.RawMessage
		if err := json.Unmarshal([]byte(requests[0].Body), &req1); err != nil {
			t.Fatalf("unmarshal request 1: %v", err)
		}
		if err := json.Unmarshal([]byte(requests[1].Body), &req2); err != nil {
			t.Fatalf("unmarshal request 2: %v", err)
		}
		if string(req1["messages"]) != string(req2["messages"]) {
			t.Fatalf("messages mismatch across attempts: %s vs %s", requests[0].Body, requests[1].Body)
		}
	})

	t.Run("Respects client context cancellation in translation", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // Cancel before execution

		upstream.Script(gatewaytest.RouteMessages,
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"unreached"}`},
		)

		recorder := newMatrixRecorder()
		engine := setupDispatcher(t, upstream.URL(), recorder)

		req := httptest.NewRequestWithContext(ctx, http.MethodPost, gatewaytest.RouteChat, strings.NewReader(clientPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		engine.ServeHTTP(w, req)

		requests := upstream.RequestsFor(gatewaytest.RouteMessages)
		if len(requests) != 0 {
			t.Fatalf("expected 0 requests due to canceled context, got %d", len(requests))
		}
	})

	t.Run("Retry-After exceeding MaxRetryAfter returns immediately without retry in translation", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages,
			gatewaytest.Step{
				Status:     http.StatusTooManyRequests,
				RetryAfter: "60",
				Body:       `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
			},
		)

		recorder := newMatrixRecorder()
		engine := setupDispatcher(t, upstream.URL(), recorder)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(clientPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		engine.ServeHTTP(w, req)

		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("expected status 429, got %d, body: %s", w.Code, w.Body.String())
		}

		requests := upstream.RequestsFor(gatewaytest.RouteMessages)
		if len(requests) != 1 {
			t.Fatalf("expected exactly 1 request (no retry), got %d", len(requests))
		}
	})
}
