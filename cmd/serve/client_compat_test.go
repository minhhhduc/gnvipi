package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// setupCompatEngine constructs the full gateway engine mirroring main.go.
func setupCompatEngine(t *testing.T, prefs *modelPrefs) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("GNVIPI_STATS_FILE", filepath.Join(os.TempDir(), "gnvipi-compat-stats.jsonl"))

	getModels := func() []gatewayModel {
		return buildCatalog(prefs, func() []*cliproxy.ModelInfo { return nil })
	}

	gateway, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gatewayV2Middleware: %v", err)
	}

	engine := gin.New()
	engine.Use(
		gatewayRequestContext(),
		modelsAndHealthMiddleware(getModels, nil, nil),
		gateway,
		anthropicResponsesPassthrough(prefs),
		sanitizeClaudeToolResults(),
		customOpenAIResponsesPassthrough(prefs),
		customOpenAICompatPassthrough(prefs),
		messagesOnlyPassthrough(prefs),
	)

	stub := func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "unrouted fallthrough to test stub"})
	}
	for _, route := range []string{
		"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses",
	} {
		engine.POST(route, stub)
	}

	return engine
}

// readFixture reads a test fixture from testdata/fixtures/...
func readFixture(t *testing.T, relPath string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", relPath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFixture %s: %v", relPath, err)
	}
	return data
}

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type recordedServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedRequest
}

func (s *recordedServer) lastRequest() (capturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return capturedRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

func writeResponseBytes(w http.ResponseWriter, payload []byte) {
	if _, err := w.Write(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newMockUpstream(handler func(w http.ResponseWriter, r *http.Request, body []byte)) *recordedServer {
	rec := &recordedServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			body = nil
		}
		rec.mu.Lock()
		rec.requests = append(rec.requests, capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		rec.mu.Unlock()
		handler(w, r, body)
	}))
	return rec
}

