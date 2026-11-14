package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/migrations"
)

const (
	permissionOwner uint8 = 1 << iota
	permissionAdmin
	permissionUser
	permissionAll = permissionOwner | permissionAdmin | permissionUser
)

func currentAdminRouteMatrix() map[string]uint8 {
	operator := permissionOwner | permissionAdmin
	return map[string]uint8{
		"POST " + adminLoginPath:    permissionAll,
		"POST " + adminLogoutPath:   permissionAll,
		"GET " + adminSessionPath:   permissionAll,
		"POST " + adminPasswordPath: permissionAll,

		"GET /admin/api/users":               permissionOwner,
		"POST /admin/api/users":              permissionOwner,
		"GET /admin/api/users/:id":           permissionAll,
		"PUT /admin/api/users/:id":           permissionOwner,
		"DELETE /admin/api/users/:id":        permissionOwner,
		"POST /admin/api/users/:id/password": permissionAll,
		"POST /admin/api/users/:id/revoke":   permissionAll,

		"GET /admin/api/tokens":        permissionAll,
		"POST /admin/api/tokens":       permissionAll,
		"DELETE /admin/api/tokens/:id": permissionAll,
		"GET /admin/api/audit":         permissionOwner,

		"GET /admin/api/providers":           permissionAll,
		"POST /admin/api/providers":          operator,
		"POST /admin/api/providers/test":     operator,
		"GET /admin/api/providers/:id":       permissionAll,
		"PUT /admin/api/providers/:id":       operator,
		"DELETE /admin/api/providers/:id":    operator,
		"POST /admin/api/providers/:id/test": operator,

		"GET /admin/api/providers/:id/keys":               permissionAll,
		"POST /admin/api/providers/:id/keys":              permissionAll,
		"GET /admin/api/providers/:id/keys/:keyID":        permissionAll,
		"PUT /admin/api/providers/:id/keys/:keyID":        permissionAll,
		"DELETE /admin/api/providers/:id/keys/:keyID":     permissionAll,
		"POST /admin/api/providers/:id/keys/:keyID/test":  permissionAll,
		"GET /admin/api/providers/:id/keys/:keyID/models": permissionAll,
		"PUT /admin/api/providers/:id/keys/:keyID/models": permissionAll,
		"GET /admin/api/stats":                            operator,
		"GET /admin/api/models":                           operator,
		"POST /admin/api/models":                          operator,
		"DELETE /admin/api/models/*id":                    operator,
		"POST /admin/api/models/*id":                      operator,
		"GET /admin/api/config/automode":                  operator,
		"POST /admin/api/config/automode":                 operator,
		"GET /admin/api/config/cycle":                     operator,
		"POST /admin/api/config/cycle":                    operator,
		"GET /admin/api/config/search":                    operator,
		"POST /admin/api/config/search":                   operator,
		"GET /admin/api/chromes":                          operator,
		"POST /admin/api/chromes":                         operator,
	}
}

func concreteAdminRoute(pattern, subjectID string) string {
	path := strings.ReplaceAll(pattern, ":id", subjectID)
	return strings.ReplaceAll(path, "*id", "vendor/model")
}

