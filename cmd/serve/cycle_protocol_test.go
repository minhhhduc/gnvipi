package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// TestCycleCrossProtocol_AnthropicToOpenAIChat verifies that when the first candidate
// custom provider speaks Anthropic Messages and fails with 429, cycle advances to the
// second custom provider speaking OpenAI Chat, returns 200 translated to Anthropic Messages,
// and preserves the sticky index for subsequent requests.
func TestCycleCrossProtocol_AnthropicToOpenAIChat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var p1Calls, p2Calls int32

	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("p1 expected path /v1/messages, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)); err != nil {
			t.Errorf("write p1 response: %v", err)
		}
	}))
	defer s1.Close()

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Calls, 1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("p2 expected path /v1/chat/completions, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{
			"id":"chatcmpl-winner",
			"model":"gpt4",
			"object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai chat"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}
		}`)); err != nil {
			t.Errorf("write p2 response: %v", err)
		}
	}))
	defer s2.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{Name: "anthro", Model: "sonnet", BaseURL: s1.URL, MessagesOnly: true},
			{Name: "oa", Model: "gpt4", BaseURL: s2.URL},
		},
		cycle: &cycleConfig{Name: "cycle", Models: []string{"anthro/sonnet", "oa/gpt4"}},
	}

	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gateway middleware: %v", err)
	}

	engine := gin.New()
	engine.Use(gatewayRequestContext(), cycleRouter(prefs, engine), dispatcher)

	// First request: hits p1 (429), then retries p2 (200)
	reqBody := `{"model":"cycle","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(reqBody)))

	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, body = %s", w.Code, w.Body.String())
	}

	var anthropicResp struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &anthropicResp); err != nil {
		t.Fatalf("unmarshal anthropic response: %v, raw: %s", err, w.Body.String())
	}
	if anthropicResp.Role != "assistant" || len(anthropicResp.Content) == 0 || anthropicResp.Content[0].Text != "hello from openai chat" {
		t.Fatalf("unexpected anthropic response content: %+v", anthropicResp)
	}

	if atomic.LoadInt32(&p1Calls) != 1 || atomic.LoadInt32(&p2Calls) != 1 {
		t.Fatalf("expected p1=1 p2=1, got p1=%d p2=%d", p1Calls, p2Calls)
	}

	// Verify sticky index advanced to 1 (pointing to oa/gpt4)
	if _, idx := prefs.cycleSnapshot(); idx != 1 {
		t.Fatalf("expected cycle index 1, got %d", idx)
	}

	// Second request: sticky index should go directly to p2 without calling p1
	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(reqBody)))

	if w2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, body = %s", w2.Code, w2.Body.String())
	}
	if atomic.LoadInt32(&p1Calls) != 1 || atomic.LoadInt32(&p2Calls) != 2 {
		t.Fatalf("expected p1=1 p2=2 after sticky request, got p1=%d p2=%d", p1Calls, p2Calls)
	}
}

// TestCycleCrossProtocol_OpenAIChatToOpenAIResponses verifies cycling between
// an OpenAI Chat provider (returning 500) and an OpenAI Responses provider (returning 200).
func TestCycleCrossProtocol_OpenAIChatToOpenAIResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var p1Calls, p2Calls int32

	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte(`{"error":{"message":"internal error"}}`)); err != nil {
			t.Errorf("write p1 response: %v", err)
		}
	}))
	defer s1.Close()

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Calls, 1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("p2 expected path /v1/responses, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{
			"id":"resp-123",
			"model":"model-b",
			"object":"response",
			"status":"completed",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from responses api"}]}],
			"usage":{"input_tokens":5,"output_tokens":15}
		}`)); err != nil {
			t.Errorf("write p2 response: %v", err)
		}
	}))
	defer s2.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{Name: "chatprov", Model: "model-a", BaseURL: s1.URL},
			{Name: "respprov", Model: "model-b", BaseURL: s2.URL, ResponsesOnly: true},
		},
		cycle: &cycleConfig{Name: "cycle", Models: []string{"chatprov/model-a", "respprov/model-b"}},
	}

	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gateway middleware: %v", err)
	}

	engine := gin.New()
	engine.Use(gatewayRequestContext(), cycleRouter(prefs, engine), dispatcher)

	reqBody := `{"model":"cycle","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("unmarshal chat response: %v, raw: %s", err, w.Body.String())
	}
	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "hello from responses api" {
		t.Fatalf("unexpected chat response choices: %+v", chatResp)
	}

	if atomic.LoadInt32(&p1Calls) != 1 || atomic.LoadInt32(&p2Calls) != 1 {
		t.Fatalf("expected p1=1 p2=1, got p1=%d p2=%d", p1Calls, p2Calls)
	}
	if _, idx := prefs.cycleSnapshot(); idx != 1 {
		t.Fatalf("expected cycle index 1, got %d", idx)
	}
}