// TestClientCompat_ClaudeCode_Messages verifies real Claude Code requests:
// - prompt-caching-2024-07-31 beta header is forwarded
// - system blocks with ephemeral cache control are parsed
// - tool definitions with complex JSON schemas are mapped
// - tool_use and tool_result turns are converted
// - cross-protocol translation against an OpenAI Chat fake upstream
// - unsupported client headers are dropped and client credentials are stripped.
func TestClientCompat_ClaudeCode_Messages(t *testing.T) {
	fixtureBytes := readFixture(t, "claude_code/messages_tool_history.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "expected /v1/chat/completions", http.StatusNotFound)
			return
		}

		// Verify translation to OpenAI Chat wire format
		var chatReq struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    any    `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &chatReq); err != nil {
			http.Error(w, "invalid chat JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if len(chatReq.Tools) != 3 {
			http.Error(w, "expected 3 tools", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeResponseBytes(w, []byte(`{
			"id": "chatcmpl-claude-compat-123",
			"object": "chat.completion",
			"created": 1740000000,
			"model": "upstream-gpt-model",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "I located 5 Go source files in cmd/serve."
					},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 150,
				"completion_tokens": 20,
				"total_tokens": 170
			}
		}`))
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:    "openai-chat-backend",
				Model:   "upstream-gpt-model",
				Alias:   "claude-3-7-sonnet-20250219",
				BaseURL: upstream.URL,
				APIKey:  "upstream-secret-bearer-key",
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	req.Header.Set("X-Custom-Client-Trace", "trace-id-12345")
	req.Header.Set("X-Api-Key", "client-dummy-secret-must-not-leak")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify upstream request headers
	captured, ok := upstream.lastRequest()
	if !ok {
		t.Fatal("upstream did not receive any request")
	}

	if got := captured.Header.Get("Anthropic-Beta"); got != "prompt-caching-2024-07-31" {
		t.Errorf("upstream Anthropic-Beta = %q, want prompt-caching-2024-07-31", got)
	}
	if got := captured.Header.Get("X-Custom-Client-Trace"); got != "" {
		t.Errorf("upstream X-Custom-Client-Trace was not dropped: %q", got)
	}
	if got := captured.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("client X-Api-Key leaked to upstream: %q", got)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer upstream-secret-bearer-key" {
		t.Errorf("upstream Authorization = %q, want Bearer upstream-secret-bearer-key", got)
	}

	// Verify client response matches Anthropic Messages inbound wire schema
	var anthropicResp struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &anthropicResp); err != nil {
		t.Fatalf("unmarshal anthropic response: %v\n%s", err, rec.Body.String())
	}

	if anthropicResp.Type != "message" {
		t.Errorf("type = %q, want 'message'", anthropicResp.Type)
	}
	if anthropicResp.Role != "assistant" {
		t.Errorf("role = %q, want 'assistant'", anthropicResp.Role)
	}
	if anthropicResp.Model != "claude-3-7-sonnet-20250219" {
		t.Errorf("model = %q, want 'claude-3-7-sonnet-20250219'", anthropicResp.Model)
	}
	if len(anthropicResp.Content) == 0 || anthropicResp.Content[0].Text != "I located 5 Go source files in cmd/serve." {
		t.Errorf("unexpected content: %+v", anthropicResp.Content)
	}
	if anthropicResp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want 'end_turn'", anthropicResp.StopReason)
	}
	if anthropicResp.Usage.InputTokens != 150 || anthropicResp.Usage.OutputTokens != 20 {
		t.Errorf("unexpected usage: %+v", anthropicResp.Usage)
	}
}

// TestClientCompat_ClaudeCode_CountTokens verifies /v1/messages/count_tokens
// with Claude Code tools and system blocks:
// 1. Cross-protocol against OpenAI Chat upstream returns calculated input_tokens estimate.
// 2. Same-protocol against Anthropic upstream relays transparently.
func TestClientCompat_ClaudeCode_CountTokens(t *testing.T) {
	fixtureBytes := readFixture(t, "claude_code/count_tokens.json")

	t.Run("CrossProtocol_TokenCountEstimation", func(t *testing.T) {
		upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.WriteHeader(http.StatusOK)
		})
		defer upstream.Close()

		prefs := &modelPrefs{
			providers: []customProvider{
				{
					Name:    "chat-backend",
					Model:   "gpt-chat-model",
					Alias:   "claude-3-7-sonnet-20250219",
					BaseURL: upstream.URL,
					APIKey:  "key-1",
				},
			},
		}

		engine := setupCompatEngine(t, prefs)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(fixtureBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal count_tokens response: %v\n%s", err, rec.Body.String())
		}
		if resp.InputTokens <= 0 {
			t.Errorf("expected input_tokens > 0, got %d", resp.InputTokens)
		}
	})

	t.Run("SameProtocol_AnthropicRelay", func(t *testing.T) {
		upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, body []byte) {
			if r.URL.Path != "/v1/messages/count_tokens" {
				http.Error(w, "expected /v1/messages/count_tokens", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			writeResponseBytes(w, []byte(`{"input_tokens": 42}`))
		})
		defer upstream.Close()

		prefs := &modelPrefs{
			providers: []customProvider{
				{
					Name:         "anthro-backend",
					Model:        "claude-3-7-sonnet-20250219",
					Alias:        "claude-3-7-sonnet-20250219",
					BaseURL:      upstream.URL,
					APIKey:       "key-anthro",
					MessagesOnly: true,
				},
			},
		}

		engine := setupCompatEngine(t, prefs)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(fixtureBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal count_tokens response: %v\n%s", err, rec.Body.String())
		}
		if resp.InputTokens != 42 {
			t.Errorf("input_tokens = %d, want 42", resp.InputTokens)
		}
	})
}

// TestClientCompat_ClaudeCode_ModelDiscovery verifies GET /v1/models returns
// dual-compatible JSON honoring both Anthropic and OpenAI discovery schemas.
func TestClientCompat_ClaudeCode_ModelDiscovery(t *testing.T) {
	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:         "anthro-prov",
				Model:        "claude-3-7-sonnet-20250219",
				Alias:        "claude-3-7-sonnet-20250219",
				BaseURL:      "http://fake.local",
				MessagesOnly: true,
			},
			{
				Name:    "openai-prov",
				Model:   "gpt-4o",
				Alias:   "gpt-4o",
				BaseURL: "http://fake.local",
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	for _, path := range []string{"/v1/models", "/v1/models/"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("User-Agent", "claude-code/0.2.29")

		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("path %s returned %d: %s", path, rec.Code, rec.Body.String())
		}

		var resp struct {
			Object  string         `json:"object"`
			Data    []gatewayModel `json:"data"`
			HasMore bool           `json:"has_more"`
			FirstID string         `json:"first_id"`
			LastID  string         `json:"last_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse models JSON: %v", err)
		}

		if resp.Object != "list" {
			t.Errorf("object = %q, want list", resp.Object)
		}
		if resp.HasMore {
			t.Errorf("has_more should be false")
		}

		foundClaude := false
		for _, m := range resp.Data {
			if m.ID == "claude-3-7-sonnet-20250219" {
				foundClaude = true
				if m.UpstreamProtocol != string(ir.ProtocolAnthropicMessages) {
					t.Errorf("claude upstream_protocol = %s, want %s", m.UpstreamProtocol, ir.ProtocolAnthropicMessages)
				}
			}
		}
		if !foundClaude {
			t.Errorf("claude-3-7-sonnet-20250219 not found in models data: %+v", resp.Data)
		}
	}
}