func TestDBAdminPermission_CurrentRouteRoleMatrix(t *testing.T) {
	h := newDBAdminHarness(t)

	adminID := createContractUser(t, h, "matrix-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminToken, _ := createContractAdminToken(t, h, adminID, bearer(h.token))
	userID := createContractUser(t, h, "matrix-user", auth.RoleUser)
	clearMustChangePassword(t, h, userID)
	userSession, userCSRF := h.login(t, "matrix-user", dbUserPassword)

	type actor struct {
		name      string
		bit       uint8
		subjectID string
		headers   map[string]string
	}
	actors := []actor{
		{name: "owner", bit: permissionOwner, subjectID: h.owner.ID, headers: bearer(h.token)},
		{name: "admin", bit: permissionAdmin, subjectID: adminID, headers: bearer(adminToken)},
		{name: "user", bit: permissionUser, subjectID: userID, headers: cookieAuth(userSession, userCSRF)},
	}

	registered := make(map[string]gin.RouteInfo)
	for _, route := range h.engine.Routes() {
		if strings.HasPrefix(route.Path, "/admin/api/") {
			registered[route.Method+" "+route.Path] = route
		}
	}
	expected := currentAdminRouteMatrix()
	for key := range registered {
		if _, ok := expected[key]; !ok {
			t.Errorf("registered admin route %q is missing from the role-matrix contract", key)
		}
	}
	for key := range expected {
		if _, ok := registered[key]; !ok {
			t.Errorf("role-matrix contract names unregistered admin route %q", key)
		}
	}
	if t.Failed() {
		return
	}

	keys := make([]string, 0, len(registered))
	for key := range registered {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		route := registered[key]
		for _, actor := range actors {
			t.Run(key+"/"+actor.name, func(t *testing.T) {
				engine := gin.New()
				engine.Use(adminAPIAuthorization(h.state))
				engine.Handle(route.Method, route.Path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
				res := makeRequest(t, engine, route.Method, concreteAdminRoute(route.Path, actor.subjectID), nil, actor.headers)
				if expected[key]&actor.bit != 0 {
					if res.Code != http.StatusNoContent {
						t.Fatalf("status = %d, want 204 (body %s)", res.Code, res.Body.String())
					}
					return
				}
				if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "forbidden" {
					t.Fatalf("status = %d body %s, want 403 forbidden", res.Code, res.Body.String())
				}
			})
		}
	}
}

func TestAdminPermission_UserProviderReadsHideDisabledProviders(t *testing.T) {
	const (
		providerSecret     = "provider-secret-opaque"
		proxySecret        = "proxy-secret-opaque"
		headerSecret       = "header-secret-opaque"
		bodySecret         = "body-secret-opaque"
		extensionSecret    = "extension-secret-opaque"
		basePathSecret     = "base-path-secret-opaque"
		endpointPathSecret = "endpoint-path-secret-opaque"
	)
	p := setupTestPrefs(t)
	for _, provider := range []gatewayprovider.Provider{
		{
			ID: "visible-provider", Name: "Visible", BaseURL: "https://visible.example/" + basePathSecret + "/v1",
			ProxyURL:      "http://proxy-user:" + proxySecret + "@proxy.example:8080",
			Protocol:      ir.ProtocolOpenAIChat,
			Auth:          gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: providerSecret, HeaderName: "X-Upstream-Token"},
			Headers:       map[string]string{"X-Access-Token": headerSecret, "X-Tenant": "tenant"},
			Models:        []gatewayprovider.ModelMapping{{Alias: "visible/model", UpstreamModel: "model"}},
			EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/" + endpointPathSecret + "/chat"},
			ExtraBody:     json.RawMessage(`{"credential":"` + bodySecret + `"}`),
			Extensions:    map[string]json.RawMessage{"credential": json.RawMessage(`"` + extensionSecret + `"`)},
			Enabled:       true,
		},
		{
			ID: "hidden-provider", Name: "Hidden", BaseURL: "https://hidden.example/v1",
			Protocol: ir.ProtocolOpenAIChat, Auth: gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone},
			Models: []gatewayprovider.ModelMapping{{Alias: "hidden/model", UpstreamModel: "model"}}, Enabled: false,
		},
	} {
		if err := p.upsertProviderV2(&provider); err != nil {
			t.Fatalf("seed provider %s: %v", provider.ID, err)
		}
	}

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(adminIdentityKey, &adminIdentity{Role: auth.Role(c.GetHeader("X-Test-Role"))})
		c.Next()
	})
	engine.GET("/admin/api/providers", func(c *gin.Context) { handleAdminAPIListProviders(c, p) })
	engine.GET("/admin/api/providers/:id", func(c *gin.Context) { handleAdminAPIGetProvider(c, p) })

	t.Run("user list", func(t *testing.T) {
		res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers", nil, map[string]string{"X-Test-Role": string(auth.RoleUser)})
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", res.Code, res.Body.String())
		}
		var providers []gatewayprovider.Provider
		if err := json.Unmarshal(res.Body.Bytes(), &providers); err != nil {
			t.Fatal(err)
		}
		if len(providers) != 1 || providers[0].ID != "visible-provider" {
			t.Fatalf("providers = %#v, want only visible-provider", providers)
		}
		for _, secret := range []string{providerSecret, proxySecret, headerSecret, bodySecret, extensionSecret, basePathSecret, endpointPathSecret} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("user provider metadata leaked %q: %s", secret, res.Body.String())
			}
		}
		provider := providers[0]
		if provider.BaseURL != "" || provider.ProxyURL != "" || len(provider.EndpointPaths) != 0 || len(provider.Headers) != 0 ||
			len(provider.ExtraBody) != 0 || len(provider.Extensions) != 0 || provider.Auth.Key != "" || provider.Auth.KeyEnv != "" || provider.Auth.KeyRef != "" {
			t.Fatalf("user provider response retained credential-bearing fields: %#v", provider)
		}
	})

	t.Run("disabled id is indistinguishable from missing", func(t *testing.T) {
		headers := map[string]string{"X-Test-Role": string(auth.RoleUser)}
		hidden := makeRequest(t, engine, http.MethodGet, "/admin/api/providers/hidden-provider", nil, headers)
		missing := makeRequest(t, engine, http.MethodGet, "/admin/api/providers/missing-provider", nil, headers)
		if hidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || hidden.Body.String() != missing.Body.String() {
			t.Fatalf("hidden = %d %s, missing = %d %s; want identical 404s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
	})

	t.Run("operators retain disabled metadata", func(t *testing.T) {
		res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers/hidden-provider", nil, map[string]string{"X-Test-Role": string(auth.RoleAdmin)})
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body.String())
		}
	})

	t.Run("operator projection is unchanged", func(t *testing.T) {
		res := makeRequest(t, engine, http.MethodGet, "/admin/api/providers/visible-provider", nil, map[string]string{"X-Test-Role": string(auth.RoleAdmin)})
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body.String())
		}
		var provider gatewayprovider.Provider
		if err := json.Unmarshal(res.Body.Bytes(), &provider); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(provider.BaseURL, basePathSecret) || !strings.Contains(provider.ProxyURL, proxySecret) ||
			!strings.Contains(provider.EndpointPaths[ir.ProtocolOpenAIChat], endpointPathSecret) || provider.Headers["X-Access-Token"] != headerSecret {
			t.Fatalf("operator metadata was unexpectedly stripped: %#v", provider)
		}
	})
}

