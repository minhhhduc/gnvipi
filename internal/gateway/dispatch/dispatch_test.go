package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
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
			resolver, err := routing.NewLegacyResolver([]provider.LegacyProvider{{
				Name: "vendor", BaseURL: upstream.URL, APIKey: "provider-key", Model: "upstream-model", Alias: "alias",
				MessagesOnly: tc.messages, ResponsesOnly: tc.responses, ForwardHeaders: "X-Session",
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			engine := gin.New()
			engine.Use(New(resolver, nil).Handle)
			engine.POST(tc.path, func(c *gin.Context) { t.Error("owned request reached downstream"); c.Status(http.StatusTeapot) })
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(`{"model":"alias","input":"hello","vendor":9007199254740993}`))
			request.Header.Set("Authorization", "Bearer inbound-key")
			request.Header.Set("X-Session", "session-1")
			request.Header.Set("X-Unconfigured", "dropped")
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
			if observed.URL.Path != tc.path || observed.Header.Get(authHeader) != authValue || observed.Header.Get("X-Session") != "session-1" || observed.Header.Get("X-Unconfigured") != "" {
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

type materializingResolver struct {
	base  provider.Provider
	calls atomic.Int32
	keys  []struct {
		id     string
		secret string
	}
}

func (r *materializingResolver) Resolve(_ context.Context, model string, _ ir.Protocol) (routing.Route, error) {
	// Simulate providerstore.Router.materialize cloning the provider on every call.
	cloned := provider.Clone(&r.base)
	var keyID string
	if len(r.keys) > 0 {
		idx := int(r.calls.Add(1)-1) % len(r.keys)
		keyID = r.keys[idx].id
		cloned.Auth = provider.Auth{Scheme: provider.AuthBearer, Key: r.keys[idx].secret}
	}
	return routing.Route{
		Kind:             routing.Custom,
		Provider:         &cloned,
		UpstreamModel:    "upstream-" + model,
		UpstreamProtocol: cloned.Protocol,
		Alias:            model,
		KeyID:            keyID,
	}, nil
}

func (r *materializingResolver) ResolveCandidates(ctx context.Context, model string, protocol ir.Protocol) ([]routing.Route, error) {
	route, err := r.Resolve(ctx, model, protocol)
	if err != nil {
		return nil, err
	}
	return []routing.Route{route}, nil
}

func (r *materializingResolver) AdvanceCycle(string) {}

func TestDispatcherReusesHandlersAndClientsAcrossMaterializedRoutes(t *testing.T) {
	var newConns, closedConns atomic.Int32
	var authHeaders []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"ok":true}`); err != nil {
			t.Error(err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			newConns.Add(1)
		case http.StateClosed:
			closedConns.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	factory := upstream.NewFactory()
	defer factory.CloseIdleConnections()

	resolver := &materializingResolver{
		base: provider.Provider{
			ID:       "db-provider",
			Name:     "db-provider",
			BaseURL:  server.URL,
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  true,
		},
		keys: []struct {
			id     string
			secret string
		}{
			{id: "key-1", secret: "sk-1"},
			{id: "key-2", secret: "sk-2"},
		},
	}

	d := New(resolver, &relay.Options{Factory: factory})

	// 1. Freshly materialized routes for the same (model, key) reuse the same *relay.Relay.
	route1a, err := resolver.Resolve(t.Context(), "alias-a", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	route2, err := resolver.Resolve(t.Context(), "alias-a", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	route1b, err := resolver.Resolve(t.Context(), "alias-a", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	if route1a.Provider == route1b.Provider {
		t.Fatal("test setup must produce distinct Provider pointers per Resolve")
	}

	h1a, err := d.handler("alias-a", &route1a)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := d.handler("alias-a", &route2)
	if err != nil {
		t.Fatal(err)
	}
	h1b, err := d.handler("alias-a", &route1b)
	if err != nil {
		t.Fatal(err)
	}
	if h1a != h1b {
		t.Fatal("expected *relay.Relay reuse for freshly materialized route with same key")
	}
	if h1a == h2 {
		t.Fatal("expected distinct *relay.Relay for different key credentials")
	}

	// 2. Freshly materialized routes for cross-protocol translation reuse the same *translation.
	outAdapter := outboundanthropic.New()
	t1a, err := d.translator("alias-a", &route1a, outAdapter)
	if err != nil {
		t.Fatal(err)
	}
	t1b, err := d.translator("alias-a", &route1b, outAdapter)
	if err != nil {
		t.Fatal(err)
	}
	if t1a != t1b {
		t.Fatal("expected *translation reuse for freshly materialized route with same key")
	}

	// 3. End-to-end requests across alternating keys and models reuse the single TCP connection.
	engine := gin.New()
	engine.Use(d.Handle)
	for _, model := range []string{"alias-a", "alias-b", "alias-a", "alias-b"} {
		out := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+model+`"}`))
		engine.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("dispatch %s = %d %s", model, out.Code, out.Body.String())
		}
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("new TCP connections = %d, want 1 (transport/idle conn must be reused across keys and models)", got)
	}
	if got := closedConns.Load(); got != 0 {
		t.Fatalf("closed TCP connections = %d, want 0 (CloseIdleConnections must not be called)", got)
	}
	if len(authHeaders) != 4 || authHeaders[0] == authHeaders[1] {
		t.Fatalf("expected alternating key credentials, got %v", authHeaders)
	}
}
