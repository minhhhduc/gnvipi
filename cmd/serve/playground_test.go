package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

type playgroundModelJSON struct {
	Provider      string `json:"provider"`
	Alias         string `json:"alias"`
	UpstreamModel string `json:"upstream_model"`
	Source        string `json:"source"`
}

type playgroundModelsJSON struct {
	Models []playgroundModelJSON `json:"models"`
	UserID string                `json:"user_id"`
	Union  bool                  `json:"union"`
}

func playgroundIdentity(id string, role auth.Role) *adminIdentity {
	return &adminIdentity{UserID: id, Username: id, Role: role}
}

// playgroundTestEngine mirrors the production middleware order: identity then
// the key-aware dispatcher, with the admin playground routes registered on the
// same engine so the chat handler can re-enter it without a network hop.
func playgroundTestEngine(t *testing.T, identity *adminIdentity, rows *providerstore.Rows) *gin.Engine {
	t.Helper()
	runtime := keyAwareTestRuntime(t, rows, providerRuntimeOptions{IsProtected: protectedModel})
	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	dispatcher := dispatch.New(routing.NewKeyAwareResolver(runtime.router, legacy), nil)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(requestBodyLimit(1<<20, 1<<20))
	engine.Use(gatewayRequestContext())
	engine.Use(func(c *gin.Context) {
		if identity != nil {
			c.Set(adminIdentityKey, identity)
		}
		c.Next()
	})
	engine.Use(gatewayAuth(""))
	engine.Use(dispatcher.Handle)
	registerAdminPlaygroundRoutes(engine, &authState{}, runtime, setupTestPrefs(t))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(599, gin.H{"error": "unhandled fallthrough"})
	})
	return engine
}

func playgroundRequest(t *testing.T, engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, req)
	return out
}

func writeTestBody(t *testing.T, w io.Writer, text string) {
	t.Helper()
	if _, err := io.WriteString(w, text); err != nil {
		t.Errorf("write test response: %v", err)
	}
}

func playgroundModels(t *testing.T, body string) playgroundModelsJSON {
	t.Helper()
	var out playgroundModelsJSON
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode models response %q: %v", body, err)
	}
	return out
}

func findPlaygroundModel(list []playgroundModelJSON, alias, upstream string) bool {
	for _, model := range list {
		if model.Provider == "p1" && model.Alias == alias && model.UpstreamModel == upstream {
			return true
		}
	}
	return false
}

func playgroundRows(baseURL string) providerstore.Rows {
	return providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider(baseURL)},
		Keys: []providerstore.KeyRow{
			{ID: "k-u1", ProviderID: "p1", OwnerID: "u1", Name: "one", Enabled: true, AuthScheme: providerstore.SchemeNone},
			{ID: "k-u2", ProviderID: "p1", OwnerID: "u2", Name: "two", Enabled: true, AuthScheme: providerstore.SchemeNone},
			{ID: "k-shared", ProviderID: "p1", OwnerID: "", Name: "shared", Enabled: true, AuthScheme: providerstore.SchemeNone},
		},
		KeyModels: []providerstore.KeyModelRow{
			{KeyID: "k-u1", ProviderID: "p1", OwnerID: "u1", Alias: "shared-alias", UpstreamModel: "up-u1"},
			{KeyID: "k-u2", ProviderID: "p1", OwnerID: "u2", Alias: "shared-alias", UpstreamModel: "up-u2"},
			{KeyID: "k-u1", ProviderID: "p1", OwnerID: "u1", Alias: "only-u1", UpstreamModel: "only-up-u1"},
			{KeyID: "k-shared", ProviderID: "p1", OwnerID: "", Alias: "shared-none", UpstreamModel: "up-shared"},
		},
	}
}

