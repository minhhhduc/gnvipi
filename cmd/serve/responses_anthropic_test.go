package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
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
