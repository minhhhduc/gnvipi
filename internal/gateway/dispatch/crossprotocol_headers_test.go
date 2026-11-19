package dispatch_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

// crossProtocolTransport drives one translated request and returns the client
// response and the headers the upstream received.
func crossProtocolTransport(t *testing.T, protocol ir.Protocol, inboundPath, clientPayload string, clientHeaders map[string]string, upstreamStep *gatewaytest.Step) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	var route string
	switch protocol {
	case ir.ProtocolOpenAIResponses:
		route = gatewaytest.RouteResponses
	case ir.ProtocolAnthropicMessages:
		route = gatewaytest.RouteMessages
	default:
		route = gatewaytest.RouteChat
	}
	upstream := gatewaytest.New(t)
	upstream.Script(route, *upstreamStep)

	prov := provider.Provider{
		ID:       "issue-provider",
		Name:     "Issue Provider",
		Protocol: protocol,
		BaseURL:  upstream.URL(),
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "provider-secret"},
	}
	if protocol == ir.ProtocolAnthropicMessages {
		prov.Auth = provider.Auth{Scheme: provider.AuthAPIKey, Key: "provider-secret"}
	}
	dispatcher := dispatch.New(staticResolver{routes: map[string]routing.Route{
		"issue-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: protocol},
	}}, &relay.Options{Recorder: newMatrixRecorder()})
	dispatcher.Registry = setupMatrixRegistry()
	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, inboundPath, strings.NewReader(clientPayload))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range clientHeaders {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	return w, reqs[0].Headers
}

// A translated request authenticates as the configured provider and carries no
// client credential, and the client's experimental flags never cross into the
// other protocol family.
func TestCrossProtocolRequestHeaders(t *testing.T) {
	t.Run("messages-to-chat", func(t *testing.T) {
		w, sent := crossProtocolTransport(t, ir.ProtocolOpenAIChat, gatewaytest.RouteMessages,
			`{"model":"issue-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{
				"Anthropic-Version": "2023-06-01",
				"Anthropic-Beta":    "interleaved-thinking-2025-05-14",
				"OpenAI-Beta":       "responses=experimental",
				"Authorization":     "Bearer client-secret",
				"X-Api-Key":         "client-secret",
			},
			&gatewaytest.Step{Body: `{"id":"chatcmpl_1","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`})

		if got := sent.Get("Authorization"); got != "Bearer provider-secret" {
			t.Fatalf("Authorization = %q, want the provider credential", got)
		}
		if got := sent.Get("X-Api-Key"); got != "" {
			t.Fatalf("client API key reached the Chat upstream: %q", got)
		}
		if got := sent.Get("Content-Type"); got != "application/json" {
			t.Fatalf("upstream Content-Type = %q, want application/json", got)
		}
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if got := sent.Get(name); got != "" {
				t.Fatalf("%s leaked into the Chat family: %q", name, got)
			}
		}
		if got := sent.Get("OpenAI-Beta"); got != "responses=experimental" {
			t.Fatalf("OpenAI-Beta = %q, want the same-family value", got)
		}
		if got := w.Header().Get(core.RequestIDHeader); got == "" {
			t.Fatal("client response has no X-Request-Id")
		}
	})

	t.Run("chat-to-messages", func(t *testing.T) {
		w, sent := crossProtocolTransport(t, ir.ProtocolAnthropicMessages, gatewaytest.RouteChat,
			`{"model":"issue-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{
				"Authorization":  "Bearer client-secret",
				"X-Api-Key":      "client-secret",
				"Anthropic-Beta": "tools-2024-01-01",
				"OpenAI-Beta":    "responses=experimental",
			},
			&gatewaytest.Step{Body: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`})

		if got := sent.Get("X-Api-Key"); got != "provider-secret" {
			t.Fatalf("X-Api-Key = %q, want the provider credential", got)
		}
		if got := sent.Get("Authorization"); got != "" {
			t.Fatalf("client Authorization reached the Messages upstream: %q", got)
		}
		if got := sent.Get("Content-Type"); got != "application/json" {
			t.Fatalf("upstream Content-Type = %q, want application/json", got)
		}
		if got := sent.Get("OpenAI-Beta"); got != "" {
			t.Fatalf("OpenAI-Beta leaked into the Messages family: %q", got)
		}
		if got := sent.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("Anthropic-Version = %q, want the protocol default", got)
		}
		if got := sent.Get("Anthropic-Beta"); got != "tools-2024-01-01" {
			t.Fatalf("Anthropic-Beta = %q, want the same-family value", got)
		}
		if got := w.Header().Get(core.RequestIDHeader); got == "" {
			t.Fatal("client response has no X-Request-Id")
		}
	})

	t.Run("chat-to-responses", func(t *testing.T) {
		_, sent := crossProtocolTransport(t, ir.ProtocolOpenAIResponses, gatewaytest.RouteChat,
			`{"model":"issue-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{
				"Anthropic-Version": "2023-06-01",
				"Anthropic-Beta":    "interleaved-thinking-2025-05-14",
				"OpenAI-Beta":       "responses=experimental",
				"Authorization":     "Bearer client-secret",
				"X-Api-Key":         "client-secret",
			},
			&gatewaytest.Step{Body: `{"id":"resp_1","object":"response","model":"upstream-model","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`})

		if got := sent.Get("Authorization"); got != "Bearer provider-secret" {
			t.Fatalf("Authorization = %q, want the provider credential", got)
		}
		if got := sent.Get("X-Api-Key"); got != "" {
			t.Fatalf("client API key reached the Responses upstream: %q", got)
		}
		if got := sent.Get("Content-Type"); got != "application/json" {
			t.Fatalf("upstream Content-Type = %q, want application/json", got)
		}
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if got := sent.Get(name); got != "" {
				t.Fatalf("%s leaked into the Responses family: %q", name, got)
			}
		}
		if got := sent.Get("OpenAI-Beta"); got != "responses=experimental" {
			t.Fatalf("OpenAI-Beta = %q, want the same-family value", got)
		}
	})
}