func TestDBAdminPermission_CrossUserIDsAreIndistinguishable404s(t *testing.T) {
	h := newDBAdminHarness(t)
	adminID := createContractUser(t, h, "idor-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminToken, _ := createContractAdminToken(t, h, adminID, bearer(h.token))
	userID := createContractUser(t, h, "idor-user", auth.RoleUser)
	clearMustChangePassword(t, h, userID)
	userSession, userCSRF := h.login(t, "idor-user", dbUserPassword)
	_, ownerTokenID := createContractAdminToken(t, h, "", bearer(h.token))

	const unknownID = "11111111-1111-1111-1111-111111111111"
	attackers := []struct {
		name    string
		headers map[string]string
	}{
		{name: "admin", headers: bearer(adminToken)},
		{name: "user", headers: cookieAuth(userSession, userCSRF)},
	}
	for _, attacker := range attackers {
		t.Run(attacker.name, func(t *testing.T) {
			checks := []struct {
				name         string
				method       string
				existingPath string
				missingPath  string
				existingBody any
				missingBody  any
			}{
				{name: "read user", method: http.MethodGet, existingPath: "/admin/api/users/" + h.owner.ID, missingPath: "/admin/api/users/" + unknownID},
				{name: "change password", method: http.MethodPost, existingPath: "/admin/api/users/" + h.owner.ID + "/password", missingPath: "/admin/api/users/" + unknownID + "/password", existingBody: gin.H{}, missingBody: gin.H{}},
				{name: "revoke user", method: http.MethodPost, existingPath: "/admin/api/users/" + h.owner.ID + "/revoke", missingPath: "/admin/api/users/" + unknownID + "/revoke"},
				{name: "list tokens", method: http.MethodGet, existingPath: "/admin/api/tokens?user_id=" + h.owner.ID, missingPath: "/admin/api/tokens?user_id=" + unknownID},
				{name: "create token", method: http.MethodPost, existingPath: "/admin/api/tokens", missingPath: "/admin/api/tokens", existingBody: gin.H{"name": "forbidden", "kind": "gateway", "user_id": h.owner.ID}, missingBody: gin.H{"name": "forbidden", "kind": "gateway", "user_id": unknownID}},
				{name: "delete token", method: http.MethodDelete, existingPath: "/admin/api/tokens/" + ownerTokenID, missingPath: "/admin/api/tokens/" + unknownID},
			}
			for _, check := range checks {
				t.Run(check.name, func(t *testing.T) {
					existing := makeRequest(t, h.engine, check.method, check.existingPath, check.existingBody, attacker.headers)
					missing := makeRequest(t, h.engine, check.method, check.missingPath, check.missingBody, attacker.headers)
					if existing.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || existing.Body.String() != missing.Body.String() {
						t.Fatalf("existing = %d %s, missing = %d %s; want identical 404s", existing.Code, existing.Body.String(), missing.Code, missing.Body.String())
					}
				})
			}
		})
	}
}

func TestDBAdminPermission_StrictCSRFAmbiguousHeadersFailClosed(t *testing.T) {
	h := newDBAdminHarness(t)
	session, csrf := h.login(t, dbOwnerUsername, dbOwnerPassword)
	engine := gin.New()
	engine.Use(adminAPIAuthorization(h.state))
	engine.POST("/admin/api/tokens", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	tests := []struct {
		name     string
		mutate   func(http.Header)
		wantCode string
		want     int
	}{
		{name: "matching origin", mutate: func(h http.Header) {}, want: http.StatusNoContent},
		{name: "conflicting fetch metadata", mutate: func(h http.Header) { h.Set("Sec-Fetch-Site", "cross-site") }, want: http.StatusForbidden, wantCode: "cross_origin"},
		{name: "repeated fetch metadata", mutate: func(h http.Header) { h.Add("Sec-Fetch-Site", "same-origin"); h.Add("Sec-Fetch-Site", "same-origin") }, want: http.StatusForbidden, wantCode: "cross_origin"},
		{name: "repeated csrf header", mutate: func(h http.Header) { h.Add(auth.CSRFHeaderName, csrf) }, want: http.StatusForbidden, wantCode: "csrf_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/tokens", http.NoBody)
			req.Host = "127.0.0.1:8080"
			for name, value := range cookieAuth(session, csrf) {
				req.Header.Set(name, value)
			}
			test.mutate(req.Header)
			res := httptest.NewRecorder()
			engine.ServeHTTP(res, req)
			if res.Code != test.want {
				t.Fatalf("status = %d, want %d (body %s)", res.Code, test.want, res.Body.String())
			}
			if test.wantCode != "" && decodeBody(t, res)["code"] != test.wantCode {
				t.Fatalf("body = %s, want code %s", res.Body.String(), test.wantCode)
			}
		})
	}
}

