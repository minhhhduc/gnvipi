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
	name           string
	inboundProto   ir.Protocol
	inboundPath    string
	upstreamProto  ir.Protocol
	upstreamPath   string
	clientPayload  string
	cannedResponse string
	checkResponse  func(t *testing.T, body string)
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

	pairs := []matrixPair{
		{
			name:           "Chat->Anthropic",
			inboundProto:   ir.ProtocolOpenAIChat,
			inboundPath:    gatewaytest.RouteChat,
			upstreamProto:  ir.ProtocolAnthropicMessages,
			upstreamPath:   gatewaytest.RouteMessages,
			clientPayload:  chatClientPayload,
			cannedResponse: anthropicUpstreamResponse,
			checkResponse:  checkChatClientResponse,
		},
		{
			name:           "Chat->Responses",
			inboundProto:   ir.ProtocolOpenAIChat,
			inboundPath:    gatewaytest.RouteChat,
			upstreamProto:  ir.ProtocolOpenAIResponses,
			upstreamPath:   gatewaytest.RouteResponses,
			clientPayload:  chatClientPayload,
			cannedResponse: responsesUpstreamResponse,
			checkResponse:  checkChatClientResponse,
		},
		{
			name:           "Anthropic->Chat",
			inboundProto:   ir.ProtocolAnthropicMessages,
			inboundPath:    gatewaytest.RouteMessages,
			upstreamProto:  ir.ProtocolOpenAIChat,
			upstreamPath:   gatewaytest.RouteChat,
			clientPayload:  anthropicClientPayload,
			cannedResponse: chatUpstreamResponse,
			checkResponse:  checkAnthropicClientResponse,
		},
		{
			name:           "Anthropic->Responses",
			inboundProto:   ir.ProtocolAnthropicMessages,
			inboundPath:    gatewaytest.RouteMessages,
			upstreamProto:  ir.ProtocolOpenAIResponses,
			upstreamPath:   gatewaytest.RouteResponses,
			clientPayload:  anthropicClientPayload,
			cannedResponse: responsesUpstreamResponse,
			checkResponse:  checkAnthropicClientResponse,
		},
		{
			name:           "Responses->Chat",
			inboundProto:   ir.ProtocolOpenAIResponses,
			inboundPath:    gatewaytest.RouteResponses,
			upstreamProto:  ir.ProtocolOpenAIChat,
			upstreamPath:   gatewaytest.RouteChat,
			clientPayload:  responsesClientPayload,
			cannedResponse: chatUpstreamResponse,
			checkResponse:  checkResponsesClientResponse,
		},
		{
			name:           "Responses->Anthropic",
			inboundProto:   ir.ProtocolOpenAIResponses,
			inboundPath:    gatewaytest.RouteResponses,
			upstreamProto:  ir.ProtocolAnthropicMessages,
			upstreamPath:   gatewaytest.RouteMessages,
			clientPayload:  responsesClientPayload,
			cannedResponse: anthropicUpstreamResponse,
			checkResponse:  checkResponsesClientResponse,
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