// TestPlaygroundModelsIsolatesUsers is the model-list matrix: personal keys
// never leak across accounts, shared none keys are visible to everyone, and an
// owner sees the union (or one selected account when asked).
func TestPlaygroundModelsIsolatesUsers(t *testing.T) {
	rows := playgroundRows("https://p1.example.com/v1")

	userOne := playgroundModels(t, playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u1", auth.RoleUser), &rows), http.MethodGet, "/admin/api/playground/models", "").Body.String())
	if userOne.UserID != "u1" || userOne.Union {
		t.Fatalf("user u1 target = %+v", userOne)
	}
	if !findPlaygroundModel(userOne.Models, "shared-alias", "up-u1") {
		t.Fatalf("u1 does not see its own shared-alias: %+v", userOne.Models)
	}
	if findPlaygroundModel(userOne.Models, "shared-alias", "up-u2") {
		t.Fatal("u1 sees u2's key")
	}
	if !findPlaygroundModel(userOne.Models, "shared-none", "up-shared") {
		t.Fatal("u1 does not see the shared none-key alias")
	}

	userTwo := playgroundModels(t, playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u2", auth.RoleUser), &rows), http.MethodGet, "/admin/api/playground/models", "").Body.String())
	if findPlaygroundModel(userTwo.Models, "shared-alias", "up-u1") {
		t.Fatal("u2 sees u1's key")
	}
	if !findPlaygroundModel(userTwo.Models, "shared-alias", "up-u2") {
		t.Fatal("u2 does not see its own alias")
	}

	owner := playgroundModels(t, playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("owner", auth.RoleOwner), &rows), http.MethodGet, "/admin/api/playground/models", "").Body.String())
	if !owner.Union {
		t.Fatal("owner union view is not marked as a union")
	}
	if !findPlaygroundModel(owner.Models, "shared-alias", "up-u1") || !findPlaygroundModel(owner.Models, "shared-alias", "up-u2") {
		t.Fatalf("owner union missed an account alias: %+v", owner.Models)
	}

	selected := playgroundModels(t, playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("owner", auth.RoleOwner), &rows), http.MethodGet, "/admin/api/playground/models?user_id=u2", "").Body.String())
	if selected.Union || selected.UserID != "u2" {
		t.Fatalf("owner selected view = %+v", selected)
	}
	if findPlaygroundModel(selected.Models, "shared-alias", "up-u1") {
		t.Fatal("owner selected u2 still sees u1's key")
	}

	// The shared NVIDIA Playground catalog is reachable by every identity.
	if len(models.Snapshot()) > 0 {
		found := false
		for _, model := range userOne.Models {
			if model.Source == "nvidia" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("no NVIDIA Playground model in the user picker")
		}
	}
}

