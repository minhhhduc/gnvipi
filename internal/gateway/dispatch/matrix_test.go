package dispatch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	inboundchat "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openaichat"
	inboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	outboundchat "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
	outboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

type matrixTestRecorder struct {
	completions chan *core.Completion
}

func newMatrixRecorder() *matrixTestRecorder {
	return &matrixTestRecorder{completions: make(chan *core.Completion, 8)}
}

func (r *matrixTestRecorder) Record(c *core.Completion) {
	r.completions <- c
}

type staticResolver struct {
	routes map[string]routing.Route
}

func (s staticResolver) Resolve(_ context.Context, model string, _ ir.Protocol) (routing.Route, error) {
	if r, ok := s.routes[model]; ok {
		return r, nil
	}
	return routing.Route{Kind: routing.Fallthrough}, nil
}

func (s staticResolver) ResolveCandidates(ctx context.Context, model string, p ir.Protocol) ([]routing.Route, error) {
	r, err := s.Resolve(ctx, model, p)
	if err != nil {
		return nil, err
	}
	return []routing.Route{r}, nil
}

func (s staticResolver) AdvanceCycle(string) {}

func setupMatrixRegistry() *adapter.Registry {
	r := adapter.NewRegistry()
	r.RegisterInbound(inboundanthropic.New())
	r.RegisterInbound(inboundchat.New())
	r.RegisterInbound(inboundresponses.New())
	r.RegisterOutbound(outboundanthropic.New())
	r.RegisterOutbound(outboundchat.New())
	r.RegisterOutbound(outboundresponses.New())
	return r
}

type matrixPair struct {
	name                 string
	inboundProto         ir.Protocol
	inboundPath          string
	upstreamProto        ir.Protocol
	upstreamPath         string
	clientPayload        string
	cannedResponse       string
	checkResponse        func(t *testing.T, body string)
	checkUpstreamRequest func(t *testing.T, body string)
}

