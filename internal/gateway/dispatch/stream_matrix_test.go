package dispatch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

type streamMatrixRecorder struct {
	mu          sync.Mutex
	completions []*core.Completion
	notify      chan struct{}
}

func newStreamMatrixRecorder() *streamMatrixRecorder {
	return &streamMatrixRecorder{notify: make(chan struct{}, 10)}
}

func (r *streamMatrixRecorder) Record(c *core.Completion) {
	r.mu.Lock()
	r.completions = append(r.completions, c)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *streamMatrixRecorder) waitCompletion(t *testing.T) *core.Completion {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		if len(r.completions) > 0 {
			c := r.completions[0]
			r.completions = r.completions[1:]
			r.mu.Unlock()
			return c
		}
		r.mu.Unlock()
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatal("timed out waiting for recorder completion")
			return nil
		}
	}
}

type streamMatrixPair struct {
	name                 string
	inboundProto         ir.Protocol
	inboundPath          string
	upstreamProto        ir.Protocol
	upstreamPath         string
	clientPayload        string
	cannedEvents         []string
	checkClientSSE       func(t *testing.T, body string)
	checkUpstreamRequest func(t *testing.T, body string)
}

func TestCrossProtocolStreamingMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chatUpstreamSSE := []string{
		"data: " + `{"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"upstream-model","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}` + "\n\n",
		"data: [DONE]\n\n",
	}

	anthropicUpstreamSSE := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}` + "\n\n",
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}` + "\n\n",
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
	}

	responsesUpstreamSSE := []string{
		"event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_stream","object":"response","status":"in_progress","model":"upstream-model"}}` + "\n\n",
		"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","status":"in_progress","role":"assistant"}}` + "\n\n",
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"delta":"Hello "}` + "\n\n",
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"delta":"world"}` + "\n\n",
		"event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0}` + "\n\n",
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_stream","status":"completed","model":"upstream-model","usage":{"input_tokens":12,"output_tokens":6,"total_tokens":18}}}` + "\n\n",
	}

	chatClientPayload := `{
		"model": "client-stream-model",
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [
			{"role": "user", "content": "Hello world"}
		]
	}`

	anthropicClientPayload := `{
		"model": "client-stream-model",
		"stream": true,
		"messages": [
			{"role": "user", "content": "Hello world"}
		],
		"max_tokens": 100
	}`

	responsesClientPayload := `{
		"model": "client-stream-model",
		"stream": true,
		"input": [
			{"role": "user", "content": "Hello world"}
		]
	}`

	checkChatClientSSE := func(t *testing.T, body string) {
		t.Helper()
		if !strings.Contains(body, `"delta":{"content":"Hello "}`) {
			t.Errorf("chat client missing 'Hello ' delta in:\n%s", body)
		}
		if !strings.Contains(body, `"delta":{"content":"world"}`) {
			t.Errorf("chat client missing 'world' delta in:\n%s", body)
		}
		if !strings.Contains(body, `"finish_reason":"stop"`) {
			t.Errorf("chat client missing finish_reason:stop in:\n%s", body)
		}
		if !strings.Contains(body, `"prompt_tokens":12`) || !strings.Contains(body, `"completion_tokens":6`) {
			t.Errorf("chat client missing usage in:\n%s", body)
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Errorf("chat client missing [DONE] in:\n%s", body)
		}
	}

	checkAnthropicClientSSE := func(t *testing.T, body string) {
		t.Helper()
		if !strings.Contains(body, "event: message_start") {
			t.Errorf("anthropic client missing message_start in:\n%s", body)
		}
		if !strings.Contains(body, `"text":"Hello "`) {
			t.Errorf("anthropic client missing 'Hello ' delta in:\n%s", body)
		}
		if !strings.Contains(body, `"text":"world"`) {
			t.Errorf("anthropic client missing 'world' delta in:\n%s", body)
		}
		if !strings.Contains(body, `"stop_reason":"end_turn"`) {
			t.Errorf("anthropic client missing stop_reason in:\n%s", body)
		}
		if !strings.Contains(body, `"output_tokens":6`) {
			t.Errorf("anthropic client missing output_tokens in:\n%s", body)
		}
		if !strings.Contains(body, "event: message_stop") {
			t.Errorf("anthropic client missing message_stop in:\n%s", body)
		}
	}

	checkResponsesClientSSE := func(t *testing.T, body string) {
		t.Helper()
		if !strings.Contains(body, "event: response.created") {
			t.Errorf("responses client missing response.created in:\n%s", body)
		}
		if !strings.Contains(body, `"delta":"Hello "`) {
			t.Errorf("responses client missing 'Hello ' delta in:\n%s", body)
		}
		if !strings.Contains(body, `"delta":"world"`) {
			t.Errorf("responses client missing 'world' delta in:\n%s", body)
		}
		if !strings.Contains(body, "event: response.completed") {
			t.Errorf("responses client missing response.completed in:\n%s", body)
		}
		if !strings.Contains(body, `"output_tokens":6`) {
			t.Errorf("responses client missing output_tokens in:\n%s", body)
		}
	}

	checkChatUpstreamReq := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal chat upstream: %v\n%s", err, body)
		}
		if !req.Stream {
			t.Errorf("stream = false, want true")
		}
		if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Errorf("stream_options.include_usage = false, want true")
		}
	}

	pairs := []streamMatrixPair{
		{
			name:                 "Chat->Anthropic",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        chatClientPayload,
			cannedEvents:         anthropicUpstreamSSE,
			checkClientSSE:       checkChatClientSSE,
			checkUpstreamRequest: nil,
		},
		{
			name:                 "Chat->Responses",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        chatClientPayload,
			cannedEvents:         responsesUpstreamSSE,
			checkClientSSE:       checkChatClientSSE,
			checkUpstreamRequest: nil,
		},
		{
			name:                 "Anthropic->Chat",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        anthropicClientPayload,
			cannedEvents:         chatUpstreamSSE,
			checkClientSSE:       checkAnthropicClientSSE,
			checkUpstreamRequest: checkChatUpstreamReq,
		},
		{
			name:                 "Anthropic->Responses",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        anthropicClientPayload,
			cannedEvents:         responsesUpstreamSSE,
			checkClientSSE:       checkAnthropicClientSSE,
			checkUpstreamRequest: nil,
		},
		{
			name:                 "Responses->Chat",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        responsesClientPayload,
			cannedEvents:         chatUpstreamSSE,
			checkClientSSE:       checkResponsesClientSSE,
			checkUpstreamRequest: checkChatUpstreamReq,
		},
		{
			name:                 "Responses->Anthropic",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        responsesClientPayload,
			cannedEvents:         anthropicUpstreamSSE,
			checkClientSSE:       checkResponsesClientSSE,
			checkUpstreamRequest: nil,
		},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(p.upstreamPath, gatewaytest.Step{
				Events: p.cannedEvents,
			})

			prov := provider.Provider{
				ID:       "stream-provider",
				Name:     "Stream Provider",
				Protocol: p.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "test-secret"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-stream-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: p.upstreamProto,
					},
				},
			}

			recorder := newStreamMatrixRecorder()
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

			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("content-type = %q, want text/event-stream", ct)
			}

			p.checkClientSSE(t, w.Body.String())

			if p.checkUpstreamRequest != nil {
				reqs := upstream.Requests()
				if len(reqs) == 0 {
					t.Fatal("no upstream request recorded")
				}
				p.checkUpstreamRequest(t, reqs[0].Body)
			}

			completion := recorder.waitCompletion(t)
			if completion.Err != nil {
				t.Fatalf("completion error: %v", completion.Err)
			}
			if completion.Usage.InputTokens != 12 || completion.Usage.OutputTokens != 6 {
				t.Errorf("completion usage = %+v, want input 12, output 6", completion.Usage)
			}
			if completion.Request.ClientModel != "client-stream-model" {
				t.Errorf("completion client model = %q, want client-stream-model", completion.Request.ClientModel)
			}
			// Flakiness-safe TTFB assertion
			if completion.Request.FirstByte.IsZero() {
				t.Error("expected FirstByte to be non-zero")
			}
			if completion.Request.FirstByte.Before(completion.Request.Start) {
				t.Errorf("FirstByte %v before Start %v", completion.Request.FirstByte, completion.Request.Start)
			}
			if completion.TTFB < 0 {
				t.Errorf("TTFB %v < 0", completion.TTFB)
			}
		})
	}
}

