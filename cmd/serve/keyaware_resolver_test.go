package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

func keyAwareTestProvider(baseURL string) providerstore.ProviderRow {
	return providerstore.ProviderRow{
		ID: "p1", Name: "p1", Protocol: "openai-chat", BaseURL: baseURL, Enabled: true, TLS: "default",
	}
}

func keyAwareTestRuntime(t *testing.T, rows *providerstore.Rows, opts providerRuntimeOptions) *providerRuntime {
	t.Helper()
	opts.Logf = func(string, ...any) {}
	runtime, err := openProviderRuntime(t.Context(), &stubSnapshotSource{rows: *rows, version: 1}, opts)
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)
	return runtime
}

func keyAwareTestContext(userID string) context.Context {
	rc := core.NewRequestContext("keyaware-test")
	rc.UserID = userID
	return core.WithRequestContext(context.Background(), rc)
}

func TestKeyAwareResolverRoutesByUser(t *testing.T) {
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider("https://p1.example.com/v1")},
		Keys: []providerstore.KeyRow{
			{ID: "k-u1", ProviderID: "p1", OwnerID: "u1", Name: "one", Enabled: true, AuthScheme: providerstore.SchemeNone},
			{ID: "k-u2", ProviderID: "p1", OwnerID: "u2", Name: "two", Enabled: true, AuthScheme: providerstore.SchemeNone},
		},
		KeyModels: []providerstore.KeyModelRow{
			{KeyID: "k-u1", ProviderID: "p1", OwnerID: "u1", Alias: "shared-alias", UpstreamModel: "up-u1"},
			{KeyID: "k-u2", ProviderID: "p1", OwnerID: "u2", Alias: "shared-alias", UpstreamModel: "up-u2"},
		},
	}
	runtime := keyAwareTestRuntime(t, &rows, providerRuntimeOptions{})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	resolver := &keyAwareResolver{runtime: runtime, legacy: legacy}

	route, err := resolver.Resolve(keyAwareTestContext("u1"), "shared-alias", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("u1 resolve: %v", err)
	}
	if route.KeyID != "k-u1" || route.UpstreamModel != "up-u1" || route.Kind != routing.Custom {
		t.Fatalf("u1 route = %+v", route)
	}
	route, err = resolver.Resolve(keyAwareTestContext("u2"), "shared-alias", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("u2 resolve: %v", err)
	}
	if route.KeyID != "k-u2" || route.UpstreamModel != "up-u2" {
		t.Fatalf("u2 route = %+v", route)
	}

	// A user with no key must never borrow another user's key.
	_, err = resolver.Resolve(keyAwareTestContext("u3"), "shared-alias", ir.ProtocolOpenAIChat)
	var normalized *gwerr.Error
	if !errors.As(err, &normalized) || normalized.Status != http.StatusForbidden {
		t.Fatalf("cross-user resolve error = %v, want 403", err)
	}
	for _, want := range []string{"p1", "shared-alias", "add an enabled provider key"} {
		if !strings.Contains(normalized.Message, want) {
			t.Fatalf("missing-key message %q does not mention %q", normalized.Message, want)
		}
	}
}