// TestCycleCrossProtocol_CountTokensSingleAttempt verifies that count_tokens
// only executes a single attempt on the active cycle candidate and does not advance.
func TestCycleCrossProtocol_CountTokensSingleAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var p1Calls, p2Calls int32

	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)); err != nil {
			t.Errorf("write p1 response: %v", err)
		}
	}))
	defer s1.Close()

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"input_tokens":42}`)); err != nil {
			t.Errorf("write p2 response: %v", err)
		}
	}))
	defer s2.Close()

	prefs := &modelPrefs{
		providers: []customProvider{
			{Name: "p1", Model: "m1", BaseURL: s1.URL, MessagesOnly: true},
			{Name: "p2", Model: "m2", BaseURL: s2.URL, MessagesOnly: true},
		},
		cycle: &cycleConfig{Name: "cycle", Models: []string{"p1/m1", "p2/m2"}},
	}

	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gateway middleware: %v", err)
	}

	engine := gin.New()
	engine.Use(gatewayRequestContext(), cycleRouter(prefs, engine), dispatcher)

	reqBody := `{"model":"cycle","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(reqBody)))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (%s)", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&p1Calls) != 1 {
		t.Fatalf("expected p1 called once, got %d", p1Calls)
	}
	if atomic.LoadInt32(&p2Calls) != 0 {
		t.Fatalf("expected p2 NOT called for count_tokens, got %d", p2Calls)
	}
	if _, idx := prefs.cycleSnapshot(); idx != 0 {
		t.Fatalf("cycle index should remain 0, got %d", idx)
	}
}

// TestCycleResolverCandidateOrder verifies ResolveCandidates and AdvanceCycle
// behavior directly through the resolver.
func TestCycleResolverCandidateOrder(t *testing.T) {
	prefs := &modelPrefs{
		providers: []customProvider{
			{Name: "prov1", Model: "mod1", BaseURL: "http://example.com/v1", MessagesOnly: true},
			{Name: "prov2", Model: "mod2", BaseURL: "http://example.com/v2"},
		},
		cycle: &cycleConfig{Name: "cycle", Models: []string{"prov1/mod1", "prov2/mod2"}},
	}

	resolver, err := prefs.modelResolver()
	if err != nil {
		t.Fatalf("modelResolver: %v", err)
	}

	ctx := context.Background()
	candidates, err := resolver.ResolveCandidates(ctx, "cycle", ir.ProtocolAnthropicMessages)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].UpstreamModel != "mod1" || candidates[0].UpstreamProtocol != ir.ProtocolAnthropicMessages {
		t.Fatalf("candidate 0 mismatch: %+v", candidates[0])
	}
	if candidates[1].UpstreamModel != "mod2" || candidates[1].UpstreamProtocol != ir.ProtocolOpenAIChat {
		t.Fatalf("candidate 1 mismatch: %+v", candidates[1])
	}

	// Advance cycle
	resolver.AdvanceCycle("prov1/mod1")

	candidatesAfter, err := resolver.ResolveCandidates(ctx, "cycle", ir.ProtocolAnthropicMessages)
	if err != nil {
		t.Fatalf("ResolveCandidates after advance: %v", err)
	}
	if len(candidatesAfter) != 2 {
		t.Fatalf("expected 2 candidates after advance, got %d", len(candidatesAfter))
	}
	if candidatesAfter[0].UpstreamModel != "mod2" {
		t.Fatalf("expected first candidate to be mod2 after advance, got %s", candidatesAfter[0].UpstreamModel)
	}
	if candidatesAfter[1].UpstreamModel != "mod1" {
		t.Fatalf("expected second candidate to be mod1 after advance, got %s", candidatesAfter[1].UpstreamModel)
	}
}
