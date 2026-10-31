package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSetCustomProviderHeadersSpoofsAgentRouterClient(t *testing.T) {
	src := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", http.NoBody)
	src.Header.Set("User-Agent", "test-client")
	dst := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://agentrouter.org/v1/chat/completions", http.NoBody)
	setCustomProviderHeaders(dst, src, &customProvider{BaseURL: "https://agentrouter.org/v1", APIKey: "secret"})
	if got := dst.Header.Get("User-Agent"); got != "opencode/1.18.25" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := dst.Header.Get("X-App"); got != "opencode" {
		t.Fatalf("X-App = %q", got)
	}
	if got := dst.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestRewriteJSONModelIgnoresFormatting(t *testing.T) {
	body, changed := rewriteJSONModel([]byte(`{"model":  "claude-sonnet-5", "max_tokens": 32}`), "claude-sonnet-5", "nvidia/default")
	if !changed || string(body) != `{"max_tokens":32,"model":"nvidia/default"}` {
		t.Fatalf("body=%s changed=%v", body, changed)
	}
}

func TestClaudeAutoModelAliasTracksSelectedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{}
	engine := gin.New()
	engine.Use(claudeAutoModelAlias(p))
	engine.POST("/v1/messages", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	request := func(model string) string {
		t.Helper()
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`"}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if got := request("automode"); got != `{"model":"nvidia/nemotron-3.5-lightning-30b-a3b"}` {
		t.Fatalf("default automode=%s", got)
	}
	if err := p.setAutoModel("z-ai/glm-5.2"); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"automode"} {
		if got := request(model); got != `{"model":"z-ai/glm-5.2"}` {
			t.Fatalf("%s routed to %s", model, got)
		}
	}
	if got := request("moonshotai/kimi-k3"); got != `{"model":"moonshotai/kimi-k3"}` {
		t.Fatalf("ordinary model changed: %s", got)
	}
	if got := request("claude-sonnet-5"); got != `{"model":"claude-sonnet-5"}` {
		t.Fatalf("ordinary Claude model changed: %s", got)
	}
}

func TestMessagesOnlyPassthroughStopsChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldClient := messagesOnlyClient
	t.Cleanup(func() { messagesOnlyClient = oldClient })
	messagesOnlyClient = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})}
	p := &modelPrefs{providers: []customProvider{{Name: "custom", Model: "m", BaseURL: "https://example.invalid", MessagesOnly: true}}}
	engine := gin.New()
	engine.Use(messagesOnlyPassthrough(p))
	called := false
	engine.POST("/v1/messages", func(c *gin.Context) { called = true; c.String(http.StatusOK, "second handler") })
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"custom/m"}`)))
	if called || w.Body.String() != `{"ok":true}` {
		t.Fatalf("called=%v body=%q", called, w.Body.String())
	}
}

func TestLookupCustomProviderRejectsHiddenAliases(t *testing.T) {
	p := &modelPrefs{hidden: map[string]struct{}{"custom/m": {}}, providers: []customProvider{{Name: "custom", Model: "m"}}}
	if got := lookupCustomProvider(p, "custom/m"); got != nil {
		t.Fatalf("hidden provider remained routable: %#v", got)
	}
}

func TestHiddenCustomModelStopsMiddlewareChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{hidden: map[string]struct{}{"custom/m": {}}, providers: []customProvider{{Name: "custom", Model: "m"}}}
	engine := gin.New()
	engine.Use(customOpenAICompatPassthrough(p))
	called := false
	engine.POST("/v1/chat/completions", func(c *gin.Context) { called = true; c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"custom/m"}`)))
	if called || w.Code != http.StatusNotFound {
		t.Fatalf("called=%v status=%d", called, w.Code)
	}
}

func TestResponsesPassthroughUsesResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldClient := compatClient
	t.Cleanup(func() { compatClient = oldClient })
	var got *http.Request
	compatClient = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1"}`)),
		}, nil
	})}
	p := &modelPrefs{providers: []customProvider{{
		Name: "opencode-zen", Model: "muse-spark-1.3", BaseURL: "https://opencode.ai/zen/v1",
		APIKey: "secret", ResponsesOnly: true,
	}}}
	engine := gin.New()
	engine.Use(customOpenAIResponsesPassthrough(p))
	called := false
	engine.POST("/v1/responses", func(c *gin.Context) { called = true; c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-zen/muse-spark-1.3","input":"ping"}`))
	req.Header.Set("x-opencode-session", "session-test-1")
	engine.ServeHTTP(w, req)
	if called || w.Code != http.StatusOK || got == nil {
		t.Fatalf("called=%v status=%d request=%v", called, w.Code, got)
	}
	if got.URL.String() != "https://opencode.ai/zen/v1/responses" {
		t.Fatalf("target=%s", got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("authorization=%q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("x-opencode-session") != "session-test-1" {
		t.Fatalf("x-opencode-session=%q", got.Header.Get("x-opencode-session"))
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"muse-spark-1.3"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestProviderEndpointAcceptsFullEndpoint(t *testing.T) {
	if got := providerEndpoint("https://opencode.ai/zen/v1/responses", "/responses"); got != "https://opencode.ai/zen/v1/responses" {
		t.Fatalf("full endpoint=%q", got)
	}
	if got := providerEndpoint("https://opencode.ai/zen/v1", "/responses"); got != "https://opencode.ai/zen/v1/responses" {
		t.Fatalf("base URL=%q", got)
	}
}
