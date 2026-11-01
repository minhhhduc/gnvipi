package main

// Characterization tests for the custom-provider routing chain main.go
// installs. These pin today's observable behavior — target URL, model
// rewrite, auth headers, fallthrough, retries — as black-box HTTP tests so
// the dispatcher refactor (#14) can be shown to be a drop-in replacement:
// same requests in, same upstream calls and client responses out.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// mountChain installs the custom-provider middlewares in exactly the order
// main.go uses. stub stands in for CLIProxyAPI's own routes, so a request no
// middleware claims is observable as the stub's response.
func mountChain(t *testing.T, p *modelPrefs, stub gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// requestLogger tees responses into a stats file whose path a sync.Once
	// fixes for the whole test binary, and the handle then outlives this test.
	// A path under t.TempDir() therefore fails the step's cleanup on Windows
	// ("file in use"); the package's established idiom is a plain os.TempDir()
	// path with no cleanup, same as serve_test.go.
	t.Setenv("GNVIPI_STATS_FILE", filepath.Join(os.TempDir(), "gnvipi-characterization-stats.jsonl"))
	engine := gin.New()
	engine.Use(
		anthropicResponsesPassthrough(p),
		sanitizeClaudeToolResults(),
		customOpenAIResponsesPassthrough(p),
		customOpenAICompatPassthrough(p),
		messagesOnlyPassthrough(p),
		requestLogger(p),
		claudeAutoModelAlias(p),
		cycleRouter(p, engine),
	)
	for _, route := range []string{
		"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses",
	} {
		engine.POST(route, stub)
	}
	return engine
}

// echoStub answers with status/body, echoing the request body when it has
// one, so what the chain passed on is visible in the response.
func echoStub(status int, body string) gin.HandlerFunc {
	return func(c *gin.Context) {
		in, err := io.ReadAll(c.Request.Body)
		if err != nil || len(in) == 0 {
			c.String(status, body)
			return
		}
		c.Data(status, "application/json", in)
	}
}

// fakeUpstream replaces both package-level passthrough clients with a recorder
// that answers status/body, and reports what the gateway actually sent.
func fakeUpstream(t *testing.T, status int, body string) *upstreamLog {
	t.Helper()
	log := &upstreamLog{}
	rt := testRoundTripper(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		log.entries = append(log.entries, upstreamCall{URL: r.URL.String(), Header: r.Header, Body: string(b)})
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	oldCompat, oldMessages := compatClient, messagesOnlyClient
	t.Cleanup(func() { compatClient, messagesOnlyClient = oldCompat, oldMessages })
	compatClient = &http.Client{Transport: rt}
	messagesOnlyClient = &http.Client{Transport: rt}
	return log
}

type upstreamCall struct {
	URL    string
	Header http.Header
	Body   string
}

type upstreamLog struct{ entries []upstreamCall }

func (l *upstreamLog) count() int { return len(l.entries) }

func (l *upstreamLog) last(t *testing.T) upstreamCall {
	t.Helper()
	if len(l.entries) == 0 {
		t.Fatal("fakeUpstream: no upstream call recorded")
	}
	return l.entries[len(l.entries)-1]
}

// jsonModel extracts the top-level "model" of a JSON body.
func jsonModel(t *testing.T, body string) string {
	t.Helper()
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("upstream body is not JSON: %s: %v", body, err)
	}
	return got.Model
}

