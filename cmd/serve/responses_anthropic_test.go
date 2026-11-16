package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func TestAnthropicToResponsesConversationAndTools(t *testing.T) {
	in := anthropicRequest{Model: "gateway/muse", System: "be precise", MaxTokens: 42, Temperature: 0.2, TopP: 0.9, Stop: []string{"STOP"}, ToolChoice: map[string]any{"type": "tool", "name": "read"}, Tools: []map[string]any{{"name": "read", "description": "read file", "input_schema": map[string]any{"type": "object"}}}, Messages: []anthropicMessage{
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "first"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}}}},
		{Role: "system", Content: "also be safe"},
		{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": map[string]any{"path": "a"}}}},
		{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "contents"}}},
	}}
	out, err := anthropicToResponses(&in, "upstream-muse")
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "upstream-muse" || out["max_output_tokens"] != 42 || out["stop"].([]string)[0] != "STOP" {
		t.Fatalf("missing scalar conversion: %#v", out)
	}
	if out["tool_choice"].(map[string]any)["name"] != "read" {
		t.Fatalf("tool choice: %#v", out["tool_choice"])
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"role":"system"`, `"also be safe"`, `"type":"input_image"`, `data:image/png;base64,AA==`, `"type":"function_call"`, `"call_id":"call_1"`, `"type":"function_call_output"`, `"output":"contents"`} {
		if !strings.Contains(s, want) {
			t.Errorf("converted request missing %s: %s", want, s)
		}
	}
}

func TestAnthropicErrorDoesNotLeakProviderKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, `{"error":{"message":"key secret-token denied"}}`); err != nil {
			t.Error(err)
			return
		}
	}))
	defer up.Close()
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{providers: []customProvider{{Name: "gateway", Model: "muse", BaseURL: up.URL, APIKey: "secret-token", ResponsesOnly: true}}}
	e := gin.New()
	e.Use(anthropicResponsesPassthrough(p))
	e.POST("/v1/messages", func(c *gin.Context) { t.Fatal("adapter fell through") })
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gateway/muse","messages":[{"role":"user","content":"ping"}]}`)))
	if strings.Contains(w.Body.String(), "secret-token") || !strings.Contains(w.Body.String(), "[redacted]") {
		t.Fatalf("secret leak: %s", w.Body.String())
	}
}