func TestCrossProtocolNonStreamingMatrix(t *testing.T) {
	chatUpstreamResponse := `{
		"id": "chatcmpl_test",
		"object": "chat.completion",
		"model": "upstream-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Translated response text"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18}
	}`

	anthropicUpstreamResponse := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "upstream-model",
		"content": [{"type": "text", "text": "Translated response text"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 6}
	}`

	responsesUpstreamResponse := `{
		"id": "resp_test",
		"object": "response",
		"status": "completed",
		"model": "upstream-model",
		"output": [{
			"id": "msg_0",
			"type": "message",
			"status": "completed",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "Translated response text"}]
		}],
		"usage": {"input_tokens": 12, "output_tokens": 6, "total_tokens": 18}
	}`

	chatClientPayload := `{
		"model": "client-test-model",
		"messages": [
			{"role": "system", "content": "You are helpful"},
			{"role": "user", "content": "Hello world"}
		],
		"temperature": 0.7,
		"max_tokens": 50
	}`

	anthropicClientPayload := `{
		"model": "client-test-model",
		"system": "You are helpful",
		"messages": [
			{"role": "user", "content": "Hello world"}
		],
		"max_tokens": 50
	}`

	responsesClientPayload := `{
		"model": "client-test-model",
		"instructions": "You are helpful",
		"input": [
			{"role": "user", "content": "Hello world"}
		],
		"max_output_tokens": 50
	}`

	checkChatClientResponse := func(t *testing.T, body string) {
		t.Helper()
		var res struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("unmarshal chat response: %v\n%s", err, body)
		}
		if res.Object != "chat.completion" {
			t.Errorf("object = %q, want chat.completion", res.Object)
		}
		if res.Model != "client-test-model" {
			t.Errorf("model = %q, want client-test-model", res.Model)
		}
		if len(res.Choices) == 0 || res.Choices[0].Message.Content != "Translated response text" {
			t.Errorf("unexpected content: %v", res.Choices)
		}
		if res.Usage.PromptTokens != 12 || res.Usage.CompletionTokens != 6 {
			t.Errorf("unexpected usage: %+v", res.Usage)
		}
	}

	checkAnthropicClientResponse := func(t *testing.T, body string) {
		t.Helper()
		var res struct {
			Type    string `json:"type"`
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("unmarshal anthropic response: %v\n%s", err, body)
		}
		if res.Type != "message" {
			t.Errorf("type = %q, want message", res.Type)
		}
		if res.Model != "client-test-model" {
			t.Errorf("model = %q, want client-test-model", res.Model)
		}
		if len(res.Content) == 0 || res.Content[0].Text != "Translated response text" {
			t.Errorf("unexpected content: %v", res.Content)
		}
		if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 6 {
			t.Errorf("unexpected usage: %+v", res.Usage)
		}
	}

	checkResponsesClientResponse := func(t *testing.T, body string) {
		t.Helper()
		var res struct {
			Object string `json:"object"`
			Model  string `json:"model"`
			Status string `json:"status"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("unmarshal responses response: %v\n%s", err, body)
		}
		if res.Object != "response" {
			t.Errorf("object = %q, want response", res.Object)
		}
		if res.Model != "client-test-model" {
			t.Errorf("model = %q, want client-test-model", res.Model)
		}
		if res.Status != "completed" {
			t.Errorf("status = %q, want completed", res.Status)
		}
		if len(res.Output) == 0 || len(res.Output[0].Content) == 0 || res.Output[0].Content[0].Text != "Translated response text" {
			t.Errorf("unexpected output: %+v", res.Output)
		}
		if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 6 {
			t.Errorf("unexpected usage: %+v", res.Usage)
		}
	}

	checkChatUpstreamReq := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal chat upstream: %v\n%s", err, body)
		}
		if len(req.Messages) < 2 {
			t.Fatalf("expected at least 2 messages, got %+v", req.Messages)
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != "You are helpful" {
			t.Errorf("system message = %+v, want role: system, content: 'You are helpful'", req.Messages[0])
		}
		if req.Messages[1].Role != "user" || req.Messages[1].Content != "Hello world" {
			t.Errorf("user message = %+v, want role: user, content: 'Hello world'", req.Messages[1])
		}
	}

	checkAnthropicUpstreamReq := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			System   any `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal anthropic upstream: %v\n%s", err, body)
		}
		systemBytes, err := json.Marshal(req.System)
		if err != nil {
			t.Fatalf("marshal system: %v", err)
		}
		if !strings.Contains(string(systemBytes), "You are helpful") {
			t.Errorf("system = %s, want to contain 'You are helpful'", string(systemBytes))
		}
		if len(req.Messages) == 0 {
			t.Fatalf("expected at least 1 message, got 0")
		}
		msgBytes, err := json.Marshal(req.Messages[0])
		if err != nil {
			t.Fatalf("marshal message[0]: %v", err)
		}
		if !strings.Contains(string(msgBytes), "Hello world") {
			t.Errorf("expected 'Hello world' in message[0]: %s", string(msgBytes))
		}
	}

	checkResponsesUpstreamReq := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Instructions string `json:"instructions"`
			Input        []any  `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal responses upstream: %v\n%s", err, body)
		}
		if req.Instructions != "You are helpful" {
			t.Errorf("instructions = %q, want 'You are helpful'", req.Instructions)
		}
		if len(req.Input) == 0 {
			t.Fatalf("expected at least 1 input item, got 0")
		}
		inputBytes, err := json.Marshal(req.Input[0])
		if err != nil {
			t.Fatalf("marshal input[0]: %v", err)
		}
		if !strings.Contains(string(inputBytes), "Hello world") {
			t.Errorf("expected 'Hello world' in input[0]: %s", string(inputBytes))
		}
	}

	pairs := []matrixPair{
		{
			name:                 "Chat->Anthropic",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        chatClientPayload,
			cannedResponse:       anthropicUpstreamResponse,
			checkResponse:        checkChatClientResponse,
			checkUpstreamRequest: checkAnthropicUpstreamReq,
		},
		{
			name:                 "Chat->Responses",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        chatClientPayload,
			cannedResponse:       responsesUpstreamResponse,
			checkResponse:        checkChatClientResponse,
			checkUpstreamRequest: checkResponsesUpstreamReq,
		},
		{
			name:                 "Anthropic->Chat",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        anthropicClientPayload,
			cannedResponse:       chatUpstreamResponse,
			checkResponse:        checkAnthropicClientResponse,
			checkUpstreamRequest: checkChatUpstreamReq,
		},
		{
			name:                 "Anthropic->Responses",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        anthropicClientPayload,
			cannedResponse:       responsesUpstreamResponse,
			checkResponse:        checkAnthropicClientResponse,
			checkUpstreamRequest: checkResponsesUpstreamReq,
		},
		{
			name:                 "Responses->Chat",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        responsesClientPayload,
			cannedResponse:       chatUpstreamResponse,
			checkResponse:        checkResponsesClientResponse,
			checkUpstreamRequest: checkChatUpstreamReq,
		},
		{
			name:                 "Responses->Anthropic",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        responsesClientPayload,
			cannedResponse:       anthropicUpstreamResponse,
			checkResponse:        checkResponsesClientResponse,
			checkUpstreamRequest: checkAnthropicUpstreamReq,
		},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(p.upstreamPath, gatewaytest.Step{
				Status: http.StatusOK,
				Body:   p.cannedResponse,
			})

			prov := provider.Provider{
				ID:       "test-provider",
				Name:     "Test Provider",
				Protocol: p.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-test-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: p.upstreamProto,
					},
				},
			}

			recorder := newMatrixRecorder()
			dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, p.inboundPath, strings.NewReader(p.clientPayload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}

			p.checkResponse(t, w.Body.String())

			reqs := upstream.Requests()
			if len(reqs) == 0 {
				t.Fatal("no request received by upstream")
			}
			if p.checkUpstreamRequest != nil {
				p.checkUpstreamRequest(t, reqs[0].Body)
			}

			// Verify usage recorded in completion
			select {
			case completion := <-recorder.completions:
				if completion.Err != nil {
					t.Fatalf("completion error: %v", completion.Err)
				}
				if completion.Usage.InputTokens != 12 || completion.Usage.OutputTokens != 6 {
					t.Errorf("recorded usage = %+v, want input 12, output 6", completion.Usage)
				}
				if completion.Request.ClientModel != "client-test-model" {
					t.Errorf("recorded client model = %q", completion.Request.ClientModel)
				}
			default:
				t.Fatal("no completion recorded")
			}
		})
	}
}

func TestCrossProtocolUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name           string
		inboundPath    string
		clientPayload  string
		upstreamProto  ir.Protocol
		upstreamPath   string
		cannedError    string
		upstreamStatus int
		expectCode     int
		checkEnvelope  func(t *testing.T, body string)
	}{
		{
			name:           "Chat->Anthropic 429",
			inboundPath:    gatewaytest.RouteChat,
			clientPayload:  `{"model":"err-model","messages":[{"role":"user","content":"hi"}]}`,
			upstreamProto:  ir.ProtocolAnthropicMessages,
			upstreamPath:   gatewaytest.RouteMessages,
			cannedError:    `{"type":"error","error":{"type":"rate_limit_error","message":"anthropic rate limit"}}`,
			upstreamStatus: http.StatusTooManyRequests,
			expectCode:     http.StatusTooManyRequests,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "rate_limit") || !strings.Contains(body, "anthropic rate limit") {
					t.Fatalf("unexpected chat error envelope: %s", body)
				}
			},
		},
		{
			name:           "Chat->Responses 400",
			inboundPath:    gatewaytest.RouteChat,
			clientPayload:  `{"model":"err-model","messages":[{"role":"user","content":"hi"}]}`,
			upstreamProto:  ir.ProtocolOpenAIResponses,
			upstreamPath:   gatewaytest.RouteResponses,
			cannedError:    `{"error":{"code":"invalid_request","message":"bad responses request"}}`,
			upstreamStatus: http.StatusBadRequest,
			expectCode:     http.StatusBadRequest,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "invalid_request") || !strings.Contains(body, "bad responses request") {
					t.Fatalf("unexpected chat error envelope: %s", body)
				}
			},
		},
		{
			name:           "Anthropic->Chat 500",
			inboundPath:    gatewaytest.RouteMessages,
			clientPayload:  `{"model":"err-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			upstreamProto:  ir.ProtocolOpenAIChat,
			upstreamPath:   gatewaytest.RouteChat,
			cannedError:    `{"error":{"code":"internal_error","message":"internal chat error"}}`,
			upstreamStatus: http.StatusInternalServerError,
			expectCode:     http.StatusBadGateway,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "internal chat error") {
					t.Fatalf("unexpected anthropic error envelope: %s", body)
				}
			},
		},
		{
			name:           "Anthropic->Responses 400",
			inboundPath:    gatewaytest.RouteMessages,
			clientPayload:  `{"model":"err-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			upstreamProto:  ir.ProtocolOpenAIResponses,
			upstreamPath:   gatewaytest.RouteResponses,
			cannedError:    `{"error":{"code":"invalid_request","message":"bad responses request"}}`,
			upstreamStatus: http.StatusBadRequest,
			expectCode:     http.StatusBadRequest,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "invalid_request") || !strings.Contains(body, "bad responses request") {
					t.Fatalf("unexpected anthropic error envelope: %s", body)
				}
			},
		},
		{
			name:           "Responses->Chat 500",
			inboundPath:    gatewaytest.RouteResponses,
			clientPayload:  `{"model":"err-model","input":"hi"}`,
			upstreamProto:  ir.ProtocolOpenAIChat,
			upstreamPath:   gatewaytest.RouteChat,
			cannedError:    `{"error":{"code":"internal_error","message":"internal chat error"}}`,
			upstreamStatus: http.StatusInternalServerError,
			expectCode:     http.StatusBadGateway,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "internal chat error") {
					t.Fatalf("unexpected responses error envelope: %s", body)
				}
			},
		},
		{
			name:           "Responses->Anthropic 429",
			inboundPath:    gatewaytest.RouteResponses,
			clientPayload:  `{"model":"err-model","input":"hi"}`,
			upstreamProto:  ir.ProtocolAnthropicMessages,
			upstreamPath:   gatewaytest.RouteMessages,
			cannedError:    `{"type":"error","error":{"type":"rate_limit_error","message":"anthropic rate limit"}}`,
			upstreamStatus: http.StatusTooManyRequests,
			expectCode:     http.StatusTooManyRequests,
			checkEnvelope: func(t *testing.T, body string) {
				if !strings.Contains(body, "rate_limit") || !strings.Contains(body, "anthropic rate limit") {
					t.Fatalf("unexpected responses error envelope: %s", body)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(tc.upstreamPath, gatewaytest.Step{
				Status: tc.upstreamStatus,
				Body:   tc.cannedError,
			})

			prov := provider.Provider{
				ID:       "err-provider",
				Name:     "Err Provider",
				Protocol: tc.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"err-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-err",
						UpstreamProtocol: tc.upstreamProto,
					},
				},
			}

			dispatcher := dispatch.New(resolver, &relay.Options{})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.inboundPath, strings.NewReader(tc.clientPayload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != tc.expectCode {
				t.Fatalf("status = %d, want %d, body: %s", w.Code, tc.expectCode, w.Body.String())
			}
			tc.checkEnvelope(t, w.Body.String())
		})
	}
}