// POST /v1/chat/completions is forwarded to the owning provider's
// /chat/completions: the alias is rewritten to the upstream model name, the
// client's User-Agent is carried over, and providerEndpoint accepts all three
// documented base-URL forms.
func TestChatRoutePassthrough(t *testing.T) {
	for _, tc := range []struct{ name, baseURL string }{
		{"bare base", "https://up.invalid"},
		{"v1 base", "https://up.invalid/v1"},
		{"full endpoint", "https://up.invalid/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &modelPrefs{providers: []customProvider{{
				Name: "vendor", Model: "gpt-5.6-terra", BaseURL: tc.baseURL, APIKey: "secret",
			}}}
			log := fakeUpstream(t, http.StatusOK, `{"id":"chat_1"}`)
			engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"vendor/gpt-5.6-terra","messages":[]}`))
			req.Header.Set("User-Agent", "test-client/1")
			req.Header.Set("x-opencode-session", "sess-1")
			req.Header.Set("X-Other", "dropped")
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK || w.Body.String() != `{"id":"chat_1"}` {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			if log.count() != 1 {
				t.Fatalf("upstream calls=%d, want 1", log.count())
			}
			call := log.last(t)
			if !strings.HasSuffix(call.URL, "/v1/chat/completions") || !strings.HasPrefix(call.URL, "https://up.invalid") {
				t.Fatalf("target=%s", call.URL)
			}
			if got := jsonModel(t, call.Body); got != "gpt-5.6-terra" {
				t.Fatalf("upstream model=%q want gpt-5.6-terra", got)
			}
			if got := call.Header.Get("Authorization"); got != "Bearer secret" {
				t.Fatalf("Authorization=%q", got)
			}
			if got := call.Header.Get("User-Agent"); got != "test-client/1" {
				t.Fatalf("User-Agent=%q", got)
			}
			if got := call.Header.Get("x-opencode-session"); got != "sess-1" {
				t.Fatalf("x-opencode-session=%q", got)
			}
			if call.Header.Get("X-Other") != "" {
				t.Fatalf("non-x-opencode header forwarded: %v", call.Header)
			}
		})
	}
}

// POST /v1/responses is forwarded untranslated to a responses_only provider,
// at providerEndpoint's three accepted base-URL forms.
func TestResponsesRoutePassthrough(t *testing.T) {
	for _, tc := range []struct{ name, baseURL, wantTarget string }{
		{"bare base", "https://zen.invalid", "https://zen.invalid/v1/responses"},
		{"v1 base", "https://zen.invalid/v1", "https://zen.invalid/v1/responses"},
		{"full endpoint", "https://zen.invalid/v1/responses", "https://zen.invalid/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &modelPrefs{providers: []customProvider{{
				Name: "opencode-zen", Model: "muse-spark-1.3", BaseURL: tc.baseURL, APIKey: "zen-key", ResponsesOnly: true,
			}}}
			log := fakeUpstream(t, http.StatusOK, `{"id":"resp_1"}`)
			engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"opencode-zen/muse-spark-1.3","input":"ping"}`))
			req.Header.Set("x-opencode-session", "sess-1")
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK || w.Body.String() != `{"id":"resp_1"}` {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			if log.count() != 1 {
				t.Fatalf("upstream calls=%d, want 1", log.count())
			}
			call := log.last(t)
			if call.URL != tc.wantTarget {
				t.Fatalf("target=%s want %s", call.URL, tc.wantTarget)
			}
			if got := jsonModel(t, call.Body); got != "muse-spark-1.3" {
				t.Fatalf("upstream model=%q want muse-spark-1.3", got)
			}
			if got := call.Header.Get("Authorization"); got != "Bearer zen-key" {
				t.Fatalf("Authorization=%q", got)
			}
			if got := call.Header.Get("x-opencode-session"); got != "sess-1" {
				t.Fatalf("x-opencode-session=%q", got)
			}
		})
	}
}

// POST /v1/messages for a messages_only provider is relayed byte-for-byte to
// the upstream's /v1/messages with Anthropic-style auth headers.
func TestMessagesRouteMessagesOnlyPassthrough(t *testing.T) {
	p := &modelPrefs{providers: []customProvider{{
		Name: "justworker", Model: "gpt-5.6-terra", BaseURL: "https://up.invalid/v1", APIKey: "mw-key", MessagesOnly: true,
	}}}
	log := fakeUpstream(t, http.StatusOK, `{"type":"message","content":[{"type":"text","text":"hi"}]}`)
	engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"justworker/gpt-5.6-terra","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-opencode-session", "sess-1")
	req.Header.Set("User-Agent", "test-client/1")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	if log.count() != 1 {
		t.Fatalf("upstream calls=%d, want 1", log.count())
	}
	call := log.last(t)
	if call.URL != "https://up.invalid/v1/messages" {
		t.Fatalf("target=%s", call.URL)
	}
	if got := jsonModel(t, call.Body); got != "gpt-5.6-terra" {
		t.Fatalf("upstream model=%q want gpt-5.6-terra", got)
	}
	if got := call.Header.Get("x-api-key"); got != "mw-key" {
		t.Fatalf("x-api-key=%q", got)
	}
	if got := call.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version=%q", got)
	}
	if got := call.Header.Get("User-Agent"); got != "curl/8.4.0" {
		t.Fatalf("User-Agent=%q want curl/8.4.0", got)
	}
	if got := call.Header.Get("x-opencode-session"); got != "sess-1" {
		t.Fatalf("x-opencode-session=%q", got)
	}
}