// An upstream response may only expose its own family's rate-limit vocabulary,
// and never its own trace identity: the gateway stamps X-Request-Id itself, so
// an upstream X-Request-Id or Anthropic request-id must not survive the relay.
func TestCrossProtocolResponseHeaders(t *testing.T) {
	upstreamHeaders := map[string]string{
		"Request-Id":                   "upstream-anthropic-trace",
		"X-Request-Id":                 "upstream-openai-trace",
		"X-Ratelimit-Limit-Requests":   "99",
		"Anthropic-Ratelimit-Requests": "77",
	}

	t.Run("chat-client-keeps-chat-vocabulary", func(t *testing.T) {
		w, _ := crossProtocolTransport(t, ir.ProtocolOpenAIChat, gatewaytest.RouteChat,
			`{"model":"issue-model","messages":[{"role":"user","content":"hi"}]}`, nil,
			&gatewaytest.Step{Headers: upstreamHeaders, Body: chatCompletionBody})

		if got := w.Header().Get("X-Ratelimit-Limit-Requests"); got != "99" {
			t.Fatalf("same-family rate limit lost: %q", got)
		}
		for _, name := range []string{"Anthropic-Ratelimit-Requests", "Request-Id"} {
			if got := w.Header().Get(name); got != "" {
				t.Fatalf("%s reached a Chat client: %q", name, got)
			}
		}
		if got := w.Header().Get(core.RequestIDHeader); got == "" || got == "upstream-openai-trace" {
			t.Fatalf("X-Request-Id = %q, want the gateway identity", got)
		}
	})

	t.Run("messages-client-keeps-anthropic-vocabulary", func(t *testing.T) {
		w, _ := crossProtocolTransport(t, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages,
			`{"model":"issue-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil,
			&gatewaytest.Step{Headers: upstreamHeaders, Body: messagesBody})

		if got := w.Header().Get("Anthropic-Ratelimit-Requests"); got != "77" {
			t.Fatalf("same-family rate limit lost: %q", got)
		}
		for _, name := range []string{"X-Ratelimit-Limit-Requests", "Request-Id"} {
			if got := w.Header().Get(name); got != "" {
				t.Fatalf("%s reached a Messages client: %q", name, got)
			}
		}
		if got := w.Header().Get(core.RequestIDHeader); got == "" || got == "upstream-openai-trace" {
			t.Fatalf("X-Request-Id = %q, want the gateway identity", got)
		}
	})

	// A translated response is re-encoded, so no upstream header survives at
	// all; the client sees the gateway trace id and nothing of the other family.
	t.Run("translated-response-drops-upstream-headers", func(t *testing.T) {
		w, _ := crossProtocolTransport(t, ir.ProtocolOpenAIChat, gatewaytest.RouteMessages,
			`{"model":"issue-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"Anthropic-Version": "2023-06-01"},
			&gatewaytest.Step{Headers: upstreamHeaders, Body: chatCompletionBody})

		for _, name := range []string{"X-Ratelimit-Limit-Requests", "Anthropic-Ratelimit-Requests", "Request-Id"} {
			if got := w.Header().Get(name); got != "" {
				t.Fatalf("%s leaked from a Chat upstream to a Messages client: %q", name, got)
			}
		}
		if got := w.Header().Get(core.RequestIDHeader); got == "" || got == "upstream-openai-trace" {
			t.Fatalf("X-Request-Id = %q, want the gateway identity", got)
		}
	})
}

const chatCompletionBody = `{"id":"chatcmpl_1","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

const messagesBody = `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