func TestCrossProtocolStreamingStopReasons(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chatLengthSSE := []string{
		"data: " + `{"id":"chatcmpl_len","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_len","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"Truncated answer"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_len","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n",
		"data: " + `{"id":"chatcmpl_len","object":"chat.completion.chunk","model":"upstream-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n",
		"data: [DONE]\n\n",
	}

	anthropicMaxTokensSSE := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_len","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Truncated answer"}}` + "\n\n",
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":2}}` + "\n\n",
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
	}

	responsesMaxTokensSSE := []string{
		"event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_len","object":"response","status":"in_progress","model":"upstream-model"}}` + "\n\n",
		"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","status":"in_progress","role":"assistant"}}` + "\n\n",
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"delta":"Truncated answer"}` + "\n\n",
		"event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0}` + "\n\n",
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_len","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"upstream-model","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}` + "\n\n",
	}

	chatPayload := `{"model":"client-stream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	anthropicPayload := `{"model":"client-stream-model","stream":true,"max_tokens":2,"messages":[{"role":"user","content":"hi"}]}`
	responsesPayload := `{"model":"client-stream-model","stream":true,"input":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name          string
		inboundPath   string
		upstreamProto ir.Protocol
		upstreamPath  string
		payload       string
		upstreamSSE   []string
		wantSubstring string
	}{
		{"Chat<-Anthropic (length)", gatewaytest.RouteChat, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, chatPayload, anthropicMaxTokensSSE, `"finish_reason":"length"`},
		{"Chat<-Responses (length)", gatewaytest.RouteChat, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, chatPayload, responsesMaxTokensSSE, `"finish_reason":"length"`},
		{"Anthropic<-Chat (max_tokens)", gatewaytest.RouteMessages, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, anthropicPayload, chatLengthSSE, `"stop_reason":"max_tokens"`},
		{"Anthropic<-Responses (max_tokens)", gatewaytest.RouteMessages, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, anthropicPayload, responsesMaxTokensSSE, `"stop_reason":"max_tokens"`},
		{"Responses<-Chat (incomplete)", gatewaytest.RouteResponses, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, responsesPayload, chatLengthSSE, `"status":"incomplete"`},
		{"Responses<-Anthropic (incomplete)", gatewaytest.RouteResponses, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, responsesPayload, anthropicMaxTokensSSE, `"status":"incomplete"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(tc.upstreamPath, gatewaytest.Step{
				Events: tc.upstreamSSE,
			})

			prov := provider.Provider{
				ID:       "stop-provider",
				Name:     "Stop Provider",
				Protocol: tc.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "key"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-stream-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: tc.upstreamProto,
					},
				},
			}

			recorder := newStreamMatrixRecorder()
			dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.inboundPath, strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantSubstring) {
				t.Errorf("body missing %q in:\n%s", tc.wantSubstring, w.Body.String())
			}

			completion := recorder.waitCompletion(t)
			if completion.Err != nil {
				t.Fatalf("unexpected completion error: %v", completion.Err)
			}
			if completion.Usage.InputTokens != 10 || completion.Usage.OutputTokens != 2 {
				t.Errorf("unexpected usage: %+v", completion.Usage)
			}
		})
	}
}

func TestCrossProtocolStreamingErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chatErrSSE := []string{
		"data: " + `{"id":"chatcmpl_err","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"First chunk"},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"error":{"message":"midstream rate limit exceeded","type":"rate_limit_exceeded"}}` + "\n\n",
	}

	anthropicErrSSE := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_err","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"First chunk"}}` + "\n\n",
		"event: error\ndata: " + `{"type":"error","error":{"type":"rate_limit_error","message":"midstream rate limit exceeded"}}` + "\n\n",
	}

	responsesErrSSE := []string{
		"event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_err","object":"response","status":"in_progress","model":"upstream-model"}}` + "\n\n",
		"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","status":"in_progress","role":"assistant"}}` + "\n\n",
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"delta":"First chunk"}` + "\n\n",
		"event: response.failed\ndata: " + `{"type":"response.failed","response":{"id":"resp_err","status":"failed","error":{"code":"rate_limit_exceeded","message":"midstream rate limit exceeded"}}}` + "\n\n",
	}

	chatPayload := `{"model":"client-stream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	anthropicPayload := `{"model":"client-stream-model","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	responsesPayload := `{"model":"client-stream-model","stream":true,"input":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name          string
		inboundPath   string
		upstreamProto ir.Protocol
		upstreamPath  string
		payload       string
		upstreamSSE   []string
	}{
		{"Chat<-Anthropic error", gatewaytest.RouteChat, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, chatPayload, anthropicErrSSE},
		{"Chat<-Responses error", gatewaytest.RouteChat, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, chatPayload, responsesErrSSE},
		{"Anthropic<-Chat error", gatewaytest.RouteMessages, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, anthropicPayload, chatErrSSE},
		{"Anthropic<-Responses error", gatewaytest.RouteMessages, ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, anthropicPayload, responsesErrSSE},
		{"Responses<-Chat error", gatewaytest.RouteResponses, ir.ProtocolOpenAIChat, gatewaytest.RouteChat, responsesPayload, chatErrSSE},
		{"Responses<-Anthropic error", gatewaytest.RouteResponses, ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, responsesPayload, anthropicErrSSE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(tc.upstreamPath, gatewaytest.Step{
				Events: tc.upstreamSSE,
			})

			prov := provider.Provider{
				ID:       "err-provider",
				Name:     "Err Provider",
				Protocol: tc.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "key"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-stream-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: tc.upstreamProto,
					},
				},
			}

			recorder := newStreamMatrixRecorder()
			dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.inboundPath, strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if !strings.Contains(w.Body.String(), "midstream rate limit exceeded") {
				t.Fatalf("expected error message in stream body, got:\n%s", w.Body.String())
			}

			completion := recorder.waitCompletion(t)
			if completion.Err == nil {
				t.Fatal("expected completion error for mid-stream error")
			}
		})
	}
}

func TestCrossProtocolStreamingClientCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[]}}\n\n")); err != nil {
			t.Errorf("write message_start: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		<-r.Context().Done()
		select {
		case <-upstreamCanceled:
		default:
			close(upstreamCanceled)
		}
	}))
	defer server.Close()

	prov := provider.Provider{
		ID:       "cancel-provider",
		Name:     "Cancel Provider",
		Protocol: ir.ProtocolAnthropicMessages,
		BaseURL:  server.URL,
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "key"},
	}

	resolver := staticResolver{
		routes: map[string]routing.Route{
			"client-stream-model": {
				Kind:             routing.Custom,
				Provider:         &prov,
				UpstreamModel:    "upstream-model",
				UpstreamProtocol: ir.ProtocolAnthropicMessages,
			},
		},
	}

	recorder := newStreamMatrixRecorder()
	dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
	dispatcher.Registry = setupMatrixRegistry()

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	ctx, cancel := context.WithCancel(context.Background())

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"client-stream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream to start")
	}

	// Cancel client context
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to return on cancellation")
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream context cancellation")
	}

	completion := recorder.waitCompletion(t)
	if completion.Err == nil {
		t.Fatal("expected completion error on cancellation")
	}
}