// POST /v1/messages for a responses_only provider goes through the
// Anthropic-to-Responses adapter: the upstream receives /v1/responses with the
// provider's model name, and the client receives an Anthropic-shaped reply.
func TestMessagesRouteResponsesAdapter(t *testing.T) {
	p := &modelPrefs{providers: []customProvider{{
		Name: "gateway", Model: "muse", BaseURL: "https://up.invalid/v1", APIKey: "secret", ResponsesOnly: true,
	}}}
	log := fakeUpstream(t, http.StatusOK, `{"id":"r","model":"muse","status":"completed","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"pong"}]}]}`)
	engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gateway/muse","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`)))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stop_reason"`) || !strings.Contains(w.Body.String(), `"text":"pong"`) {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	if log.count() != 1 {
		t.Fatalf("upstream calls=%d, want 1", log.count())
	}
	call := log.last(t)
	if call.URL != "https://up.invalid/v1/responses" {
		t.Fatalf("target=%s", call.URL)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(call.Body), &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent["model"] != "muse" {
		t.Fatalf("upstream model=%v want muse", sent["model"])
	}
	if sent["max_output_tokens"].(float64) != 8 {
		t.Fatalf("max_output_tokens=%v want 8", sent["max_output_tokens"])
	}
}

// Hidden custom models are rejected before any upstream call, per route.
func TestHiddenModelsRejected(t *testing.T) {
	tests := []struct {
		name  string
		route string
		body  string
		prov  customProvider
		want  string
	}{
		{"chat", "/v1/chat/completions", `{"model":"vendor/m"}`,
			customProvider{Name: "vendor", Model: "m"}, `"model is hidden"`},
		{"messages passthrough", "/v1/messages", `{"model":"vendor/m","messages":[{"role":"user","content":"hi"}]}`,
			customProvider{Name: "vendor", Model: "m", MessagesOnly: true}, `"model is hidden"`},
		{"messages adapter", "/v1/messages", `{"model":"vendor/m","messages":[{"role":"user","content":"hi"}]}`,
			customProvider{Name: "vendor", Model: "m", ResponsesOnly: true}, `"not_found_error"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &modelPrefs{
				hidden:    map[string]struct{}{"vendor/m": {}},
				providers: []customProvider{tc.prov},
			}
			log := fakeUpstream(t, http.StatusOK, `{}`)
			engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader(tc.body)))

			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("response=%d %s, want 404 containing %s", w.Code, w.Body.String(), tc.want)
			}
			if log.count() != 0 {
				t.Fatalf("hidden model reached upstream: %v", log.entries)
			}
		})
	}
}

// Models no custom provider owns fall through to CLIProxyAPI's own handlers.
func TestUnknownModelsFallThrough(t *testing.T) {
	p := &modelPrefs{providers: []customProvider{{Name: "vendor", Model: "m", ResponsesOnly: true}}}
	log := fakeUpstream(t, http.StatusOK, `{}`)
	engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
	for _, tc := range []struct{ route, body string }{
		{"/v1/chat/completions", `{"model":"vendor/other","messages":[]}`},
		{"/v1/messages", `{"model":"unknown/model","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"unknown/model","input":"ping"}`},
	} {
		t.Run(tc.route, func(t *testing.T) {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader(tc.body)))
			// echoStub answers with the request body, so an exact round-trip
			// is proof the stub — not an upstream or a middleware — answered.
			if w.Code != http.StatusOK || w.Body.String() != tc.body {
				t.Fatalf("response=%d %s, want the stub's round-trip of %s", w.Code, w.Body.String(), tc.body)
			}
		})
	}
	if log.count() != 0 {
		t.Fatalf("unknown model reached upstream: %v", log.entries)
	}
}

// Today customOpenAIResponsesPassthrough checks ownership, not visibility: a
// hidden responses_only model on /v1/responses is served by the downstream
// handler rather than rejected. Pinned because it is observable behavior the
// dispatcher must reproduce, not because it is desirable.
func TestResponsesRouteHiddenModelFallsThrough(t *testing.T) {
	p := &modelPrefs{
		hidden:    map[string]struct{}{"vendor/m": {}},
		providers: []customProvider{{Name: "vendor", Model: "m", BaseURL: "https://up.invalid/v1", ResponsesOnly: true}},
	}
	log := fakeUpstream(t, http.StatusOK, `{}`)
	engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"vendor/m","input":"ping"}`)))

	if w.Code != http.StatusOK || w.Body.String() != `{"model":"vendor/m","input":"ping"}` {
		t.Fatalf("response=%d %s, want the stub's round-trip", w.Code, w.Body.String())
	}
	if log.count() != 0 {
		t.Fatalf("hidden responses_only model reached upstream: %v", log.entries)
	}
}

// Only AgentRouter upstreams get the spoofed opencode client identity; every
// other upstream sees the caller's User-Agent.
func TestAgentRouterClientSpoofed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		wantUA  string
	}{
		{"agentrouter.org", "https://agentrouter.org/v1", "opencode/1.18.25"},
		{"other host", "https://up.invalid/v1", "test-client/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &modelPrefs{providers: []customProvider{{
				Name: "ar", Model: "agent", BaseURL: tc.baseURL, APIKey: "ar-key",
			}}}
			log := fakeUpstream(t, http.StatusOK, `{}`)
			engine := mountChain(t, p, echoStub(http.StatusOK, `{"fell":"through"}`))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ar/agent","messages":[]}`))
			req.Header.Set("User-Agent", "test-client/1")
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			call := log.last(t)
			if got := call.Header.Get("User-Agent"); got != tc.wantUA {
				t.Fatalf("User-Agent=%q want %q", got, tc.wantUA)
			}
			if got := call.Header.Get("X-App"); tc.name == "agentrouter.org" && got != "opencode" {
				t.Fatalf("X-App=%q want opencode", got)
			}
		})
	}
}