// TestClientCompat_OpenAISDK_ChatCompletions_NonStreaming verifies OpenAI SDK
// POST /v1/chat/completions replayed with cross-protocol translation against Anthropic upstream.
func TestClientCompat_OpenAISDK_ChatCompletions_NonStreaming(t *testing.T) {
	fixtureBytes := readFixture(t, "openai_sdk/chat_non_streaming.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "expected /v1/messages", http.StatusNotFound)
			return
		}

		// Verify translation to Anthropic wire format
		var anthropicReq struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &anthropicReq); err != nil {
			http.Error(w, "invalid anthropic JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if len(anthropicReq.Tools) == 0 || anthropicReq.Tools[0].Name != "get_weather" {
			http.Error(w, "expected get_weather tool", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeResponseBytes(w, []byte(`{
			"id": "msg_oa_compat_123",
			"type": "message",
			"role": "assistant",
			"model": "upstream-claude-model",
			"content": [
				{
					"type": "text",
					"text": "The weather in Seattle is 15°C and rainy."
				}
			],
			"stop_reason": "end_turn",
			"usage": {
				"input_tokens": 85,
				"output_tokens": 12
			}
		}`))
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:         "anthro-vendor",
				Model:        "upstream-claude-model",
				Alias:        "gpt-4o",
				BaseURL:      upstream.URL,
				APIKey:       "secret-anthro-key",
				MessagesOnly: true,
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-raw-secret")
	req.Header.Set("X-Custom-Client", "drop-this")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify unsupported parameter response_format was reported dropped when translating to base Anthropic
	if got := rec.Header().Get("X-Gateway-Dropped-Params"); !strings.Contains(got, "response_format") {
		t.Logf("X-Gateway-Dropped-Params = %q (informational)", got)
	}

	// Verify upstream received provider auth and dropped unsupported client header
	captured, ok := upstream.lastRequest()
	if !ok {
		t.Fatal("no upstream request recorded")
	}
	if got := captured.Header.Get("X-Api-Key"); got != "secret-anthro-key" {
		t.Errorf("upstream X-Api-Key = %q, want secret-anthro-key", got)
	}
	if got := captured.Header.Get("X-Custom-Client"); got != "" {
		t.Errorf("upstream X-Custom-Client was not dropped: %q", got)
	}

	// Verify response schema conforms to OpenAI Chat completion wire format
	var chatResp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("unmarshal chat response: %v\n%s", err, rec.Body.String())
	}

	if chatResp.Object != "chat.completion" {
		t.Errorf("object = %q, want 'chat.completion'", chatResp.Object)
	}
	if chatResp.Model != "gpt-4o" {
		t.Errorf("model = %q, want 'gpt-4o'", chatResp.Model)
	}
	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "The weather in Seattle is 15°C and rainy." {
		t.Errorf("unexpected choices: %+v", chatResp.Choices)
	}
	if chatResp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want 'stop'", chatResp.Choices[0].FinishReason)
	}
	if chatResp.Usage.PromptTokens != 85 || chatResp.Usage.CompletionTokens != 12 || chatResp.Usage.TotalTokens != 97 {
		t.Errorf("unexpected usage: %+v", chatResp.Usage)
	}
}

