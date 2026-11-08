package dispatch_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func TestReasoningConfigMappingMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name                 string
		inboundPath          string
		upstreamProto        ir.Protocol
		upstreamPath         string
		clientPayload        string
		cannedResponse       string
		checkUpstreamRequest func(t *testing.T, body string)
	}{
		// Anthropic budget 1024 -> Chat reasoning_effort low
		{
			name:          "anthropic-to-chat-budget-1024-to-low",
			inboundPath:   gatewaytest.RouteMessages,
			upstreamProto: ir.ProtocolOpenAIChat,
			upstreamPath:  gatewaytest.RouteChat,
			clientPayload: `{
				"model": "client-test-model",
				"max_tokens": 100,
				"thinking": {"type": "enabled", "budget_tokens": 1024},
				"messages": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "chatcmpl_1",
				"object": "chat.completion",
				"model": "upstream-model",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "done"}, "finish_reason": "stop"}]
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				if req["reasoning_effort"] != "low" {
					t.Errorf("reasoning_effort = %v, want 'low'", req["reasoning_effort"])
				}
			},
		},
		// Anthropic budget 16384 -> Responses effort high
		{
			name:          "anthropic-to-responses-budget-16384-to-high",
			inboundPath:   gatewaytest.RouteMessages,
			upstreamProto: ir.ProtocolOpenAIResponses,
			upstreamPath:  gatewaytest.RouteResponses,
			clientPayload: `{
				"model": "client-test-model",
				"max_tokens": 100,
				"thinking": {"type": "enabled", "budget_tokens": 16384},
				"messages": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "resp_1",
				"object": "response",
				"status": "completed",
				"model": "upstream-model",
				"output": [{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant", "content": [{"type": "output_text", "text": "done"}]}]
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				reasoning, ok := req["reasoning"].(map[string]any)
				if !ok || reasoning["effort"] != "high" {
					t.Errorf("reasoning = %v, want effort 'high'", req["reasoning"])
				}
			},
		},
		// Chat reasoning_effort high -> Anthropic budget 16384
		{
			name:          "chat-to-anthropic-effort-high-to-budget",
			inboundPath:   gatewaytest.RouteChat,
			upstreamProto: ir.ProtocolAnthropicMessages,
			upstreamPath:  gatewaytest.RouteMessages,
			clientPayload: `{
				"model": "client-test-model",
				"reasoning_effort": "high",
				"messages": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "msg_1",
				"type": "message",
				"role": "assistant",
				"model": "upstream-model",
				"content": [{"type": "text", "text": "done"}],
				"stop_reason": "end_turn"
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				thinking, ok := req["thinking"].(map[string]any)
				if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
					t.Errorf("thinking = %v, want enabled with budget_tokens 16384", req["thinking"])
				}
			},
		},
		// Chat reasoning_effort medium -> Responses effort medium
		{
			name:          "chat-to-responses-effort-medium",
			inboundPath:   gatewaytest.RouteChat,
			upstreamProto: ir.ProtocolOpenAIResponses,
			upstreamPath:  gatewaytest.RouteResponses,
			clientPayload: `{
				"model": "client-test-model",
				"reasoning_effort": "medium",
				"messages": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "resp_1",
				"object": "response",
				"status": "completed",
				"model": "upstream-model",
				"output": [{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant", "content": [{"type": "output_text", "text": "done"}]}]
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				reasoning, ok := req["reasoning"].(map[string]any)
				if !ok || reasoning["effort"] != "medium" {
					t.Errorf("reasoning = %v, want effort 'medium'", req["reasoning"])
				}
			},
		},
		// Responses effort low -> Anthropic budget 1024
		{
			name:          "responses-to-anthropic-effort-low-to-budget",
			inboundPath:   gatewaytest.RouteResponses,
			upstreamProto: ir.ProtocolAnthropicMessages,
			upstreamPath:  gatewaytest.RouteMessages,
			clientPayload: `{
				"model": "client-test-model",
				"reasoning": {"effort": "low"},
				"input": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "msg_1",
				"type": "message",
				"role": "assistant",
				"model": "upstream-model",
				"content": [{"type": "text", "text": "done"}],
				"stop_reason": "end_turn"
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				thinking, ok := req["thinking"].(map[string]any)
				if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(1024) {
					t.Errorf("thinking = %v, want enabled with budget_tokens 1024", req["thinking"])
				}
			},
		},
		// Responses effort high -> Chat reasoning_effort high
		{
			name:          "responses-to-chat-effort-high",
			inboundPath:   gatewaytest.RouteResponses,
			upstreamProto: ir.ProtocolOpenAIChat,
			upstreamPath:  gatewaytest.RouteChat,
			clientPayload: `{
				"model": "client-test-model",
				"reasoning": {"effort": "high"},
				"input": [{"role": "user", "content": "solve"}]
			}`,
			cannedResponse: `{
				"id": "chatcmpl_1",
				"object": "chat.completion",
				"model": "upstream-model",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "done"}, "finish_reason": "stop"}]
			}`,
			checkUpstreamRequest: func(t *testing.T, body string) {
				var req map[string]any
				if err := json.Unmarshal([]byte(body), &req); err != nil {
					t.Fatalf("unmarshal upstream request: %v", err)
				}
				if req["reasoning_effort"] != "high" {
					t.Errorf("reasoning_effort = %v, want 'high'", req["reasoning_effort"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(tt.upstreamPath, gatewaytest.Step{
				Status: http.StatusOK,
				Body:   tt.cannedResponse,
			})

			prov := provider.Provider{
				ID:       "test-provider",
				Name:     "Test Provider",
				Protocol: tt.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-test-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: tt.upstreamProto,
					},
				},
			}

			recorder := newMatrixRecorder()
			dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.inboundPath, strings.NewReader(tt.clientPayload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}

			reqs := upstream.Requests()
			if len(reqs) != 1 {
				t.Fatalf("expected 1 upstream request, got %d", len(reqs))
			}
			tt.checkUpstreamRequest(t, reqs[0].Body)
		})
	}
}

func TestReasoningMultiTurnSignaturePreservationAndStripping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	anthropicMultiTurnWithSig := `{
		"model": "client-test-model",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "compute 2+2"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "Let me add 2 and 2", "signature": "secret-sig-42"},
				{"type": "text", "text": "4"}
			]},
			{"role": "user", "content": "add 1"}
		]
	}`

	// 1. Anthropic -> Anthropic: signature MUST be preserved!
	t.Run("anthropic-to-anthropic-preserves-signature", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "msg_res",
				"type": "message",
				"role": "assistant",
				"model": "upstream-model",
				"content": [{"type": "text", "text": "5"}],
				"stop_reason": "end_turn"
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolAnthropicMessages},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(anthropicMultiTurnWithSig))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		var wireReq struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &wireReq); err != nil {
			t.Fatal(err)
		}
		asst := wireReq.Messages[1]
		var blocks []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(asst.Content, &blocks); err != nil {
			t.Fatalf("unmarshal assistant blocks: %v", err)
		}
		if len(blocks) != 2 || blocks[0].Type != "thinking" {
			t.Fatalf("unexpected assistant content blocks: %+v", blocks)
		}
		if blocks[0].Signature != "secret-sig-42" {
			t.Errorf("signature = %q, want 'secret-sig-42' (should be preserved for same-protocol multi-turn)", blocks[0].Signature)
		}
	})

	// 2. Anthropic -> OpenAIChat: signature MUST be stripped!
	t.Run("anthropic-to-chat-strips-signature", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteChat, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "chatcmpl_res",
				"object": "chat.completion",
				"model": "upstream-model",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "5"}, "finish_reason": "stop"}]
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolOpenAIChat,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIChat},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(anthropicMultiTurnWithSig))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		var wireReq struct {
			Messages []struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &wireReq); err != nil {
			t.Fatal(err)
		}
		asst := wireReq.Messages[1]
		if asst.ReasoningContent != "Let me add 2 and 2" {
			t.Errorf("reasoning_content = %q, want 'Let me add 2 and 2'", asst.ReasoningContent)
		}
		if strings.Contains(reqs[0].Body, "secret-sig-42") {
			t.Errorf("signature leaked to chat upstream: %s", reqs[0].Body)
		}
	})

	// 3. Anthropic -> OpenAIResponses: signature MUST be stripped!
	t.Run("anthropic-to-responses-strips-signature", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteResponses, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "resp_res",
				"object": "response",
				"status": "completed",
				"model": "upstream-model",
				"output": [{"id": "msg_0", "type": "message", "status": "completed", "role": "assistant", "content": [{"type": "output_text", "text": "5"}]}]
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolOpenAIResponses,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIResponses},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(anthropicMultiTurnWithSig))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		if strings.Contains(reqs[0].Body, "secret-sig-42") {
			t.Errorf("signature leaked to responses upstream: %s", reqs[0].Body)
		}
		var wireReq struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &wireReq); err != nil {
			t.Fatal(err)
		}
		foundReasoning := false
		for _, item := range wireReq.Input {
			if item["type"] == "reasoning" {
				foundReasoning = true
			}
		}
		if !foundReasoning {
			t.Errorf("expected reasoning item in responses input: %+v", wireReq.Input)
		}
	})
}

func TestReasoningResponseSurfacingAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Upstream Chat with reasoning_content -> Client Anthropic
	t.Run("chat-reasoning-to-anthropic-client", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteChat, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "chatcmpl_r",
				"object": "chat.completion",
				"model": "upstream-model",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"reasoning_content": "Deep thought here",
						"content": "Final answer"
					},
					"finish_reason": "stop"
				}]
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolOpenAIChat,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIChat},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"client-test-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Content []struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
				Text     string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Content) != 2 {
			t.Fatalf("expected 2 content blocks (thinking + text), got %d: %+v", len(res.Content), res.Content)
		}
		if res.Content[0].Type != "thinking" || res.Content[0].Thinking != "Deep thought here" {
			t.Errorf("thinking block = %+v, want type 'thinking' and content 'Deep thought here'", res.Content[0])
		}
		if res.Content[1].Type != "text" || res.Content[1].Text != "Final answer" {
			t.Errorf("text block = %+v, want type 'text' and content 'Final answer'", res.Content[1])
		}
	})

	// Upstream Anthropic with thinking -> Client Chat
	t.Run("anthropic-thinking-to-chat-client", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "msg_r",
				"type": "message",
				"role": "assistant",
				"model": "upstream-model",
				"content": [
					{"type": "thinking", "thinking": "Pondering the solution", "signature": "sig-123"},
					{"type": "text", "text": "42"}
				],
				"stop_reason": "end_turn"
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolAnthropicMessages},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-test-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Choices) == 0 {
			t.Fatal("expected at least 1 choice")
		}
		msg := res.Choices[0].Message
		if msg.ReasoningContent != "Pondering the solution" {
			t.Errorf("reasoning_content = %q, want 'Pondering the solution'", msg.ReasoningContent)
		}
		if msg.Content != "42" {
			t.Errorf("content = %q, want '42'", msg.Content)
		}
	})

	// Upstream Responses with reasoning -> Client Chat
	t.Run("responses-reasoning-to-chat-client", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteResponses, gatewaytest.Step{
			Status: http.StatusOK,
			Body: `{
				"id": "resp_r",
				"object": "response",
				"status": "completed",
				"model": "upstream-model",
				"output": [
					{
						"id": "rs_1",
						"type": "reasoning",
						"summary": [{"type": "summary_text", "text": "Analyzing query"}]
					},
					{
						"id": "msg_1",
						"type": "message",
						"status": "completed",
						"role": "assistant",
						"content": [{"type": "output_text", "text": "Results"}]
					}
				]
			}`,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolOpenAIResponses,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIResponses},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-test-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Choices) == 0 {
			t.Fatal("expected at least 1 choice")
		}
		msg := res.Choices[0].Message
		if msg.ReasoningContent != "Analyzing query" {
			t.Errorf("reasoning_content = %q, want 'Analyzing query'", msg.ReasoningContent)
		}
		if msg.Content != "Results" {
			t.Errorf("content = %q, want 'Results'", msg.Content)
		}
	})
}

func TestReasoningDroppedWhenUnsupportedReporting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := gatewaytest.New(t)
	upstream.Script(gatewaytest.RouteChat, gatewaytest.Step{
		Status: http.StatusOK,
		Body: `{
			"id": "chatcmpl_dropped",
			"object": "chat.completion",
			"model": "upstream-model",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "Done"}, "finish_reason": "stop"}]
		}`,
	})

	prov := provider.Provider{
		ID:       "test-provider",
		Name:     "Test Provider",
		Protocol: ir.ProtocolOpenAIChat,
		BaseURL:  upstream.URL(),
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
	}
	resolver := staticResolver{
		routes: map[string]routing.Route{
			"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIChat},
		},
	}
	dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
	dispatcher.Registry = setupMatrixRegistry()

	falseVal := false
	dispatcher.FeaturePolicy = policy.Config{
		ProviderOverrides: map[string]policy.CapabilityOverrides{
			"test-provider": {Reasoning: &falseVal},
		},
	}

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteMessages, strings.NewReader(`{
		"model": "client-test-model",
		"max_tokens": 100,
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	droppedHeader := w.Header().Get("X-Gateway-Dropped-Params")
	if !strings.Contains(droppedHeader, "reasoning") {
		t.Errorf("expected X-Gateway-Dropped-Params to contain 'reasoning', got %q", droppedHeader)
	}

	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	if strings.Contains(reqs[0].Body, "reasoning_effort") {
		t.Errorf("reasoning_effort should be dropped, but found in upstream body: %s", reqs[0].Body)
	}
}

func TestReasoningStreamingCrossProtocolMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	anthropicSSEWithThinking := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Plan "}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"carefully"}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-sig-123"}}` + "\n\n",
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n",
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":1}` + "\n\n",
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n",
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
	}

	chatSSEWithReasoning := []string{
		"data: " + `{"id":"chat_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chat_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"reasoning_content":"Thinking "},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chat_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"reasoning_content":"step"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chat_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"Answer"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chat_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	// 1. Anthropic stream with thinking -> Chat client (verifies reasoning_content streamed, signature dropped)
	t.Run("anthropic-stream-to-chat-client", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Events: anthropicSSEWithThinking,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolAnthropicMessages},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"client-test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `"reasoning_content":"Plan "`) || !strings.Contains(body, `"reasoning_content":"carefully"`) {
			t.Errorf("chat client missing streamed reasoning deltas:\n%s", body)
		}
		if !strings.Contains(body, `"content":"Hello"`) {
			t.Errorf("chat client missing content delta:\n%s", body)
		}
		if strings.Contains(body, "opaque-sig-123") {
			t.Errorf("signature should not leak to chat client:\n%s", body)
		}
	})

	// 2. Chat stream with reasoning -> Anthropic client (verifies thinking_delta emitted)
	t.Run("chat-stream-to-anthropic-client", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteChat, gatewaytest.Step{
			Status: http.StatusOK,
			Events: chatSSEWithReasoning,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolOpenAIChat,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolOpenAIChat},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteMessages, strings.NewReader(`{"model":"client-test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `"type":"thinking_delta"`) || !strings.Contains(body, `"thinking":"Thinking "`) {
			t.Errorf("anthropic client missing thinking deltas:\n%s", body)
		}
		if !strings.Contains(body, `"type":"text_delta"`) || !strings.Contains(body, `"text":"Answer"`) {
			t.Errorf("anthropic client missing text delta:\n%s", body)
		}
	})

	// 3. Anthropic stream -> Anthropic client (verifies signature_delta is preserved)
	t.Run("anthropic-stream-to-anthropic-client-preserves-signature", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Events: anthropicSSEWithThinking,
		})

		prov := provider.Provider{
			ID:       "test-provider",
			Name:     "Test Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
		}
		resolver := staticResolver{
			routes: map[string]routing.Route{
				"client-test-model": {Kind: routing.Custom, Provider: &prov, UpstreamModel: "upstream-model", UpstreamProtocol: ir.ProtocolAnthropicMessages},
			},
		}
		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()

		engine := gin.New()
		engine.Use(dispatcher.Handle)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteMessages, strings.NewReader(`{"model":"client-test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `"type":"signature_delta"`) || !strings.Contains(body, `"signature":"opaque-sig-123"`) {
			t.Errorf("same-protocol anthropic stream missing signature delta:\n%s", body)
		}
	})
}
