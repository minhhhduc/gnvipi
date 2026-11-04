package dispatch

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func init() { gin.SetMode(gin.TestMode) }

func TestSameProtocolDispatchAndCountTokens(t *testing.T) {
	for _, tc := range []struct {
		path                string
		messages, responses bool
	}{
		{"/v1/chat/completions", false, false},
		{"/v1/messages", true, false},
		{"/v1/messages/count_tokens", true, false},
		{"/v1/responses", false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			requests := make(chan *http.Request, 1)
			bodies := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- r.Clone(r.Context())
				bodies <- body
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, " {\"answer\":\"unchanged\"}\n"); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{{Name: "vendor", BaseURL: upstream.URL, APIKey: "provider-key", Model: "upstream-model", Alias: "alias", MessagesOnly: tc.messages, ResponsesOnly: tc.responses}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			engine := gin.New()
			engine.Use(New(resolver, nil).Handle)
			engine.POST(tc.path, func(c *gin.Context) { t.Error("owned request reached downstream"); c.Status(http.StatusTeapot) })
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(`{"model":"alias","input":"hello","vendor":9007199254740993}`))
			request.Header.Set("Authorization", "Bearer inbound-key")
			request.Header.Set("X-Opencode-Session", "session-1")
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, request)
			if out.Code != http.StatusOK || out.Body.String() != " {\"answer\":\"unchanged\"}\n" {
				t.Fatalf("response = %d %q", out.Code, out.Body.String())
			}
			observed, body := <-requests, <-bodies
			authHeader, authValue := "Authorization", "Bearer provider-key"
			if tc.messages {
				authHeader, authValue = "X-Api-Key", "provider-key"
			}
			if observed.URL.Path != tc.path || observed.Header.Get(authHeader) != authValue || observed.Header.Get("X-Opencode-Session") != "session-1" {
				t.Fatalf("upstream request = %s %v", observed.URL.Path, observed.Header)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["model"]) != `"upstream-model"` || string(fields["vendor"]) != "9007199254740993" {
				t.Fatalf("routing changed opaque payload: %s", body)
			}
		})
	}
}

func TestNvidiaAndCrossProtocolFallthroughKeepOriginalBody(t *testing.T) {
	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{{Name: "vendor", BaseURL: "https://upstream.invalid/v1", Model: "custom", Alias: "custom", MessagesOnly: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{` { "model" : "nvidia/nemotron", "vendor" : 9007199254740993 } `, ` { "model" : "custom", "input" : "legacy translation" } `} {
		engine := gin.New()
		engine.Use(New(resolver, nil).Handle)
		engine.POST("/v1/chat/completions", func(c *gin.Context) {
			got, err := io.ReadAll(c.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			c.Data(http.StatusOK, "application/json", got)
		})
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if out.Code != http.StatusOK || out.Body.String() != body {
			t.Fatalf("fallthrough was intercepted: %d %q", out.Code, out.Body.String())
		}
	}
}

func TestHiddenModelsUseInboundErrorEnvelope(t *testing.T) {
	resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{{Name: "vendor", BaseURL: "https://upstream.invalid/v1", Model: "custom", Alias: "hidden"}}, []string{"hidden"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		engine := gin.New()
		engine.Use(New(resolver, nil).Handle)
		engine.POST(path, func(c *gin.Context) { t.Error("hidden request reached downstream") })
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"model":"hidden"}`)))
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if out.Code != http.StatusNotFound || len(envelope.Error) == 0 || envelope.Error[0] != '{' {
			t.Fatalf("hidden-model response = %d %s", out.Code, out.Body.String())
		}
	}
}

func TestBodyLimitCannotBeBypassedByDispatcherBuffer(t *testing.T) {
	resolver, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 12) }, New(resolver, nil).Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("oversized request reached downstream") })
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nvidia/nemotron"}`)))
	if out.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized response = %d %s", out.Code, out.Body.String())
	}
}

func TestProviderReloadCannotReuseOldRelayCredentials(t *testing.T) {
	headers := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	providers := []provider.LegacyProvider{{Name: "vendor", BaseURL: upstream.URL, Model: "model", Alias: "alias", APIKey: "first-key"}}
	resolver, err := routing.NewLegacyResolver(providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := New(resolver, nil)
	engine := gin.New()
	engine.Use(dispatcher.Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("custom request reached downstream") })
	for _, key := range []string{"first-key", "second-key"} {
		providers[0].APIKey = key
		if err := resolver.Update(providers, nil); err != nil {
			t.Fatal(err)
		}
		dispatcher.Reset()
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"alias"}`)))
		if out.Code != http.StatusOK || <-headers != "Bearer "+key {
			t.Fatalf("reloaded credential was not used: %d %s", out.Code, out.Body.String())
		}
	}
}