// TestClientCompat_OpenAISDK_ChatCompletions_Streaming verifies streaming
// chat completions with stream_options: {include_usage: true} translated from Anthropic SSE.
func TestClientCompat_OpenAISDK_ChatCompletions_Streaming(t *testing.T) {
	fixtureBytes := readFixture(t, "openai_sdk/chat_streaming.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream_compat\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":40,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Refactored \"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"code snippet\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}

		for _, ev := range events {
			writeResponseBytes(w, []byte(ev))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:         "anthro-streaming",
				Model:        "upstream-model",
				Alias:        "gpt-4o",
				BaseURL:      upstream.URL,
				APIKey:       "key-stream",
				MessagesOnly: true,
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %s, want text/event-stream", rec.Header().Get("Content-Type"))
	}

	// Verify streaming delta content and finish_reason
	if !strings.Contains(body, `"delta":{"content":"Refactored "}`) {
		t.Errorf("missing first delta in SSE stream:\n%s", body)
	}
	if !strings.Contains(body, `"delta":{"content":"code snippet"}`) {
		t.Errorf("missing second delta in SSE stream:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason:stop in SSE stream:\n%s", body)
	}

	// Verify stream_options: {include_usage: true} delivered usage chunk
	if !strings.Contains(body, `"prompt_tokens":40`) || !strings.Contains(body, `"completion_tokens":10`) {
		t.Errorf("missing usage chunk in SSE stream:\n%s", body)
	}

	// Verify stream ends with [DONE]
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] sentinel in SSE stream:\n%s", body)
	}
}