func TestKeyAwareResolverFallsBackWithoutUser(t *testing.T) {
	runtime := keyAwareTestRuntime(t, &providerstore.Rows{}, providerRuntimeOptions{})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	resolver := &keyAwareResolver{runtime: runtime, legacy: legacy}
	route, err := resolver.Resolve(context.Background(), "nvidia/model", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.Kind != routing.Fallthrough || route.UpstreamModel != "nvidia/model" {
		t.Fatalf("route = %+v, want legacy fallthrough", route)
	}
}

func TestKeyAwareResolverCredentialSources(t *testing.T) {
	keyBytes := bytes.Repeat([]byte{0x11}, providerstore.MasterKeySize)
	ring, err := providerstore.NewKeyring(1, keyBytes, nil)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	secret, nonce, kver, err := ring.Encrypt([]byte("stored-secret"), "k-secret", "u1", providerstore.SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	t.Setenv("GNVIPI_KEY_SERVE_202_ENV", "env-secret")
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider("https://p1.example.com/v1")},
		Keys: []providerstore.KeyRow{
			{ID: "k-secret", ProviderID: "p1", OwnerID: "u1", Name: "secret", Enabled: true, AuthScheme: providerstore.SchemeBearer,
				SecretEnc: secret, SecretNonce: nonce, SecretKVer: kver, SecretLast4: providerstore.SecretLast4([]byte("stored-secret"), providerstore.SchemeBearer)},
			{ID: "k-env", ProviderID: "p1", OwnerID: "u1", Name: "env", Enabled: true, AuthScheme: providerstore.SchemeBearer, KeyEnv: "GNVIPI_KEY_SERVE_202_ENV"},
			{ID: "k-ref", ProviderID: "p1", OwnerID: "u1", Name: "ref", Enabled: true, AuthScheme: providerstore.SchemeBearer, KeyRef: "import:serve"},
		},
		KeyModels: []providerstore.KeyModelRow{
			{KeyID: "k-secret", ProviderID: "p1", OwnerID: "u1", Alias: "secret-model", UpstreamModel: "up"},
			{KeyID: "k-env", ProviderID: "p1", OwnerID: "u1", Alias: "env-model", UpstreamModel: "up"},
			{KeyID: "k-ref", ProviderID: "p1", OwnerID: "u1", Alias: "ref-model", UpstreamModel: "up"},
		},
	}
	runtime := keyAwareTestRuntime(t, &rows, providerRuntimeOptions{
		Keyring: ring,
		Refs:    providerstore.MapRefResolver{"import:serve": "ref-secret"},
	})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	resolver := &keyAwareResolver{runtime: runtime, legacy: legacy}
	for alias, want := range map[string]string{"secret-model": "stored-secret", "env-model": "env-secret", "ref-model": "ref-secret"} {
		route, err := resolver.Resolve(keyAwareTestContext("u1"), alias, ir.ProtocolOpenAIChat)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if route.Provider.Auth.Key != want {
			t.Fatalf("%s: auth key = %q, want %q", alias, route.Provider.Auth.Key, want)
		}
	}
}

// TestKeyAwareDispatchAttributesUserAndKey proves the live path: a user-scoped
// route reaches the upstream and stats carry the request user and key.
func TestKeyAwareDispatchAttributesUserAndKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":2}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider(upstream.URL + "/v1")},
		Keys:      []providerstore.KeyRow{{ID: "k-attr", ProviderID: "p1", OwnerID: "u1", Name: "k", Enabled: true, AuthScheme: providerstore.SchemeNone}},
		KeyModels: []providerstore.KeyModelRow{{KeyID: "k-attr", ProviderID: "p1", OwnerID: "u1", Alias: "attributed-model", UpstreamModel: "up-attributed"}},
	}
	runtime := keyAwareTestRuntime(t, &rows, providerRuntimeOptions{})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	dispatcher := dispatch.New(&keyAwareResolver{runtime: runtime, legacy: legacy}, nil)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gatewayRequestContext())
	engine.Use(func(c *gin.Context) {
		if rc := core.RequestContextFrom(c.Request.Context()); rc != nil {
			rc.UserID = "u1"
		}
		c.Next()
	})
	engine.Use(dispatcher.Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusTeapot) })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"attributed-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", out.Code, out.Body.String())
	}

	for _, event := range nvidia.GlobalStats.Events(200) {
		if event.Model != "attributed-model" {
			continue
		}
		if event.UserID != "u1" || event.KeyID != "k-attr" || event.ProviderID != "p1" {
			t.Fatalf("stats attribution = %+v, want user u1 key k-attr provider p1", event)
		}
		return
	}
	t.Fatal("no stats event for attributed-model")
}

func TestKeyAwareResolverUnknownModelIs404(t *testing.T) {
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider("https://p1.example.com/v1")},
		Keys:      []providerstore.KeyRow{{ID: "k", ProviderID: "p1", OwnerID: "u1", Name: "k", Enabled: true, AuthScheme: providerstore.SchemeNone}},
		KeyModels: []providerstore.KeyModelRow{{KeyID: "k", ProviderID: "p1", OwnerID: "u1", Alias: "m", UpstreamModel: "up"}},
	}
	runtime := keyAwareTestRuntime(t, &rows, providerRuntimeOptions{})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	_, err = (&keyAwareResolver{runtime: runtime, legacy: legacy}).Resolve(keyAwareTestContext("u1"), "unknown", ir.ProtocolOpenAIChat)
	var normalized *gwerr.Error
	if !errors.As(err, &normalized) || normalized.Status != http.StatusNotFound {
		t.Fatalf("unknown model error = %v, want 404", err)
	}
}