func TestAdminPermission_UndeclaredRouteFailsClosedOutsideTests(t *testing.T) {
	previous := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(previous) })

	engine := gin.New()
	engine.Use(adminAPIAuthorization(&authState{}))
	called := false
	engine.GET("/admin/api/undeclared", func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})
	res := makeRequest(t, engine, http.MethodGet, "/admin/api/undeclared", nil, nil)
	if called || res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "forbidden" {
		t.Fatalf("called = %v, response = %d %s; want fail-closed 403", called, res.Code, res.Body.String())
	}
}

func TestDBAdminPermission_RuntimeRoleCannotTamperWithAuditRows(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed admin API tests")
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix[:])
	schema := "audit_tamper_" + tag
	role := "gnvipi_test_app_" + tag
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	roleSQL := pgx.Identifier{role}.Sanitize()
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	var runtime *pgxpool.Pool
	t.Cleanup(func() {
		if runtime != nil {
			runtime.Close()
		}
		cleanupCtx := context.Background()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaSQL+" CASCADE"); err != nil {
			t.Errorf("drop audit test schema: %v", err)
		}
		if _, err := admin.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+roleSQL); err != nil {
			t.Errorf("drop audit test role: %v", err)
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Errorf("close audit admin connection: %v", err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE ROLE "+roleSQL+" NOLOGIN"); err != nil {
		t.Skipf("test database user cannot create roles: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Fatalf("create citext extension: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+schemaSQL+", public")
		return err
	}
	runtime, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect runtime pool: %v", err)
	}
	if _, err := runtime.Exec(ctx, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		t.Fatalf("create migration metadata: %v", err)
	}
	v1, err := adminTestDDL(schema)
	if err != nil {
		t.Fatalf("read initial migration: %v", err)
	}
	v1 = strings.ReplaceAll(v1, "gnvipi_app", role)
	v1 = strings.ReplaceAll(v1, "SCHEMA public", "SCHEMA "+schemaSQL)
	if _, err := runtime.Exec(ctx, v1); err != nil {
		t.Fatalf("apply initial migration: %v", err)
	}
	v2, err := migrations.FS.ReadFile("0002_runtime_role_grants.up.sql")
	if err != nil {
		t.Fatalf("read runtime grants migration: %v", err)
	}
	hardened := strings.ReplaceAll(string(v2), "gnvipi_app", role)
	hardened = strings.ReplaceAll(hardened, "'public'", "'"+schema+"'")
	hardened = strings.ReplaceAll(hardened, "SCHEMA public", "SCHEMA "+schemaSQL)
	if _, err := runtime.Exec(ctx, hardened); err != nil {
		t.Fatalf("apply runtime grants migration: %v", err)
	}

	conn, err := runtime.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+roleSQL); err != nil {
		t.Fatalf("set runtime role: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO audit_log (actor_type, action, target_type) VALUES ('system', 'hardening.test', 'audit')`); err != nil {
		t.Fatalf("runtime role cannot append audit row: %v", err)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'hardening.test'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runtime role cannot read appended audit row: count=%d err=%v", count, err)
	}
	for _, statement := range []string{
		`UPDATE audit_log SET action = 'tampered' WHERE action = 'hardening.test'`,
		`DELETE FROM audit_log WHERE action = 'hardening.test'`,
	} {
		_, err := conn.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("audit tamper %q error = %v, want SQLSTATE 42501", statement, err)
		}
	}
}