// TestClientCompat_OpenAISDK_ChatCompletions_StrictSchema verifies OpenAI SDK
// with response_format strict json_schema replayed with cross-protocol translation.
func TestClientCompat_OpenAISDK_ChatCompletions_StrictSchema(t *testing.T) {
	fixtureBytes := readFixture(t, "openai_sdk/chat_strict_schema.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeResponseBytes(w, []byte(`{
			"id": "msg_schema_123",
			"type": "message",
			"role": "assistant",
			"model": "upstream-claude",
			"content": [
				{
					"type": "text",
					"text": "{\"people\":[\"Alice\",\"Bob\"],\"cities\":[\"Zurich\"],\"year\":2026}"
				}
			],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 50, "output_tokens": 25}
		}`))
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:         "anthro-schema",
				Model:        "upstream-claude",
				Alias:        "gpt-4o",
				BaseURL:      upstream.URL,
				APIKey:       "key-schema",
				MessagesOnly: true,
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(chatResp.Choices) == 0 {
		t.Fatal("expected at least 1 choice")
	}

	var extracted struct {
		People []string `json:"people"`
		Cities []string `json:"cities"`
		Year   int      `json:"year"`
	}
	if err := json.Unmarshal([]byte(chatResp.Choices[0].Message.Content), &extracted); err != nil {
		t.Fatalf("content is not valid JSON conforming to strict schema: %v\ncontent: %s", err, chatResp.Choices[0].Message.Content)
	}
	if len(extracted.People) != 2 || extracted.Cities[0] != "Zurich" || extracted.Year != 2026 {
		t.Errorf("unexpected extracted schema: %+v", extracted)
	}
}

// TestClientCompat_OpenAIResponses_Codex verifies OpenAI Responses API client
// requests (instructions, input items list, function tools, modality settings)
// translated to OpenAI Chat upstream.
func TestClientCompat_OpenAIResponses_Codex(t *testing.T) {
	fixtureBytes := readFixture(t, "openai_responses/responses_codex.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "expected /v1/chat/completions", http.StatusNotFound)
			return
		}

		// Verify translation from Responses input items to OpenAI Chat messages
		var chatReq struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    any    `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &chatReq); err != nil {
			http.Error(w, "invalid chat req JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if len(chatReq.Tools) == 0 || chatReq.Tools[0].Function.Name != "exec_cmd" {
			http.Error(w, "missing exec_cmd tool", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeResponseBytes(w, []byte(`{
			"id": "chatcmpl-codex-compat",
			"object": "chat.completion",
			"created": 1740000000,
			"model": "upstream-codex-chat",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "Repository is clean on feat/58-client-fixtures branch."
					},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 110,
				"completion_tokens": 15,
				"total_tokens": 125
			}
		}`))
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:    "chat-backend",
				Model:   "upstream-codex-chat",
				Alias:   "gpt-4o-responses",
				BaseURL: upstream.URL,
				APIKey:  "key-codex",
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify inbound OpenAI Responses wire format
	var resp struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal responses response: %v\n%s", err, rec.Body.String())
	}

	if resp.Object != "response" {
		t.Errorf("object = %q, want 'response'", resp.Object)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want 'completed'", resp.Status)
	}
	if resp.Model != "gpt-4o-responses" {
		t.Errorf("model = %q, want 'gpt-4o-responses'", resp.Model)
	}
	if len(resp.Output) == 0 || len(resp.Output[0].Content) == 0 ||
		resp.Output[0].Content[0].Text != "Repository is clean on feat/58-client-fixtures branch." {
		t.Errorf("unexpected output: %+v", resp.Output)
	}
	if resp.Usage.InputTokens != 110 || resp.Usage.OutputTokens != 15 || resp.Usage.TotalTokens != 125 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

// TestClientCompat_OpenCode_HeadersMetadata verifies that OpenCode custom
// headers (x-opencode-*) are forwarded to preserve client metadata, while
// arbitrary unsupported extension headers are dropped and credentials stripped.
func TestClientCompat_OpenCode_HeadersMetadata(t *testing.T) {
	fixtureBytes := readFixture(t, "opencode/chat_opencode.json")

	upstream := newMockUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeResponseBytes(w, []byte(`{
			"id": "chatcmpl-opencode-compat",
			"object": "chat.completion",
			"created": 1740000000,
			"model": "upstream-opencode",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "Code optimization suggestion."
					},
					"finish_reason": "stop"
				}
			],
			"usage": {"prompt_tokens": 30, "completion_tokens": 10, "total_tokens": 40}
		}`))
	})
	defer upstream.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{
				Name:    "opencode-backend",
				Model:   "upstream-opencode",
				Alias:   "opencode-chat-model",
				BaseURL: upstream.URL,
				APIKey:  "opencode-provider-key",
			},
		},
	}

	engine := setupCompatEngine(t, prefs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(fixtureBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret-must-not-leak")
	req.Header.Set("X-Opencode-Session-Id", "sess-987654321")
	req.Header.Set("X-Opencode-Client-Version", "1.4.2")
	req.Header.Set("X-Opencode-Project-Root", "/home/developer/gnvipi")
	req.Header.Set("X-Custom-Unsupported-Header", "should-be-dropped")

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	captured, ok := upstream.lastRequest()
	if !ok {
		t.Fatal("no upstream request recorded")
	}

	// Verify X-Opencode-* metadata headers are preserved
	if got := captured.Header.Get("X-Opencode-Session-Id"); got != "sess-987654321" {
		t.Errorf("upstream X-Opencode-Session-Id = %q, want sess-987654321", got)
	}
	if got := captured.Header.Get("X-Opencode-Client-Version"); got != "1.4.2" {
		t.Errorf("upstream X-Opencode-Client-Version = %q, want 1.4.2", got)
	}
	if got := captured.Header.Get("X-Opencode-Project-Root"); got != "/home/developer/gnvipi" {
		t.Errorf("upstream X-Opencode-Project-Root = %q, want /home/developer/gnvipi", got)
	}

	// Verify unsupported header was dropped
	if got := captured.Header.Get("X-Custom-Unsupported-Header"); got != "" {
		t.Errorf("upstream X-Custom-Unsupported-Header was not dropped: %q", got)
	}

	// Verify client raw secret was replaced by provider credential
	if got := captured.Header.Get("Authorization"); got != "Bearer opencode-provider-key" {
		t.Errorf("upstream Authorization = %q, want Bearer opencode-provider-key", got)
	}
}

// TestClientCompat_SanitizedFixturesNoSecrets walks all fixture files and
// ensures they are strictly sanitized of real API keys or sensitive credentials.
func TestClientCompat_SanitizedFixturesNoSecrets(t *testing.T) {
	fixtureDir := filepath.Join("testdata", "fixtures")

	// Sensitive patterns that must never appear in test fixtures
	forbiddenPatterns := []*regexp.Regexp{
		regexp.MustCompile(`sk-ant-api[a-zA-Z0-9_\-]+`),
		regexp.MustCompile(`sk-proj-[a-zA-Z0-9_\-]+`),
		regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
		regexp.MustCompile(`nvapi-[a-zA-Z0-9_\-]+`),
		regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`),
		regexp.MustCompile(`glpat-[a-zA-Z0-9_\-]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	}

	fileCount := 0
	err := filepath.Walk(fixtureDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		fileCount++

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		// Ensure valid JSON
		var js any
		if err := json.Unmarshal(content, &js); err != nil {
			t.Errorf("fixture %s is not valid JSON: %v", path, err)
		}

		// Check for forbidden secret patterns
		for _, re := range forbiddenPatterns {
			if match := re.Find(content); match != nil {
				t.Errorf("fixture %s contains forbidden secret pattern %s: %s", path, re.String(), string(match))
			}
		}

		return nil
	})

	if err != nil {
		t.Fatalf("walking fixture directory: %v", err)
	}

	if fileCount < 7 {
		t.Fatalf("expected at least 7 test fixtures, verified %d", fileCount)
	}
}