// "automode" on /v1/messages is rewritten to the configured target before the
// request reaches the downstream handler; a hidden target is a 404.
func TestAutomodeRewritten(t *testing.T) {
	t.Run("alias replaced", func(t *testing.T) {
		p := &modelPrefs{
			autoModel: "vendor/m",
			providers: []customProvider{{Name: "vendor", Model: "m"}},
		}
		engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"automode","messages":[{"role":"user","content":"hi"}]}`)))

		if w.Code != http.StatusOK {
			t.Fatalf("response=%d %s", w.Code, w.Body.String())
		}
		if got := jsonModel(t, w.Body.String()); got != "vendor/m" {
			t.Fatalf("downstream model=%q want vendor/m", got)
		}
	})
	t.Run("hidden target", func(t *testing.T) {
		p := &modelPrefs{
			autoModel: "vendor/m",
			hidden:    map[string]struct{}{"vendor/m": {}},
			providers: []customProvider{{Name: "vendor", Model: "m"}},
		}
		engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"automode","messages":[{"role":"user","content":"hi"}]}`)))

		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "automode target is hidden") {
			t.Fatalf("response=%d %s", w.Code, w.Body.String())
		}
	})
}

// The "cycle" model retries the next configured model before any retryable
// status (429/5xx) reaches the client, committing the first non-retryable
// answer without a second attempt.
func TestCycleRetriesAndCommits(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls []string
			stub := func(c *gin.Context) {
				var payload struct {
					Model string `json:"model"`
				}
				body, _ := io.ReadAll(c.Request.Body)
				_ = json.Unmarshal(body, &payload)
				calls = append(calls, payload.Model)
				switch payload.Model {
				case "first":
					c.JSON(status, gin.H{"error": "retry me"})
				case "second":
					c.JSON(http.StatusOK, gin.H{"model": "second"})
				default:
					c.String(http.StatusBadRequest, "stub does not know %q", payload.Model)
				}
			}
			p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
			engine := mountChain(t, p, stub)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"cycle","messages":[{"role":"user","content":"hi"}]}`)))

			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"second"`) {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			if fmt.Sprint(calls) != "[first second]" {
				t.Fatalf("attempts=%v", calls)
			}
			if _, index := p.cycleSnapshot(); index != 1 {
				t.Fatalf("cycle index=%d, want 1 advanced past the failed model", index)
			}
		})
	}
}