func TestCrossProtocolLargeIntegerPrecision(t *testing.T) {
	upstream := gatewaytest.New(t)
	upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
		Status: http.StatusOK,
		Body:   `{"id":"msg_int","type":"message","role":"assistant","model":"claude-int","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`,
	})

	prov := provider.Provider{
		ID:       "int-provider",
		Name:     "Int Provider",
		Protocol: ir.ProtocolAnthropicMessages,
		BaseURL:  upstream.URL(),
	}

	resolver := staticResolver{
		routes: map[string]routing.Route{
			"int-model": {
				Kind:             routing.Custom,
				Provider:         &prov,
				UpstreamModel:    "claude-int",
				UpstreamProtocol: ir.ProtocolAnthropicMessages,
			},
		},
	}

	dispatcher := dispatch.New(resolver, &relay.Options{})
	dispatcher.Registry = setupMatrixRegistry()

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	// Send 900719925474099312345678901 in client JSON
	raw := `{"model":"int-model","messages":[{"role":"user","content":"hi"}],"vendor_id":900719925474099312345678901}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	reqs := upstream.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request received by upstream")
	}
	if !strings.Contains(reqs[0].Body, "900719925474099312345678901") {
		t.Fatalf("large integer was truncated or float-converted in upstream request: %s", reqs[0].Body)
	}
}

func TestCrossProtocolMultiTurnMatrix(t *testing.T) {
	chatMultiTurnPayload := `{
		"model": "client-test-model",
		"messages": [
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "And +1?"}
		]
	}`

	anthropicMultiTurnPayload := `{
		"model": "client-test-model",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "And +1?"}
		]
	}`

	responsesMultiTurnPayload := `{
		"model": "client-test-model",
		"input": [
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "And +1?"}
		]
	}`

	chatUpstreamResponse := `{
		"id": "chatcmpl_multi",
		"object": "chat.completion",
		"model": "upstream-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "5"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 20, "completion_tokens": 2, "total_tokens": 22}
	}`

	anthropicUpstreamResponse := `{
		"id": "msg_multi",
		"type": "message",
		"role": "assistant",
		"model": "upstream-model",
		"content": [{"type": "text", "text": "5"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 20, "output_tokens": 2}
	}`

	responsesUpstreamResponse := `{
		"id": "resp_multi",
		"object": "response",
		"status": "completed",
		"model": "upstream-model",
		"output": [{
			"id": "msg_0",
			"type": "message",
			"status": "completed",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "5"}]
		}],
		"usage": {"input_tokens": 20, "output_tokens": 2, "total_tokens": 22}
	}`

	checkChatUpstreamMultiTurn := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal chat upstream: %v\n%s", err, body)
		}
		if len(req.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d: %+v", len(req.Messages), req.Messages)
		}
		if req.Messages[0].Role != "user" || req.Messages[0].Content != "What is 2+2?" {
			t.Errorf("turn 0 = %+v, want user 'What is 2+2?'", req.Messages[0])
		}
		if req.Messages[1].Role != "assistant" || req.Messages[1].Content != "4" {
			t.Errorf("turn 1 = %+v, want assistant '4'", req.Messages[1])
		}
		if req.Messages[2].Role != "user" || req.Messages[2].Content != "And +1?" {
			t.Errorf("turn 2 = %+v, want user 'And +1?'", req.Messages[2])
		}
	}

	checkAnthropicUpstreamMultiTurn := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal anthropic upstream: %v\n%s", err, body)
		}
		if len(req.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d: %+v", len(req.Messages), req.Messages)
		}
		for i, want := range []struct {
			role    string
			content string
		}{
			{"user", "What is 2+2?"},
			{"assistant", "4"},
			{"user", "And +1?"},
		} {
			if req.Messages[i].Role != want.role {
				t.Errorf("turn %d role = %q, want %q", i, req.Messages[i].Role, want.role)
			}
			raw, err := json.Marshal(req.Messages[i].Content)
			if err != nil {
				t.Fatalf("marshal turn %d content: %v", i, err)
			}
			if !strings.Contains(string(raw), want.content) {
				t.Errorf("turn %d content = %s, want to contain %q", i, string(raw), want.content)
			}
		}
	}

	checkResponsesUpstreamMultiTurn := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Input []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal responses upstream: %v\n%s", err, body)
		}
		if len(req.Input) != 3 {
			t.Fatalf("expected 3 input items, got %d: %+v", len(req.Input), req.Input)
		}
		for i, want := range []struct {
			role    string
			content string
		}{
			{"user", "What is 2+2?"},
			{"assistant", "4"},
			{"user", "And +1?"},
		} {
			if req.Input[i].Role != want.role {
				t.Errorf("turn %d role = %q, want %q", i, req.Input[i].Role, want.role)
			}
			raw, err := json.Marshal(req.Input[i].Content)
			if err != nil {
				t.Fatalf("marshal turn %d input: %v", i, err)
			}
			if !strings.Contains(string(raw), want.content) {
				t.Errorf("turn %d content = %s, want to contain %q", i, string(raw), want.content)
			}
		}
	}

	pairs := []struct {
		name                 string
		inboundPath          string
		upstreamProto        ir.Protocol
		upstreamPath         string
		clientPayload        string
		cannedResponse       string
		checkUpstreamRequest func(t *testing.T, body string)
	}{
		{"Chat->Anthropic MultiTurn", gatewaytest.RouteChat, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, chatMultiTurnPayload, anthropicUpstreamResponse, checkAnthropicUpstreamMultiTurn},
		{"Chat->Responses MultiTurn", gatewaytest.RouteChat, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, chatMultiTurnPayload, responsesUpstreamResponse, checkResponsesUpstreamMultiTurn},
		{"Anthropic->Chat MultiTurn", gatewaytest.RouteMessages, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, anthropicMultiTurnPayload, chatUpstreamResponse, checkChatUpstreamMultiTurn},
		{"Anthropic->Responses MultiTurn", gatewaytest.RouteMessages, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, anthropicMultiTurnPayload, responsesUpstreamResponse, checkResponsesUpstreamMultiTurn},
		{"Responses->Chat MultiTurn", gatewaytest.RouteResponses, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, responsesMultiTurnPayload, chatUpstreamResponse, checkChatUpstreamMultiTurn},
		{"Responses->Anthropic MultiTurn", gatewaytest.RouteResponses, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, responsesMultiTurnPayload, anthropicUpstreamResponse, checkAnthropicUpstreamMultiTurn},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(p.upstreamPath, gatewaytest.Step{
				Status: http.StatusOK,
				Body:   p.cannedResponse,
			})

			prov := provider.Provider{
				ID:       "multi-provider",
				Name:     "Multi Provider",
				Protocol: p.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-test-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: p.upstreamProto,
					},
				},
			}

			dispatcher := dispatch.New(resolver, &relay.Options{})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, p.inboundPath, strings.NewReader(p.clientPayload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}

			reqs := upstream.Requests()
			if len(reqs) == 0 {
				t.Fatal("no upstream request received")
			}
			p.checkUpstreamRequest(t, reqs[0].Body)
		})
	}
}

func TestStructuredOutputLosslessMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chatUpstreamResponse := `{
		"id": "chatcmpl_test",
		"object": "chat.completion",
		"model": "upstream-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "{\"name\":\"Alice\",\"age\":30}"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18}
	}`

	responsesUpstreamResponse := `{
		"id": "resp_test",
		"object": "response",
		"status": "completed",
		"model": "upstream-model",
		"output": [{
			"id": "msg_0",
			"type": "message",
			"status": "completed",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "{\"name\":\"Alice\",\"age\":30}"}]
		}],
		"usage": {"input_tokens": 12, "output_tokens": 6, "total_tokens": 18}
	}`

	t.Run("ChatToResponses_JSONSchema_Lossless", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteResponses, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   responsesUpstreamResponse,
		})

		prov := provider.Provider{
			ID:       "responses-prov",
			Name:     "Responses Provider",
			Protocol: ir.ProtocolOpenAIResponses,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolOpenAIResponses,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		chatPayload := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "Extract person"}],
			"response_format": {
				"type": "json_schema",
				"json_schema": {
					"name": "person",
					"strict": true,
					"schema": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"age": {"type": "integer"}
						},
						"required": ["name", "age"],
						"additionalProperties": false
					}
				}
			}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(chatPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatal("expected upstream request")
		}

		var upstreamBody struct {
			Text struct {
				Format struct {
					Type   string          `json:"type"`
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v\n%s", err, reqs[0].Body)
		}
		if upstreamBody.Text.Format.Type != "json_schema" {
			t.Errorf("type = %q, want json_schema", upstreamBody.Text.Format.Type)
		}
		if upstreamBody.Text.Format.Name != "person" {
			t.Errorf("name = %q, want person", upstreamBody.Text.Format.Name)
		}
		if !upstreamBody.Text.Format.Strict {
			t.Errorf("strict = %v, want true", upstreamBody.Text.Format.Strict)
		}
		if !strings.Contains(string(upstreamBody.Text.Format.Schema), `"additionalProperties":false`) {
			t.Errorf("schema = %s, want additionalProperties: false", upstreamBody.Text.Format.Schema)
		}
	})

	t.Run("ChatToResponses_JSONObject_Lossless", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteResponses, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   responsesUpstreamResponse,
		})

		prov := provider.Provider{
			ID:       "responses-prov",
			Name:     "Responses Provider",
			Protocol: ir.ProtocolOpenAIResponses,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolOpenAIResponses,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		chatPayload := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "Extract person"}],
			"response_format": {"type": "json_object"}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(chatPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatal("expected upstream request")
		}

		var upstreamBody struct {
			Text struct {
				Format struct {
					Type string `json:"type"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v\n%s", err, reqs[0].Body)
		}
		if upstreamBody.Text.Format.Type != "json_object" {
			t.Errorf("type = %q, want json_object", upstreamBody.Text.Format.Type)
		}
	})

	t.Run("ResponsesToChat_JSONSchema_Lossless", func(t *testing.T) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteChat, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   chatUpstreamResponse,
		})

		prov := provider.Provider{
			ID:       "chat-prov",
			Name:     "Chat Provider",
			Protocol: ir.ProtocolOpenAIChat,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolOpenAIChat,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		responsesPayload := `{
			"model": "test-model",
			"input": [{"role": "user", "content": "Extract person"}],
			"text": {
				"format": {
					"type": "json_schema",
					"name": "person",
					"strict": true,
					"schema": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"age": {"type": "integer"}
						},
						"required": ["name", "age"]
					}
				}
			}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteResponses, strings.NewReader(responsesPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatal("expected upstream request")
		}

		var upstreamBody struct {
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v\n%s", err, reqs[0].Body)
		}
		if upstreamBody.ResponseFormat.Type != "json_schema" {
			t.Errorf("type = %q, want json_schema", upstreamBody.ResponseFormat.Type)
		}
		if upstreamBody.ResponseFormat.JSONSchema.Name != "person" {
			t.Errorf("name = %q, want person", upstreamBody.ResponseFormat.JSONSchema.Name)
		}
		if !upstreamBody.ResponseFormat.JSONSchema.Strict {
			t.Errorf("strict = %v, want true", upstreamBody.ResponseFormat.JSONSchema.Strict)
		}
		if !strings.Contains(string(upstreamBody.ResponseFormat.JSONSchema.Schema), `"required":["name","age"]`) {
			t.Errorf("schema = %s, want required name, age", upstreamBody.ResponseFormat.JSONSchema.Schema)
		}
	})

	t.Run("AnthropicOutbound_ToolEmulation", func(t *testing.T) {
		anthropicUpstreamResponse := `{
			"id": "msg_emul_test",
			"type": "message",
			"role": "assistant",
			"model": "upstream-model",
			"content": [{
				"type": "tool_use",
				"id": "call_123",
				"name": "person",
				"input": {"name": "Alice", "age": 30}
			}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 20, "output_tokens": 10}
		}`

		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   anthropicUpstreamResponse,
		})

		prov := provider.Provider{
			ID:       "anthropic-emulated",
			Name:     "Anthropic Emulated",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
			Extensions: map[string]json.RawMessage{
				"tool_emulation": json.RawMessage(`true`),
			},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolAnthropicMessages,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		chatPayload := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "Extract person"}],
			"response_format": {
				"type": "json_schema",
				"json_schema": {
					"name": "person",
					"strict": true,
					"schema": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"age": {"type": "integer"}
						}
					}
				}
			}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(chatPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}

		// Verify Anthropic upstream received forced single tool with schema and tool_choice
		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatal("expected upstream request")
		}

		var upstreamBody struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
			ToolChoice struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v\n%s", err, reqs[0].Body)
		}
		if len(upstreamBody.Tools) != 1 || upstreamBody.Tools[0].Name != "person" {
			t.Fatalf("expected 1 tool 'person', got %+v", upstreamBody.Tools)
		}
		if upstreamBody.ToolChoice.Type != "tool" || upstreamBody.ToolChoice.Name != "person" {
			t.Fatalf("expected forced tool 'person', got %+v", upstreamBody.ToolChoice)
		}

		// Verify client received Chat response with unwrapped text and stop finish_reason
		var clientResp struct {
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &clientResp); err != nil {
			t.Fatalf("decode client response: %v\n%s", err, w.Body.String())
		}
		if len(clientResp.Choices) == 0 {
			t.Fatal("expected at least 1 choice")
		}
		if clientResp.Choices[0].FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop", clientResp.Choices[0].FinishReason)
		}
		if !strings.Contains(clientResp.Choices[0].Message.Content, `"Alice"`) {
			t.Errorf("content = %q, want JSON containing Alice", clientResp.Choices[0].Message.Content)
		}
	})

	t.Run("ResponsesToAnthropic_ToolEmulation", func(t *testing.T) {
		anthropicUpstreamResponse := `{
			"id": "msg_emul_test2",
			"type": "message",
			"role": "assistant",
			"model": "upstream-model",
			"content": [{
				"type": "tool_use",
				"id": "call_456",
				"name": "person",
				"input": {"name": "Bob", "age": 25}
			}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 20, "output_tokens": 10}
		}`

		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   anthropicUpstreamResponse,
		})

		prov := provider.Provider{
			ID:       "anthropic-emulated-resp",
			Name:     "Anthropic Emulated Resp",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
			Extensions: map[string]json.RawMessage{
				"tool_emulation": json.RawMessage(`true`),
			},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolAnthropicMessages,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		responsesPayload := `{
			"model": "test-model",
			"input": [{"role": "user", "content": "Extract person"}],
			"text": {
				"format": {
					"type": "json_schema",
					"name": "person",
					"strict": true,
					"schema": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"age": {"type": "integer"}
						}
					}
				}
			}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteResponses, strings.NewReader(responsesPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}

		// Verify Anthropic upstream received forced single tool with schema and tool_choice
		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatal("expected upstream request")
		}

		var upstreamBody struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			ToolChoice struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v\n%s", err, reqs[0].Body)
		}
		if len(upstreamBody.Tools) != 1 || upstreamBody.Tools[0].Name != "person" {
			t.Fatalf("expected 1 tool 'person', got %+v", upstreamBody.Tools)
		}

		// Verify client received Responses completed response with unwrapped text
		var clientResp struct {
			Status string `json:"status"`
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &clientResp); err != nil {
			t.Fatalf("decode client response: %v\n%s", err, w.Body.String())
		}
		if clientResp.Status != "completed" {
			t.Errorf("status = %q, want completed", clientResp.Status)
		}
		if len(clientResp.Output) == 0 || len(clientResp.Output[0].Content) == 0 {
			t.Fatal("expected output with content")
		}
		if !strings.Contains(clientResp.Output[0].Content[0].Text, `"Bob"`) {
			t.Errorf("text = %q, want JSON containing Bob", clientResp.Output[0].Content[0].Text)
		}
	})

	t.Run("AnthropicOutbound_DefaultUnsupported", func(t *testing.T) {
		prov := provider.Provider{
			ID:       "anthropic-default",
			Name:     "Anthropic Default",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  "https://upstream.invalid",
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
			Extensions: map[string]json.RawMessage{
				"feature_policy": json.RawMessage(`"strict"`),
			},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"test-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolAnthropicMessages,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{})
		dispatcher.Registry = setupMatrixRegistry()
		engine := gin.New()
		engine.Use(dispatcher.Handle)

		chatPayload := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "Extract person"}],
			"response_format": {
				"type": "json_schema",
				"json_schema": {
					"name": "person",
					"schema": {"type": "object"}
				}
			}
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(chatPayload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 Bad Request; body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "response_format") {
			t.Errorf("expected response_format mentioned in error, got: %s", w.Body.String())
		}
	})
}