// TestPlaygroundChatUsesCallerKey sends the same alias as two accounts and
// proves each hits its own upstream model and credential. The fake key is a
// canary: it must never appear in any console response.
func TestPlaygroundChatUsesCallerKey(t *testing.T) {
	const (
		u1Key = "fake-key-playground-u1-7f3c"
		u2Key = "fake-key-playground-u2-9a1d"
	)
	t.Setenv("GNVIPI_KEY_PLAYGROUND_U1", u1Key)
	t.Setenv("GNVIPI_KEY_PLAYGROUND_U2", u2Key)

	var mu sync.Mutex
	var seen []struct{ model, auth string }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		seen = append(seen, struct{ model, auth string }{payload.Model, r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		writeTestBody(t, w, `{"id":"up","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider(upstream.URL + "/v1")},
		Keys: []providerstore.KeyRow{
			{ID: "k-u1", ProviderID: "p1", OwnerID: "u1", Name: "one", Enabled: true, AuthScheme: providerstore.SchemeBearer, KeyEnv: "GNVIPI_KEY_PLAYGROUND_U1"},
			{ID: "k-u2", ProviderID: "p1", OwnerID: "u2", Name: "two", Enabled: true, AuthScheme: providerstore.SchemeBearer, KeyEnv: "GNVIPI_KEY_PLAYGROUND_U2"},
		},
		KeyModels: []providerstore.KeyModelRow{
			{KeyID: "k-u1", ProviderID: "p1", OwnerID: "u1", Alias: "shared-alias", UpstreamModel: "up-u1"},
			{KeyID: "k-u2", ProviderID: "p1", OwnerID: "u2", Alias: "shared-alias", UpstreamModel: "up-u2"},
		},
	}
	body := `{"model":"shared-alias","messages":[{"role":"user","content":"hi"}]}`

	first := playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u1", auth.RoleUser), &rows), http.MethodPost, "/admin/api/playground/chat", body)
	if first.Code != http.StatusOK {
		t.Fatalf("u1 chat = %d %s", first.Code, first.Body.String())
	}
	second := playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u2", auth.RoleUser), &rows), http.MethodPost, "/admin/api/playground/chat", body)
	if second.Code != http.StatusOK {
		t.Fatalf("u2 chat = %d %s", second.Code, second.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("upstream calls = %+v", seen)
	}
	if seen[0].model != "up-u1" || seen[1].model != "up-u2" {
		t.Fatalf("upstream models = %+v, want up-u1 then up-u2", seen)
	}
	if seen[0].auth != "Bearer "+u1Key || seen[1].auth != "Bearer "+u2Key {
		t.Fatal("the caller's own key was not used upstream")
	}
	for _, response := range []string{first.Body.String(), second.Body.String()} {
		if strings.Contains(response, u1Key) || strings.Contains(response, u2Key) {
			t.Fatalf("credential leaked into the console response: %s", response)
		}
	}
}

// TestPlaygroundChatMissingKeyIsTyped covers the provider-named 4xx.
func TestPlaygroundChatMissingKeyIsTyped(t *testing.T) {
	rows := playgroundRows("https://p1.example.com/v1")
	engine := playgroundTestEngine(t, playgroundIdentity("u3", auth.RoleUser), &rows)
	res := playgroundRequest(t, engine, http.MethodPost, "/admin/api/playground/chat", `{"model":"only-u1","messages":[]}`)
	if res.Code != http.StatusForbidden {
		t.Fatalf("missing key = %d %s, want 403", res.Code, res.Body.String())
	}
	for _, want := range []string{"p1", "only-u1", "add an enabled provider key"} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("missing-key body %q does not mention %q", res.Body.String(), want)
		}
	}
}

// TestPlaygroundCredentialNeverListed is the redaction canary: a key that
// resolves at request time must not appear in the picker payload.
func TestPlaygroundCredentialNeverListed(t *testing.T) {
	const canary = "fake-key-playground-canary-4b7e"
	t.Setenv("GNVIPI_KEY_PLAYGROUND_CANARY", canary)
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider("https://p1.example.com/v1")},
		Keys: []providerstore.KeyRow{
			{ID: "k-canary", ProviderID: "p1", OwnerID: "u1", Name: "canary", Enabled: true, AuthScheme: providerstore.SchemeBearer, KeyEnv: "GNVIPI_KEY_PLAYGROUND_CANARY"},
		},
		KeyModels: []providerstore.KeyModelRow{
			{KeyID: "k-canary", ProviderID: "p1", OwnerID: "u1", Alias: "canary-model", UpstreamModel: "up-canary"},
		},
	}
	res := playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u1", auth.RoleUser), &rows), http.MethodGet, "/admin/api/playground/models", "")
	if res.Code != http.StatusOK {
		t.Fatalf("models = %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), canary) {
		t.Fatal("credential material appeared in the model picker")
	}
}

// TestPlaygroundForeignUserIDIs404 keeps non-owners confined to their own id.
func TestPlaygroundForeignUserIDIs404(t *testing.T) {
	rows := playgroundRows("https://p1.example.com/v1")
	engine := playgroundTestEngine(t, playgroundIdentity("u2", auth.RoleUser), &rows)
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/admin/api/playground/models?user_id=u1", ""},
		{http.MethodPost, "/admin/api/playground/chat?user_id=u1", `{"model":"shared-alias","messages":[]}`},
	} {
		res := playgroundRequest(t, engine, request.method, request.path, request.body)
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d %s, want 404", request.method, request.path, res.Code, res.Body.String())
		}
	}
}

// TestPlaygroundChatStreams verifies incremental chunks reach the console.
func TestPlaygroundChatStreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeTestBody(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		flusher.Flush()
		writeTestBody(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		flusher.Flush()
		writeTestBody(t, w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider(upstream.URL + "/v1")},
		Keys:      []providerstore.KeyRow{{ID: "k-stream", ProviderID: "p1", OwnerID: "u1", Name: "stream", Enabled: true, AuthScheme: providerstore.SchemeNone}},
		KeyModels: []providerstore.KeyModelRow{{KeyID: "k-stream", ProviderID: "p1", OwnerID: "u1", Alias: "stream-model", UpstreamModel: "up-stream"}},
	}
	res := playgroundRequest(t, playgroundTestEngine(t, playgroundIdentity("u1", auth.RoleUser), &rows), http.MethodPost, "/admin/api/playground/chat",
		`{"model":"stream-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("stream = %d %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{"hel", "lo", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body %q missing %q", body, want)
		}
	}
}

// TestPlaygroundStopCancelsUpstream proves the stop button is wired end to end:
// aborting the request context cancels the relayed upstream call.
func TestPlaygroundStopCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeTestBody(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		once.Do(func() { close(started) })
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	rows := providerstore.Rows{
		Providers: []providerstore.ProviderRow{keyAwareTestProvider(upstream.URL + "/v1")},
		Keys:      []providerstore.KeyRow{{ID: "k-stop", ProviderID: "p1", OwnerID: "u1", Name: "stop", Enabled: true, AuthScheme: providerstore.SchemeNone}},
		KeyModels: []providerstore.KeyModelRow{{KeyID: "k-stop", ProviderID: "p1", OwnerID: "u1", Alias: "stop-model", UpstreamModel: "up-stop"}},
	}
	engine := playgroundTestEngine(t, playgroundIdentity("u1", auth.RoleUser), &rows)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/api/playground/chat",
		strings.NewReader(`{"model":"stop-model","messages":[],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was not reached")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the console request did not cancel the upstream call")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("playground handler did not return after stop")
	}
}
