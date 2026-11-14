package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// TestKeyAwareGatewayTokenRoutingDB is the end-to-end #202 contract against
// PostgreSQL: a gateway token authenticates to its user, aliases resolve from
// that user's keys only, a missing key is a 403 that names the provider, and
// stats carry the additive user_id/key_id.
func TestKeyAwareGatewayTokenRoutingDB(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed key-aware routing tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)

	password, err := loadPasswordHashConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("load password hash configuration: %v", err)
	}
	owner, err := auth.CreateFirstOwner(ctx, pool, auth.CreateOwnerInput{
		Username: "owner202", Email: "owner202@example.test", DisplayName: "Owner 202", Password: dbOwnerPassword,
	}, password)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	member, err := auth.CreateUser(ctx, pool, auth.CreateUserInput{
		Username: "member202", Email: "member202@example.test", DisplayName: "Member 202",
		Role: auth.RoleUser, PasswordHash: owner.PasswordHash,
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	nokey, err := auth.CreateUser(ctx, pool, auth.CreateUserInput{
		Username: "nokey202", Email: "nokey202@example.test", DisplayName: "No Key 202",
		Role: auth.RoleUser, PasswordHash: owner.PasswordHash,
	})
	if err != nil {
		t.Fatalf("create keyless user: %v", err)
	}
	gatewayToken := func(userID string) string {
		t.Helper()
		raw, _, err := auth.CreateAPIToken(ctx, pool, auth.CreateAPITokenInput{
			UserID: userID, Kind: auth.TokenKindGateway, Name: "issue-202",
			Expiry: auth.TokenExpiry{NoExpiry: true}, Now: time.Now(),
		})
		if err != nil {
			t.Fatalf("create gateway token: %v", err)
		}
		return raw
	}
	ownerToken, memberToken, nokeyToken := gatewayToken(owner.ID), gatewayToken(member.ID), gatewayToken(nokey.ID)

	var mu sync.Mutex
	var upstreamModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		upstreamModels = append(upstreamModels, payload.Model)
		mu.Unlock()
		if _, err := io.WriteString(w, `{"id":"up","usage":{"prompt_tokens":1,"completion_tokens":1}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	const (
		ownerKeyID  = "22222222-2222-2222-2222-222222222201"
		memberKeyID = "22222222-2222-2222-2222-222222222202"
		envKeyID    = "22222222-2222-2222-2222-222222222203"
	)
	if _, err := pool.Exec(ctx,
		`INSERT INTO providers(id, name, protocol, base_url)
		 VALUES('db202', 'DB 202', 'openai-chat', $1)`, upstream.URL+"/v1"); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	for _, key := range []struct {
		id, owner, scheme, env string
	}{
		{ownerKeyID, owner.ID, "none", ""},
		{memberKeyID, member.ID, "none", ""},
		{envKeyID, owner.ID, "bearer", "GNVIPI_KEY_SERVE_202_DB_UNSET"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO provider_keys(id, provider_id, owner_id, name, auth_scheme, key_env)
			 VALUES($1, 'db202', $2, 'issue-202', $3, NULLIF($4, ''))`,
			key.id, key.owner, key.scheme, key.env); err != nil {
			t.Fatalf("insert key %s: %v", key.id, err)
		}
	}
	for _, model := range []struct {
		keyID, owner, alias, upstream string
	}{
		{ownerKeyID, owner.ID, "db202-model", "owner-up"},
		{memberKeyID, member.ID, "db202-model", "member-up"},
		{envKeyID, owner.ID, "db202-unavailable", "unavailable-up"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO key_models(key_id, alias, upstream_model) VALUES($1, $2, $3)`,
			model.keyID, model.alias, model.upstream); err != nil {
			t.Fatalf("insert model %s: %v", model.alias, err)
		}
	}

	var auditEvents []providerstore.AuditEvent
	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), providerRuntimeOptions{
		Audit: func(event providerstore.AuditEvent) {
			mu.Lock()
			auditEvents = append(auditEvents, event)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	legacy, err := routing.NewLegacyResolver(nil, nil)
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	dispatcher := dispatch.New(&keyAwareResolver{runtime: runtime, legacy: legacy}, nil)
	state := &authState{pool: pool}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gatewayRequestContext(), gatewayAuth("", state), dispatcher.Handle)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusTeapot) })

	send := func(token, model string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, req)
		return out
	}
	lastUpstream := func() string {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(upstreamModels) == 0 {
			t.Fatal("upstream received no request")
		}
		return upstreamModels[len(upstreamModels)-1]
	}

	if out := send(ownerToken, "db202-model"); out.Code != http.StatusOK {
		t.Fatalf("owner request = %d: %s", out.Code, out.Body.String())
	}
	if got := lastUpstream(); got != "owner-up" {
		t.Fatalf("owner routed to %q, want owner-up", got)
	}
	if out := send(memberToken, "db202-model"); out.Code != http.StatusOK {
		t.Fatalf("member request = %d: %s", out.Code, out.Body.String())
	}
	if got := lastUpstream(); got != "member-up" {
		t.Fatalf("member routed to %q, want member-up", got)
	}

	// The keyless user must receive a 4xx naming the provider, never another
	// user's key and never a 5xx.
	if out := send(nokeyToken, "db202-model"); out.Code != http.StatusForbidden {
		t.Fatalf("keyless request = %d, want 403: %s", out.Code, out.Body.String())
	} else {
		for _, want := range []string{"db202", "db202-model", "add an enabled provider key"} {
			if !strings.Contains(out.Body.String(), want) {
				t.Fatalf("missing-key body %q does not mention %q", out.Body.String(), want)
			}
		}
	}

	// Additive stats attribution, written by the default recorder.
	var attributed bool
	for _, event := range nvidia.GlobalStats.Events(400) {
		if event.Model != "db202-model" || event.UserID != owner.ID {
			continue
		}
		if event.KeyID != ownerKeyID || event.ProviderID != "db202" {
			t.Fatalf("stats attribution = %+v, want key %s provider db202", event, ownerKeyID)
		}
		attributed = true
		break
	}
	if !attributed {
		t.Fatal("no stats event attributed to the owner's gateway token")
	}

	// Audit stays additive and identifiers-only: the unavailable credential is
	// a 503 and the event names the user and key.
	if out := send(ownerToken, "db202-unavailable"); out.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable credential = %d, want 503: %s", out.Code, out.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, event := range auditEvents {
		if event.Code != providerstore.AuditCredentialUnavailable {
			continue
		}
		if event.UserID != owner.ID || len(event.KeyIDs) != 1 || event.KeyIDs[0] != envKeyID {
			t.Fatalf("audit event = %+v, want user %s key %s", event, owner.ID, envKeyID)
		}
		return
	}
	t.Fatalf("no credential-unavailable audit event in %+v", auditEvents)
}

// TestGatewayAuthDBAcceptsGatewayToken proves a database gateway token is
// accepted even when static keys are configured and never echoes key material.
func TestGatewayAuthDBAcceptsGatewayToken(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed gateway auth tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)
	password, err := loadPasswordHashConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("load password hash configuration: %v", err)
	}
	owner, err := auth.CreateFirstOwner(ctx, pool, auth.CreateOwnerInput{
		Username: "auth202", Email: "auth202@example.test", DisplayName: "Auth 202", Password: dbOwnerPassword,
	}, password)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	token, _, err := auth.CreateAPIToken(ctx, pool, auth.CreateAPITokenInput{
		UserID: owner.ID, Kind: auth.TokenKindGateway, Name: "gateway",
		Expiry: auth.TokenExpiry{NoExpiry: true}, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("create gateway token: %v", err)
	}

	state := &authState{pool: pool}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gatewayRequestContext(), gatewayAuth("static-gateway-key", state))
	var gotUser string
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		if rc := core.RequestContextFrom(c.Request.Context()); rc != nil {
			gotUser = rc.UserID
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("database gateway token = %d, want 200: %s", out.Code, out.Body.String())
	}
	if gotUser != owner.ID {
		t.Fatalf("authenticated user = %q, want %q", gotUser, owner.ID)
	}

	// An invalid bearer token is still rejected when static keys are configured.
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer not-a-token")
	out = httptest.NewRecorder()
	engine.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d, want 401", out.Code)
	}
	if strings.Contains(out.Body.String(), token) || strings.Contains(out.Body.String(), "not-a-token") {
		t.Fatalf("auth error leaked key material: %s", out.Body.String())
	}
}
