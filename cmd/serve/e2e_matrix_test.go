package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

type recordedUpstreamCall struct {
	Path   string
	Header http.Header
	Body   []byte
}

type fakeMatrixUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []recordedUpstreamCall
}

func (f *fakeMatrixUpstream) record(call recordedUpstreamCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeMatrixUpstream) lastCall() recordedUpstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return recordedUpstreamCall{}
	}
	return f.calls[len(f.calls)-1]
}

func writeTestResponse(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, val any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(val); err != nil {
		t.Errorf("encode json: %v", err)
	}
}

func setupFakeMatrixUpstream(t *testing.T) *fakeMatrixUpstream {
	t.Helper()
	fake := &fakeMatrixUpstream{}

	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if closeErr := r.Body.Close(); closeErr != nil {
			t.Errorf("close request body: %v", closeErr)
		}

		fake.record(recordedUpstreamCall{
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})

		var reqPayload struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if len(body) > 0 {
			if unmarshalErr := json.Unmarshal(body, &reqPayload); unmarshalErr != nil {
				t.Errorf("unmarshal request payload: %v", unmarshalErr)
			}
		}

		// Handle error test cases
		if strings.Contains(reqPayload.Model, "err4") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			switch {
			case strings.HasSuffix(r.URL.Path, "/messages"):
				writeTestResponse(t, w, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"anthropic 400 error"}}`))
			case strings.HasSuffix(r.URL.Path, "/responses"):
				writeTestResponse(t, w, []byte(`{"error":{"message":"responses 400 error","type":"invalid_request_error"}}`))
			default:
				writeTestResponse(t, w, []byte(`{"error":{"message":"chat 400 error","type":"invalid_request_error"}}`))
			}
			return
		}

		if strings.Contains(reqPayload.Model, "err5") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			switch {
			case strings.HasSuffix(r.URL.Path, "/messages"):
				writeTestResponse(t, w, []byte(`{"type":"error","error":{"type":"api_error","message":"anthropic 500 error"}}`))
			case strings.HasSuffix(r.URL.Path, "/responses"):
				writeTestResponse(t, w, []byte(`{"error":{"message":"responses 500 error","type":"api_error"}}`))
			default:
				writeTestResponse(t, w, []byte(`{"error":{"message":"chat 500 error","type":"api_error"}}`))
			}
			return
		}

		if strings.Contains(reqPayload.Model, "slow") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
		}

		if strings.Contains(reqPayload.Model, "miderr") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			switch {
			case strings.HasSuffix(r.URL.Path, "/messages"):
				writeTestResponse(t, w, []byte(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-miderr\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n", reqPayload.Model)))
				writeTestResponse(t, w, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
				writeTestResponse(t, w, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"initial \"}}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				writeTestResponse(t, w, []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"midstream error occurred\"}}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			case strings.HasSuffix(r.URL.Path, "/responses"):
				writeTestResponse(t, w, []byte(fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-miderr\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":%q}}\n\n", reqPayload.Model)))
				writeTestResponse(t, w, []byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_0\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\"}}\n\n"))
				writeTestResponse(t, w, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"initial \"}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				writeTestResponse(t, w, []byte("event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"midstream error occurred\"}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			default:
				writeTestResponse(t, w, []byte(fmt.Sprintf("data: {\"id\":\"chatcmpl-miderr\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"initial \"},\"finish_reason\":null}]}\n\n", reqPayload.Model)))
				if flusher != nil {
					flusher.Flush()
				}
				writeTestResponse(t, w, []byte("data: {\"error\":{\"message\":\"midstream error occurred\",\"type\":\"server_error\"}}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}

		// Standard generation cases
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if reqPayload.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				chunks := []string{
					fmt.Sprintf("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", reqPayload.Model),
					fmt.Sprintf("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Paris is \"},\"finish_reason\":null}],\"custom_stream_extra\":\"chat-stream-passthrough\"}\n\n", reqPayload.Model),
					fmt.Sprintf("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the capital\"},\"finish_reason\":null}]}\n\n", reqPayload.Model),
					fmt.Sprintf("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", reqPayload.Model),
					fmt.Sprintf("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":24,\"completion_tokens\":12,\"total_tokens\":36}}\n\n", reqPayload.Model),
					"data: [DONE]\n\n",
				}
				for _, ch := range chunks {
					writeTestResponse(t, w, []byte(ch))
					if flusher != nil {
						flusher.Flush()
					}
				}
			} else {
				writeTestJSON(t, w, map[string]any{
					"id":      "chatcmpl-e2e",
					"object":  "chat.completion",
					"created": 1700000000,
					"model":   reqPayload.Model,
					"choices": []any{
						map[string]any{
							"index": 0,
							"message": map[string]any{
								"role":    "assistant",
								"content": "Paris is the capital",
							},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]any{
						"prompt_tokens":     24,
						"completion_tokens": 12,
						"total_tokens":      36,
					},
					"custom_vendor_extra": "chat-verbatim-passthrough",
				})
			}

		case strings.HasSuffix(r.URL.Path, "/messages"):
			if reqPayload.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				events := []string{
					fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":24,\"output_tokens\":0}}}\n\n", reqPayload.Model),
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Paris is \"},\"custom_stream_extra\":\"anthropic-stream-passthrough\"}\n\n",
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"the capital\"}}\n\n",
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n\n",
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
				}
				for _, ev := range events {
					writeTestResponse(t, w, []byte(ev))
					if flusher != nil {
						flusher.Flush()
					}
				}
			} else {
				writeTestJSON(t, w, map[string]any{
					"id":          "msg-e2e",
					"type":        "message",
					"role":        "assistant",
					"model":       reqPayload.Model,
					"stop_reason": "end_turn",
					"content": []any{
						map[string]any{
							"type": "text",
							"text": "Paris is the capital",
						},
					},
					"usage": map[string]any{
						"input_tokens":  24,
						"output_tokens": 12,
					},
					"custom_vendor_extra": "anthropic-verbatim-passthrough",
				})
			}

		case strings.HasSuffix(r.URL.Path, "/responses"):
			if reqPayload.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				events := []string{
					fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":%q}}\n\n", reqPayload.Model),
					"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_0\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\"}}\n\n",
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"Paris is \",\"custom_stream_extra\":\"responses-stream-passthrough\"}\n\n",
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"the capital\"}\n\n",
					"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n",
					fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream\",\"status\":\"completed\",\"model\":%q,\"usage\":{\"input_tokens\":24,\"output_tokens\":12,\"total_tokens\":36}}}\n\n", reqPayload.Model),
				}
				for _, ev := range events {
					writeTestResponse(t, w, []byte(ev))
					if flusher != nil {
						flusher.Flush()
					}
				}
			} else {
				writeTestJSON(t, w, map[string]any{
					"id":     "resp-e2e",
					"object": "response",
					"status": "completed",
					"model":  reqPayload.Model,
					"output": []any{
						map[string]any{
							"id":     "msg_0",
							"type":   "message",
							"status": "completed",
							"role":   "assistant",
							"content": []any{
								map[string]any{
									"type": "output_text",
									"text": "Paris is the capital",
								},
							},
						},
					},
					"usage": map[string]any{
						"input_tokens":  24,
						"output_tokens": 12,
						"total_tokens":  36,
					},
					"custom_vendor_extra": "responses-verbatim-passthrough",
				})
			}

		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/messages", handler)
	mux.HandleFunc("/v1/responses", handler)

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

type testGatewayFixture struct {
	server  *httptest.Server
	prefs   *modelPrefs
	fakeUp  *fakeMatrixUpstream
	baseURL string
}

func bootTestGateway(t *testing.T) *testGatewayFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("GNVIPI_STATS_FILE", filepath.Join(os.TempDir(), "gnvipi-e2e-matrix-stats.jsonl"))

	fakeUp := setupFakeMatrixUpstream(t)

	prefs := &modelPrefs{
		providers: []customProvider{
			// Standard providers for 3 upstreams
			{Name: "up-chat", Model: "m-chat", BaseURL: fakeUp.server.URL + "/v1"},
			{Name: "up-anthropic", Model: "m-anthropic", BaseURL: fakeUp.server.URL + "/v1", MessagesOnly: true},
			{Name: "up-responses", Model: "m-responses", BaseURL: fakeUp.server.URL + "/v1", ResponsesOnly: true},

			// Error 4xx providers
			{Name: "err4-chat", Model: "m-chat-err400", BaseURL: fakeUp.server.URL + "/v1"},
			{Name: "err4-anthropic", Model: "m-anthropic-err400", BaseURL: fakeUp.server.URL + "/v1", MessagesOnly: true},
			{Name: "err4-responses", Model: "m-responses-err400", BaseURL: fakeUp.server.URL + "/v1", ResponsesOnly: true},

			// Error 5xx providers
			{Name: "err5-chat", Model: "m-chat-err500", BaseURL: fakeUp.server.URL + "/v1"},
			{Name: "err5-anthropic", Model: "m-anthropic-err500", BaseURL: fakeUp.server.URL + "/v1", MessagesOnly: true},
			{Name: "err5-responses", Model: "m-responses-err500", BaseURL: fakeUp.server.URL + "/v1", ResponsesOnly: true},

			// Mid-stream error providers
			{Name: "miderr-chat", Model: "m-chat-miderr", BaseURL: fakeUp.server.URL + "/v1"},
			{Name: "miderr-anthropic", Model: "m-anthropic-miderr", BaseURL: fakeUp.server.URL + "/v1", MessagesOnly: true},
			{Name: "miderr-responses", Model: "m-responses-miderr", BaseURL: fakeUp.server.URL + "/v1", ResponsesOnly: true},

			// Slow providers for cancellation
			{Name: "slow-chat", Model: "m-chat-slow", BaseURL: fakeUp.server.URL + "/v1"},
			{Name: "slow-anthropic", Model: "m-anthropic-slow", BaseURL: fakeUp.server.URL + "/v1", MessagesOnly: true},
			{Name: "slow-responses", Model: "m-responses-slow", BaseURL: fakeUp.server.URL + "/v1", ResponsesOnly: true},
		},
	}

	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gatewayV2Middleware: %v", err)
	}

	engine := gin.New()
	engine.Use(
		secretResponseRedaction(),
		requestBodyLimit(10<<20, 10<<20),
		gatewayRequestContext(),
		claudeAutoModelAlias(prefs),
		cycleRouter(prefs, engine),
		dispatcher,
	)

	for _, route := range []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/responses",
	} {
		engine.POST(route, func(c *gin.Context) {
			c.JSON(http.StatusNotFound, gin.H{"error": "route not handled by gateway"})
		})
	}

	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	return &testGatewayFixture{
		server:  server,
		prefs:   prefs,
		fakeUp:  fakeUp,
		baseURL: server.URL,
	}
}

func getE2EModelStats(model string) nvidia.ModelStats {
	for _, row := range nvidia.GlobalStats.Snapshot() {
		if row.Model == model {
			return row
		}
	}
	return nvidia.ModelStats{}
}

// buildMultiTurnClientPayload builds the client request body for each inbound protocol
func buildMultiTurnClientPayload(proto ir.Protocol, model string, stream bool) string {
	switch proto {
	case ir.ProtocolOpenAIChat:
		type chatMsg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		data := map[string]any{
			"model":  model,
			"stream": stream,
			"messages": []chatMsg{
				{Role: "system", Content: "You are a geography expert."},
				{Role: "user", Content: "What is the capital of France?"},
				{Role: "assistant", Content: "Paris"},
				{Role: "user", Content: "Tell me more."},
			},
			"max_tokens": 50,
		}
		if stream {
			data["stream_options"] = map[string]any{"include_usage": true}
		}
		b, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		return string(b)

	case ir.ProtocolAnthropicMessages:
		type antMsg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		data := map[string]any{
			"model":      model,
			"stream":     stream,
			"system":     "You are a geography expert.",
			"max_tokens": 50,
			"messages": []antMsg{
				{Role: "user", Content: "What is the capital of France?"},
				{Role: "assistant", Content: "Paris"},
				{Role: "user", Content: "Tell me more."},
			},
		}
		b, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		return string(b)

	case ir.ProtocolOpenAIResponses:
		type respMsg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		data := map[string]any{
			"model":             model,
			"stream":            stream,
			"instructions":      "You are a geography expert.",
			"max_output_tokens": 50,
			"input": []respMsg{
				{Role: "user", Content: "What is the capital of France?"},
				{Role: "assistant", Content: "Paris"},
				{Role: "user", Content: "Tell me more."},
			},
		}
		b, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return ""
}

// verifyUpstreamReceivedMultiTurn verifies that upstream received multi-turn history
func verifyUpstreamReceivedMultiTurn(t *testing.T, upstreamProto ir.Protocol, body []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}

	switch upstreamProto {
	case ir.ProtocolOpenAIChat:
		msgs, ok := raw["messages"].([]any)
		if !ok || len(msgs) < 3 {
			t.Fatalf("upstream chat messages incomplete: %+v", raw["messages"])
		}
	case ir.ProtocolAnthropicMessages:
		msgs, ok := raw["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("upstream anthropic messages incomplete: %+v", raw["messages"])
		}
	case ir.ProtocolOpenAIResponses:
		input, ok := raw["input"].([]any)
		if !ok || len(input) < 2 {
			t.Fatalf("upstream responses input incomplete: %+v", raw["input"])
		}
	}
}

// verifyClientResponseTextAndUsage verifies generated text and token usage across inbound protocols
func verifyClientResponseTextAndUsage(t *testing.T, inboundProto ir.Protocol, stream bool, respBody string, sameProtocol bool) {
	t.Helper()

	if stream {
		if !strings.Contains(respBody, "Paris is ") || !strings.Contains(respBody, "the capital") {
			t.Fatalf("streamed body missing expected deltas: %s", respBody)
		}
		if sameProtocol {
			if !strings.Contains(respBody, "custom_stream_extra") {
				t.Fatalf("same-protocol streaming passthrough lost unmodeled custom_stream_extra field: %s", respBody)
			}
		} else {
			if strings.Contains(respBody, "custom_stream_extra") {
				t.Fatalf("cross-protocol translation leaked unmodeled custom_stream_extra field: %s", respBody)
			}
		}
		return
	}

	// Non-streaming JSON validation
	switch inboundProto {
	case ir.ProtocolOpenAIChat:
		var res struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			CustomVendorExtra string `json:"custom_vendor_extra"`
		}
		if err := json.Unmarshal([]byte(respBody), &res); err != nil {
			t.Fatalf("unmarshal chat response: %v\nbody: %s", err, respBody)
		}
		if len(res.Choices) == 0 || !strings.Contains(res.Choices[0].Message.Content, "Paris is the capital") {
			t.Fatalf("chat content mismatch: %+v", res.Choices)
		}
		if res.Usage.PromptTokens != 24 || res.Usage.CompletionTokens != 12 {
			t.Fatalf("chat token usage mismatch: %+v", res.Usage)
		}
		if sameProtocol && res.CustomVendorExtra != "chat-verbatim-passthrough" {
			t.Fatalf("same-protocol chat passthrough lost custom vendor field: %q", res.CustomVendorExtra)
		}

	case ir.ProtocolAnthropicMessages:
		var res struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			CustomVendorExtra string `json:"custom_vendor_extra"`
		}
		if err := json.Unmarshal([]byte(respBody), &res); err != nil {
			t.Fatalf("unmarshal anthropic response: %v\nbody: %s", err, respBody)
		}
		if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "Paris is the capital") {
			t.Fatalf("anthropic content mismatch: %+v", res.Content)
		}
		if res.Usage.InputTokens != 24 || res.Usage.OutputTokens != 12 {
			t.Fatalf("anthropic token usage mismatch: %+v", res.Usage)
		}
		if sameProtocol && res.CustomVendorExtra != "anthropic-verbatim-passthrough" {
			t.Fatalf("same-protocol anthropic passthrough lost custom vendor field: %q", res.CustomVendorExtra)
		}

	case ir.ProtocolOpenAIResponses:
		var res struct {
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			CustomVendorExtra string `json:"custom_vendor_extra"`
		}
		if err := json.Unmarshal([]byte(respBody), &res); err != nil {
			t.Fatalf("unmarshal responses response: %v\nbody: %s", err, respBody)
		}
		if len(res.Output) == 0 || len(res.Output[0].Content) == 0 || !strings.Contains(res.Output[0].Content[0].Text, "Paris is the capital") {
			t.Fatalf("responses content mismatch: %+v", res.Output)
		}
		if res.Usage.InputTokens != 24 || res.Usage.OutputTokens != 12 {
			t.Fatalf("responses token usage mismatch: %+v", res.Usage)
		}
		if sameProtocol && res.CustomVendorExtra != "responses-verbatim-passthrough" {
			t.Fatalf("same-protocol responses passthrough lost custom vendor field: %q", res.CustomVendorExtra)
		}
	}
}

// TestE2EMatrix_18Combinations verifies all 18 combinations:
// 3 Inbounds x 3 Upstreams x 2 Modes (stream/non-stream)
// Covering:
// - Basic text generation and multi-turn message history.
// - Same-protocol byte-for-byte passthrough fidelity.
// - Cross-protocol translation fidelity (roles, content, token usage).
// - Telemetry in headers (X-Request-Id) and stats (nvidia.GlobalStats).
func TestE2EMatrix_18Combinations(t *testing.T) {
	fixture := bootTestGateway(t)
	client := fixture.server.Client()

	inboundConfigs := []struct {
		name  string
		proto ir.Protocol
		path  string
	}{
		{"InboundChat", ir.ProtocolOpenAIChat, "/v1/chat/completions"},
		{"InboundAnthropic", ir.ProtocolAnthropicMessages, "/v1/messages"},
		{"InboundResponses", ir.ProtocolOpenAIResponses, "/v1/responses"},
	}

	upstreamConfigs := []struct {
		name     string
		proto    ir.Protocol
		provider string
		model    string
	}{
		{"UpstreamChat", ir.ProtocolOpenAIChat, "up-chat", "up-chat/m-chat"},
		{"UpstreamAnthropic", ir.ProtocolAnthropicMessages, "up-anthropic", "up-anthropic/m-anthropic"},
		{"UpstreamResponses", ir.ProtocolOpenAIResponses, "up-responses", "up-responses/m-responses"},
	}

	modes := []struct {
		name   string
		stream bool
	}{
		{"NonStream", false},
		{"Stream", true},
	}

	var totalTests int32
	for _, in := range inboundConfigs {
		for _, up := range upstreamConfigs {
			for _, m := range modes {
				atomic.AddInt32(&totalTests, 1)
				testName := fmt.Sprintf("%s_x_%s_%s", in.name, up.name, m.name)
				sameProtocol := in.proto == up.proto

				t.Run(testName, func(t *testing.T) {
					beforeStats := getE2EModelStats(up.model)

					reqBody := buildMultiTurnClientPayload(in.proto, up.model, m.stream)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.baseURL+in.path, strings.NewReader(reqBody))
					if err != nil {
						t.Fatalf("create request: %v", err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("User-Agent", "e2e-matrix-test/1.0")
					if in.proto == ir.ProtocolAnthropicMessages {
						req.Header.Set("anthropic-version", "2023-06-01")
					}

					resp, err := client.Do(req)
					if err != nil {
						t.Fatalf("client.Do: %v", err)
					}
					defer func() {
						if closeErr := resp.Body.Close(); closeErr != nil {
							t.Errorf("close resp body: %v", closeErr)
						}
					}()

					if resp.StatusCode != http.StatusOK {
						b, readErr := io.ReadAll(resp.Body)
						if readErr != nil {
							t.Fatalf("status code = %d, read body error: %v", resp.StatusCode, readErr)
						}
						t.Fatalf("status code = %d, want 200, body: %s", resp.StatusCode, string(b))
					}

					// Verify telemetry header X-Request-Id
					reqID := resp.Header.Get(core.RequestIDHeader)
					if reqID == "" {
						t.Fatalf("missing %s response header", core.RequestIDHeader)
					}

					if m.stream {
						contentType := resp.Header.Get("Content-Type")
						if !strings.HasPrefix(contentType, "text/event-stream") {
							t.Fatalf("stream Content-Type want text/event-stream, got %q", contentType)
						}
					}

					respBytes, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatalf("read response body: %v", err)
					}

					// 1. Verify multi-turn history was received by upstream
					lastCall := fixture.fakeUp.lastCall()
					verifyUpstreamReceivedMultiTurn(t, up.proto, lastCall.Body)

					// 2. Verify client response content, usage and fidelity
					verifyClientResponseTextAndUsage(t, in.proto, m.stream, string(respBytes), sameProtocol)

					// 3. Verify telemetry stats in nvidia.GlobalStats
					afterStats := getE2EModelStats(up.model)
					if afterStats.Requests-beforeStats.Requests != 1 {
						t.Fatalf("stats requests diff = %d, want 1", afterStats.Requests-beforeStats.Requests)
					}
					if afterStats.Errors-beforeStats.Errors != 0 {
						t.Fatalf("stats errors diff = %d, want 0", afterStats.Errors-beforeStats.Errors)
					}
					if !m.stream {
						if afterStats.InputTok-beforeStats.InputTok != 24 {
							t.Fatalf("stats input tokens diff = %d, want 24", afterStats.InputTok-beforeStats.InputTok)
						}
						if afterStats.OutputTok-beforeStats.OutputTok != 12 {
							t.Fatalf("stats output tokens diff = %d, want 12", afterStats.OutputTok-beforeStats.OutputTok)
						}
					}
				})
			}
		}
	}

	if totalTests != 18 {
		t.Fatalf("total combinations executed = %d, want 18", totalTests)
	}
}

// TestE2EMatrix_UpstreamErrors verifies 4xx client errors and 5xx upstream server errors
// across all inbound protocols in both streaming and non-streaming modes.
func TestE2EMatrix_UpstreamErrors(t *testing.T) {
	fixture := bootTestGateway(t)
	client := fixture.server.Client()

	inbounds := []struct {
		name  string
		proto ir.Protocol
		path  string
	}{
		{"Chat", ir.ProtocolOpenAIChat, "/v1/chat/completions"},
		{"Anthropic", ir.ProtocolAnthropicMessages, "/v1/messages"},
		{"Responses", ir.ProtocolOpenAIResponses, "/v1/responses"},
	}

	errorCases := []struct {
		errName    string
		modelName  string
		wantStatus int
	}{
		{"4xx_ChatUpstream", "err4-chat/m-chat-err400", http.StatusBadRequest},
		{"4xx_AnthropicUpstream", "err4-anthropic/m-anthropic-err400", http.StatusBadRequest},
		{"4xx_ResponsesUpstream", "err4-responses/m-responses-err400", http.StatusBadRequest},
		{"5xx_ChatUpstream", "err5-chat/m-chat-err500", http.StatusInternalServerError},
		{"5xx_AnthropicUpstream", "err5-anthropic/m-anthropic-err500", http.StatusInternalServerError},
		{"5xx_ResponsesUpstream", "err5-responses/m-responses-err500", http.StatusInternalServerError},
	}

	for _, in := range inbounds {
		for _, ec := range errorCases {
			t.Run(fmt.Sprintf("%s_Inbound_%s", in.name, ec.errName), func(t *testing.T) {
				beforeStats := getE2EModelStats(ec.modelName)

				reqBody := buildMultiTurnClientPayload(in.proto, ec.modelName, false)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.baseURL+in.path, strings.NewReader(reqBody))
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				req.Header.Set("Content-Type", "application/json")
				if in.proto == ir.ProtocolAnthropicMessages {
					req.Header.Set("anthropic-version", "2023-06-01")
				}

				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("client.Do: %v", err)
				}
				defer func() {
					if closeErr := resp.Body.Close(); closeErr != nil {
						t.Errorf("close resp body: %v", closeErr)
					}
				}()

				b, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("read response body: %v", err)
				}

				switch ec.wantStatus {
				case http.StatusBadRequest:
					if resp.StatusCode != http.StatusBadRequest {
						t.Fatalf("status code = %d, want 400, body = %s", resp.StatusCode, string(b))
					}
				case http.StatusInternalServerError:
					if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusBadGateway {
						t.Fatalf("status code = %d, want 500 or 502, body = %s", resp.StatusCode, string(b))
					}
				}

				// Error response should contain request ID
				if resp.Header.Get(core.RequestIDHeader) == "" {
					t.Fatalf("missing %s in error response", core.RequestIDHeader)
				}

				// Stats should record an error
				afterStats := getE2EModelStats(ec.modelName)
				if afterStats.Errors-beforeStats.Errors != 1 {
					t.Fatalf("expected 1 error recorded in stats, got %d", afterStats.Errors-beforeStats.Errors)
				}
			})
		}
	}
}

// TestE2EMatrix_MidStreamErrors verifies mid-stream upstream error handling:
// Initial tokens are delivered, followed by upstream error event or clean termination.
func TestE2EMatrix_MidStreamErrors(t *testing.T) {
	fixture := bootTestGateway(t)
	client := fixture.server.Client()

	inbounds := []struct {
		name  string
		proto ir.Protocol
		path  string
	}{
		{"Chat", ir.ProtocolOpenAIChat, "/v1/chat/completions"},
		{"Anthropic", ir.ProtocolAnthropicMessages, "/v1/messages"},
		{"Responses", ir.ProtocolOpenAIResponses, "/v1/responses"},
	}

	upstreams := []struct {
		name  string
		model string
	}{
		{"ChatUpstream", "miderr-chat/m-chat-miderr"},
		{"AnthropicUpstream", "miderr-anthropic/m-anthropic-miderr"},
		{"ResponsesUpstream", "miderr-responses/m-responses-miderr"},
	}

	for _, in := range inbounds {
		for _, up := range upstreams {
			t.Run(fmt.Sprintf("%s_Inbound_%s", in.name, up.name), func(t *testing.T) {
				beforeStats := getE2EModelStats(up.model)

				reqBody := buildMultiTurnClientPayload(in.proto, up.model, true)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.baseURL+in.path, strings.NewReader(reqBody))
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				req.Header.Set("Content-Type", "application/json")
				if in.proto == ir.ProtocolAnthropicMessages {
					req.Header.Set("anthropic-version", "2023-06-01")
				}

				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("client.Do: %v", err)
				}
				defer func() {
					if closeErr := resp.Body.Close(); closeErr != nil {
						t.Errorf("close resp body: %v", closeErr)
					}
				}()

				if resp.StatusCode != http.StatusOK {
					b, readErr := io.ReadAll(resp.Body)
					if readErr != nil {
						t.Fatalf("status = %d, read body error: %v", resp.StatusCode, readErr)
					}
					t.Fatalf("expected 200 stream start, got %d, body=%s", resp.StatusCode, string(b))
				}

				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("read stream body: %v", err)
				}
				bodyStr := string(bodyBytes)

				// Initial content must have arrived before mid-stream error
				if !strings.Contains(bodyStr, "initial ") {
					t.Fatalf("stream did not receive initial content: %s", bodyStr)
				}

				// Must observe error event or error keyword
				if !strings.Contains(bodyStr, "error") {
					t.Fatalf("stream did not emit error event or payload: %s", bodyStr)
				}

				// Error should be recorded in telemetry stats
				afterStats := getE2EModelStats(up.model)
				if afterStats.Errors-beforeStats.Errors != 1 {
					t.Fatalf("stats error diff = %d, want 1", afterStats.Errors-beforeStats.Errors)
				}
			})
		}
	}
}

// TestE2EMatrix_ContextCancellation verifies client disconnect / context cancellation handling.
func TestE2EMatrix_ContextCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping context cancellation test in short mode")
	}

	fixture := bootTestGateway(t)
	client := fixture.server.Client()

	testModels := []struct {
		name  string
		model string
	}{
		{"Chat", "slow-chat/m-chat-slow"},
		{"Anthropic", "slow-anthropic/m-anthropic-slow"},
		{"Responses", "slow-responses/m-responses-slow"},
	}

	for _, tm := range testModels {
		t.Run(tm.name, func(t *testing.T) {
			beforeStats := getE2EModelStats(tm.model)

			ctx, cancel := context.WithCancel(t.Context())
			reqBody := buildMultiTurnClientPayload(ir.ProtocolOpenAIChat, tm.model, false)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.baseURL+"/v1/chat/completions", strings.NewReader(reqBody))
			if err != nil {
				cancel()
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			// Cancel context shortly after sending request
			go func() {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}()

			resp, err := client.Do(req)
			if err == nil {
				if closeErr := resp.Body.Close(); closeErr != nil {
					t.Errorf("close resp body: %v", closeErr)
				}
			}

			// Wait briefly for recorder to complete
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				afterStats := getE2EModelStats(tm.model)
				if afterStats.Requests-beforeStats.Requests >= 1 {
					if afterStats.Errors-beforeStats.Errors < 1 {
						t.Fatalf("expected cancellation recorded as error in stats")
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// TestE2EMatrix_StreamReaderDrain helper to read and verify SSE chunks
func TestE2EMatrix_StreamReaderDrain(t *testing.T) {
	fixture := bootTestGateway(t)
	client := fixture.server.Client()

	reqBody := buildMultiTurnClientPayload(ir.ProtocolOpenAIChat, "up-chat/m-chat", true)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.baseURL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("close resp body: %v", closeErr)
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Paris is ") || !strings.Contains(joined, "the capital") {
		t.Fatalf("stream lines missing deltas: %s", joined)
	}
}