// A committed response — success or client error — is never retried and never
// advances the cycle.
func TestCycleDoesNotRetryCommittedResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls []string
			stub := func(c *gin.Context) {
				var payload struct {
					Model string `json:"model"`
				}
				body, _ := io.ReadAll(c.Request.Body)
				_ = json.Unmarshal(body, &payload)
				calls = append(calls, payload.Model)
				c.JSON(status, gin.H{"model": payload.Model})
			}
			p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
			engine := mountChain(t, p, stub)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"cycle","messages":[{"role":"user","content":"hi"}]}`)))

			if w.Code != status || !strings.Contains(w.Body.String(), `"model":"first"`) {
				t.Fatalf("response=%d %s, want status %d and first model", w.Code, w.Body.String(), status)
			}
			if fmt.Sprint(calls) != "[first]" {
				t.Fatalf("attempts=%v, want only first", calls)
			}
			if _, index := p.cycleSnapshot(); index != 0 {
				t.Fatalf("cycle index=%d, want 0 (no advance on a committed response)", index)
			}
		})
	}
}

// count_tokens probes only the first cycle model: its answer is committed even
// when it is retryable.
func TestCycleCountTokensTriesFirstModelOnly(t *testing.T) {
	var calls []string
	stub := func(c *gin.Context) {
		var payload struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(c.Request.Body)
		_ = json.Unmarshal(body, &payload)
		calls = append(calls, payload.Model)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "retry me"})
	}
	p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	engine := mountChain(t, p, stub)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"cycle","messages":[{"role":"user","content":"hi"}]}`)))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("response=%d %s, want the committed 429", w.Code, w.Body.String())
	}
	if fmt.Sprint(calls) != "[first]" {
		t.Fatalf("attempts=%v, want only first", calls)
	}
	if _, index := p.cycleSnapshot(); index != 0 {
		t.Fatalf("cycle index=%d, want 0", index)
	}
}

// Non-text tool_result content is wrapped into a JSON string before routing;
// content that downstream translators already accept is left alone.
func TestToolResultsSanitized(t *testing.T) {
	t.Run("wraps non-text parts", func(t *testing.T) {
		p := &modelPrefs{}
		engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"unknown/model","messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"c1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`)))

		var got struct {
			Messages []struct {
				Content []struct {
					Type    string          `json:"type"`
					Content json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(w.Body.String()), &got); err != nil {
			t.Fatalf("response=%d %s: %v", w.Code, w.Body.String(), err)
		}
		block := got.Messages[0].Content[0]
		if block.Type != "tool_result" {
			t.Fatalf("block type=%q", block.Type)
		}
		var asString string
		if err := json.Unmarshal(block.Content, &asString); err != nil {
			t.Fatalf("tool_result content is %s, not a JSON string", block.Content)
		}
		if !strings.Contains(asString, `"type":"image"`) {
			t.Fatalf("wrapped content lost the image block: %s", asString)
		}
	})
	t.Run("leaves plain content alone", func(t *testing.T) {
		p := &modelPrefs{}
		engine := mountChain(t, p, echoStub(http.StatusOK, `{}`))
		body := `{"model":"unknown/model","messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"c1","content":"string is fine"},
			{"type":"tool_result","tool_use_id":"c2","content":[{"type":"text","text":"plain"}]}]}]}`
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if got := w.Body.String(); got != body {
			t.Fatalf("body rewritten though no wrapping was needed:\n%s", got)
		}
	})
}