func TestResponsesToAnthropicTextToolUsageAndStop(t *testing.T) {
	r := map[string]any{"id": "resp_1", "model": "upstream", "status": "completed", "usage": map[string]any{"input_tokens": 11, "output_tokens": 7}, "output": []any{
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": `{"path":"a"}`},
	}}
	out := responsesToAnthropic(r, "gateway")
	if out["id"] != "resp_1" || out["model"] != "upstream" || out["stop_reason"] != "tool_use" {
		t.Fatalf("response mapping: %#v", out)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" || content[1].(map[string]any)["input"].(map[string]any)["path"] != "a" {
		t.Fatalf("content mapping: %#v", content)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 11 || usage["output_tokens"] != 7 {
		t.Fatalf("usage: %#v", usage)
	}
}

func TestAnthropicResponsesPassthroughNonStreaming(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"id":"r","model":"muse","status":"completed","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"pong"}]}]}`); err != nil {
			t.Error(err)
			return
		}
	}))
	defer up.Close()
	w := runAnthropicAdapter(t, up.URL+"/v1", `{"model":"gateway/muse","max_tokens":8,"system":"sys","messages":[{"role":"user","content":"ping"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"pong"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got["model"] != "muse" || got["max_output_tokens"].(float64) != 8 {
		t.Fatalf("upstream request=%#v", got)
	}
}

func TestAnthropicResponsesPassthroughStreamingTextAndTool(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "event: response.created\ndata: {\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":2}}}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.output_text.delta\ndata: {\"output_index\":0,\"delta\":\"hi\"}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.output_item.done\ndata: {\"output_index\":0}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.output_item.added\ndata: {\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"read\"}}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.function_call_arguments.delta\ndata: {\"output_index\":1,\"delta\":\"{\"}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.output_item.done\ndata: {\"output_index\":1}\n\n"); err != nil {
			t.Error(err)
			return
		}
		if _, err := io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"usage\":{\"output_tokens\":4}}}\n\n"); err != nil {
			t.Error(err)
			return
		}
	}))
	defer up.Close()
	w := runAnthropicAdapter(t, up.URL, `{"model":"gateway/muse","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	body := w.Body.String()
	for _, event := range []string{"message_start", "content_block_start", "text_delta", "input_json_delta", "message_delta", "message_stop"} {
		if !strings.Contains(body, event) {
			t.Errorf("missing %s: %s", event, body)
		}
	}
}

func TestAnthropicResponsesPassthroughErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-request-id", "req_up")
				w.WriteHeader(status)
				if _, err := io.WriteString(w, `{"error":{"type":"FreeTierError","message":"upstream denied"}}`); err != nil {
					t.Error(err)
					return
				}
			}))
			defer up.Close()
			w := runAnthropicAdapter(t, up.URL, `{"model":"gateway/muse","messages":[{"role":"user","content":"ping"}]}`)
			if w.Code != status || !strings.Contains(w.Body.String(), "upstream denied") || strings.Contains(w.Body.String(), "Internal server error") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"provider_error_type":"FreeTierError"`) {
				t.Fatalf("provider type lost: %s", w.Body.String())
			}
			if w.Header().Get("request-id") != "req_up" {
				t.Fatalf("request id lost: %q", w.Header().Get("request-id"))
			}
		})
	}
}

func TestAnthropicResponsesMalformed(t *testing.T) {
	w := runAnthropicAdapter(t, "http://127.0.0.1:1", `{`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func runAnthropicAdapter(t *testing.T, baseURL, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{providers: []customProvider{{Name: "gateway", Model: "muse", BaseURL: baseURL, ResponsesOnly: true}}}
	e := gin.New()
	e.Use(anthropicResponsesPassthrough(p))
	e.POST("/v1/messages", func(c *gin.Context) { t.Fatal("adapter fell through") })
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	return w
}

func mustWriteSSE(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Errorf("write SSE: %v", err)
	}
}

func TestAnthropicResponsesPassthroughStreamingFailuresAndIncomplete(t *testing.T) {
	t.Run("response.failed", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			mustWriteSSE(t, w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-fixture\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"dummy upstream failure\"}}}\n\n")
			mustWriteSSE(t, w, "data: [DONE]\n\n")
		}))
		defer up.Close()

		w := runAnthropicAdapter(t, up.URL, `{"model":"gateway/muse","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
		body := w.Body.String()
		if !strings.Contains(body, "event: error") || !strings.Contains(body, "dummy upstream failure") {
			t.Fatalf("expected Anthropic error event, got:\n%s", body)
		}
		if strings.Contains(body, "message_stop") || strings.Contains(body, "end_turn") {
			t.Fatalf("unexpected success terminal in failed stream:\n%s", body)
		}
	})

	t.Run("error event", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			mustWriteSSE(t, w, "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"too many requests\"}\n\n")
			mustWriteSSE(t, w, "data: [DONE]\n\n")
		}))
		defer up.Close()

		w := runAnthropicAdapter(t, up.URL, `{"model":"gateway/muse","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
		body := w.Body.String()
		if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"rate_limit_error"`) || !strings.Contains(body, "too many requests") {
			t.Fatalf("expected Anthropic rate_limit_error event, got:\n%s", body)
		}
		if strings.Contains(body, "message_stop") || strings.Contains(body, "end_turn") {
			t.Fatalf("unexpected success terminal in error stream:\n%s", body)
		}
	})

	t.Run("response.incomplete", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			mustWriteSSE(t, w, "event: response.created\ndata: {\"response\":{\"id\":\"resp_inc\",\"usage\":{\"input_tokens\":5}}}\n\n")
			mustWriteSSE(t, w, "event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n")
			mustWriteSSE(t, w, "event: response.output_text.delta\ndata: {\"output_index\":0,\"delta\":\"partial\"}\n\n")
			mustWriteSSE(t, w, "event: response.output_item.done\ndata: {\"output_index\":0}\n\n")
			mustWriteSSE(t, w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_inc\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":42}}}\n\n")
		}))
		defer up.Close()

		w := runAnthropicAdapter(t, up.URL, `{"model":"gateway/muse","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
		body := w.Body.String()
		if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
			t.Fatalf("expected stop_reason max_tokens, got:\n%s", body)
		}
		if !strings.Contains(body, `"output_tokens":42`) {
			t.Fatalf("expected output_tokens 42, got:\n%s", body)
		}
		if !strings.Contains(body, "message_stop") {
			t.Fatalf("expected message_stop, got:\n%s", body)
		}
		if strings.Contains(body, "end_turn") {
			t.Fatalf("unexpected end_turn in incomplete stream:\n%s", body)
		}
	})
}

func TestAnthropicResponsesPassthroughStreamingAbruptEOF(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		mustWriteSSE(t, w, "event: response.created\ndata: {\"response\":{\"id\":\"resp_eof\",\"usage\":{\"input_tokens\":5}}}\n\n")
		mustWriteSSE(t, w, "event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n")
		mustWriteSSE(t, w, "event: response.output_text.delta\ndata: {\"output_index\":0,\"delta\":\"partial\"}\n\n")
	}))
	defer up.Close()

	gin.SetMode(gin.TestMode)
	const model = "gateway/muse-abrupt-eof"
	p := &modelPrefs{providers: []customProvider{{Name: "gateway", Model: "muse-abrupt-eof", BaseURL: up.URL, ResponsesOnly: true}}}
	e := gin.New()
	e.Use(anthropicResponsesPassthrough(p))
	e.POST("/v1/messages", func(c *gin.Context) { t.Fatal("adapter fell through") })
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "text_delta") {
		t.Fatalf("expected text_delta before EOF, got:\n%s", body)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"api_error"`) || !strings.Contains(body, "upstream stream ended before completion") {
		t.Fatalf("expected Anthropic api_error event on abrupt EOF, got:\n%s", body)
	}
	for _, forbidden := range []string{"message_delta", "message_stop", "end_turn", "tool_use"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected %s in abrupt-EOF stream:\n%s", forbidden, body)
		}
	}

	var stats *nvidia.ModelStats
	for _, row := range nvidia.GlobalStats.Snapshot() {
		if row.Model == model {
			r := row
			stats = &r
			break
		}
	}
	if stats == nil || stats.Requests != 1 || stats.Errors != 1 {
		t.Fatalf("expected failed=true recorded in stats, got: %+v", stats)
	}
}
