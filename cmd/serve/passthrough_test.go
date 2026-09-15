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

func TestRewriteJSONModelIgnoresFormatting(t *testing.T) {
	body, changed := rewriteJSONModel([]byte(`{"model":  "claude-sonnet-5", "max_tokens": 32}`), "claude-sonnet-5", "nvidia/default")
	if !changed || string(body) != `{"max_tokens":32,"model":"nvidia/default"}` {
		t.Fatalf("body=%s changed=%v", body, changed)
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
