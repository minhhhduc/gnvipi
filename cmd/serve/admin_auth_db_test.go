package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/migrations"
)

const (
	dbOwnerUsername = "owner"
	dbOwnerPassword = "Crimson-Harbor7-Velvet!"
	dbUserPassword  = "Tundra-Lantern7-Cobalt!"
	dbTestOrigin    = "http://127.0.0.1:8080"
)

// dbAdminHarness is the production admin wiring backed by a real PostgreSQL
// server: migrations, identity tables, sessions, tokens and audit rows.
type dbAdminHarness struct {
	engine *gin.Engine
	pool   *pgxpool.Pool
	state  *authState
	owner  *auth.User
	token  string
}

// newDBAdminHarness applies the real migrations and wires the admin API exactly
// as run() does in database mode. It skips unless GNVIPI_TEST_DATABASE_URL
// points at a disposable database.
func newDBAdminHarness(t *testing.T) *dbAdminHarness {
	t.Helper()
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed admin API tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)
	state := newAuthState(pool)
	// The harness serves http://127.0.0.1:8080, i.e. a loopback bind, which is
	// the one configuration where plaintext cookies are allowed.
	state.loopbackBind = true
	owner, err := auth.CreateFirstOwner(ctx, pool, auth.CreateOwnerInput{
		Username:    dbOwnerUsername,
		Email:       "owner@example.test",
		DisplayName: "Owner",
		Password:    dbOwnerPassword,
	}, state.password)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	expiry := time.Now().Add(time.Hour)
	token, _, err := auth.CreateAPIToken(ctx, pool, auth.CreateAPITokenInput{
		UserID: owner.ID,
		Kind:   auth.TokenKindAdmin,
		Name:   "harness",
		Scope:  auth.ScopeWrite,
		Expiry: auth.TokenExpiry{At: &expiry},
		Now:    time.Now(),
	})
	if err != nil {
		t.Fatalf("create harness token: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAPIAuthorization(state))
	registerAdminAPIRoutes(engine, setupTestPrefs(t), nil, nil, state)
	return &dbAdminHarness{engine: engine, pool: pool, state: state, owner: owner, token: token}
}

// adminTestDDL is the production initial migration with its hardcoded public
// search_path rewritten so the schema installs into a throwaway schema.
func adminTestDDL(schema string) (string, error) {
	raw, err := migrations.FS.ReadFile("0001_init.up.sql")
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(raw), "SET search_path = public, pg_temp", "SET search_path = "+schema+", pg_temp"), nil
}

// newAdminTestPool installs the production schema into a throwaway schema, so
// these tests never touch the shared public tables other packages' DB tests use.
func newAdminTestPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		admin.Close()
		t.Fatalf("schema suffix: %v", err)
	}
	schema := "serve_admin_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		admin.Close()
		t.Fatalf("create citext extension: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET search_path TO "+schema+", public")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") //nolint:errcheck // best-effort cleanup
		admin.Close()
	})
	ddl, err := adminTestDDL(schema)
	if err != nil {
		t.Fatalf("read embedded migration: %v", err)
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("install admin test schema: %v", err)
	}
	return pool
}

func decodeBody(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %d: %v (body %s)", res.Code, err, res.Body.String())
	}
	return body
}

func cookieValue(res *httptest.ResponseRecorder, name string) string {
	for _, c := range res.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// cookieAuth is the header set a browser sends on a cookie-authenticated
// request: session cookie, CSRF double-submit pair, matching Origin.
func cookieAuth(session, csrf string) map[string]string {
	return map[string]string{
		"Cookie":            auth.SessionCookieName + "=" + session + "; " + auth.CSRFCookieName + "=" + csrf,
		auth.CSRFHeaderName: csrf,
		"Origin":            dbTestOrigin,
	}
}

func bearer(raw string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + raw}
}

func (h *dbAdminHarness) login(t *testing.T, username, password string) (session, csrf string) {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPost, adminLoginPath, gin.H{"username": username, "password": password}, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("login %s = %d, want 200 (body %s)", username, res.Code, res.Body.String())
	}
	body := decodeBody(t, res)
	session = cookieValue(res, auth.SessionCookieName)
	csrf, _ = body["csrf_token"].(string)
	if session == "" || csrf == "" {
		t.Fatalf("login %s did not set session and CSRF credentials (body %s)", username, res.Body.String())
	}
	return session, csrf
}

func TestDBAdminAuth_LoginSessionLogout(t *testing.T) {
	h := newDBAdminHarness(t)

	// Unauthenticated access to an identity route is refused before any handler.
	res := makeRequest(t, h.engine, http.MethodGet, "/admin/api/users", nil, nil)
	if res.Code != http.StatusUnauthorized || decodeBody(t, res)["code"] != "authentication_required" {
		t.Fatalf("anonymous GET /admin/api/users = %d %s, want 401 authentication_required", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodPost, adminLoginPath, gin.H{"username": dbOwnerUsername, "password": "Definitely-Not-The-Password9!"}, nil)
	if res.Code != http.StatusUnauthorized || decodeBody(t, res)["code"] != "invalid_credentials" {
		t.Fatalf("login with wrong password = %d %s, want 401 invalid_credentials", res.Code, res.Body.String())
	}

	session, csrf := h.login(t, dbOwnerUsername, dbOwnerPassword)

	res = makeRequest(t, h.engine, http.MethodGet, adminSessionPath, nil, map[string]string{
		"Cookie": auth.SessionCookieName + "=" + session,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", adminSessionPath, res.Code, res.Body.String())
	}
	user, _ := decodeBody(t, res)["user"].(map[string]any)
	if user["id"] != h.owner.ID || user["role"] != string(auth.RoleOwner) {
		t.Fatalf("session identity = %v, want owner %s", user, h.owner.ID)
	}

	// Signing in again from a browser that still holds the old cookie rotates
	// the session: the superseded token stops working immediately.
	res = makeRequest(t, h.engine, http.MethodPost, adminLoginPath, gin.H{
		"username": dbOwnerUsername,
		"password": dbOwnerPassword,
	}, map[string]string{"Cookie": auth.SessionCookieName + "=" + session})
	if res.Code != http.StatusOK {
		t.Fatalf("second login = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	rotated := cookieValue(res, auth.SessionCookieName)
	rotatedCSRF, _ := decodeBody(t, res)["csrf_token"].(string)
	if rotated == "" || rotated == session || rotatedCSRF == "" {
		t.Fatalf("second login did not rotate the session token (old %q, new %q)", session, rotated)
	}
	res = makeRequest(t, h.engine, http.MethodGet, adminSessionPath, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("superseded session = %d, want 401 (body %s)", res.Code, res.Body.String())
	}
	session, csrf = rotated, rotatedCSRF

	res = makeRequest(t, h.engine, http.MethodPost, adminLogoutPath, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNoContent {
		t.Fatalf("POST %s = %d, want 204 (body %s)", adminLogoutPath, res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, adminSessionPath, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d, want 401 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminAuth_RoleAndMustChangePassword(t *testing.T) {
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "viewer",
		"role":     string(auth.RoleUser),
		"password": dbUserPassword,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("owner create user = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	created := decodeBody(t, res)
	viewerID, _ := created["id"].(string)
	if viewerID == "" || created["must_change_password"] != true {
		t.Fatalf("created user = %v, want an id and must_change_password", created)
	}

	session, csrf := h.login(t, "viewer", dbUserPassword)

	// A fresh account cannot use the console until the password changes.
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users", nil, cookieAuth(session, csrf))
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "password_change_required" {
		t.Fatalf("GET /admin/api/users before password change = %d %s, want 403 password_change_required", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodPost, adminPasswordPath, gin.H{
		"current_password": dbUserPassword,
		"new_password":     "Nimbus-Falcon4-Ember!",
	}, cookieAuth(session, csrf))
	if res.Code != http.StatusNoContent {
		t.Fatalf("POST %s = %d, want 204 (body %s)", adminPasswordPath, res.Code, res.Body.String())
	}

	// Still a role denial: the account may only reach itself.
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users", nil, cookieAuth(session, csrf))
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "forbidden" {
		t.Fatalf("user GET /admin/api/users = %d %s, want 403 forbidden", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users/"+viewerID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusOK {
		t.Fatalf("user GET own record = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users/"+h.owner.ID, nil, cookieAuth(session, csrf))
	if res.Code != http.StatusNotFound {
		t.Fatalf("user GET owner record = %d, want 404 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminAuth_TokenScopeAndUndeclaredRoute(t *testing.T) {
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":       "reader",
		"kind":       string(auth.TokenKindAdmin),
		"scope":      string(auth.ScopeRead),
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create read token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	raw, _ := decodeBody(t, res)["token"].(string)
	if raw == "" {
		t.Fatal("token creation did not return the plaintext token")
	}

	res = makeRequest(t, h.engine, http.MethodGet, "/admin/api/users", nil, bearer(raw))
	if res.Code != http.StatusOK {
		t.Fatalf("read token GET /admin/api/users = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "sideeffect",
		"role":     string(auth.RoleUser),
		"password": dbUserPassword,
	}, bearer(raw))
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "insufficient_scope" {
		t.Fatalf("read token POST /admin/api/users = %d %s, want 403 insufficient_scope", res.Code, res.Body.String())
	}

	// A route with no declaration in the permission table fails closed.
	ghost := gin.New()
	ghost.Use(adminAPIAuthorization(h.state))
	ghost.GET("/admin/api/ghost", func(c *gin.Context) { c.Status(http.StatusOK) })
	res = makeRequest(t, ghost, http.MethodGet, "/admin/api/ghost", nil, bearer(h.token))
	if res.Code != http.StatusInternalServerError || decodeBody(t, res)["code"] != "undeclared_route" {
		t.Fatalf("undeclared admin route = %d %s, want 500 undeclared_route", res.Code, res.Body.String())
	}
}

func TestDBAdminAuth_ExplicitBearerTakesPrecedenceOverCookie(t *testing.T) {
	h := newDBAdminHarness(t)
	session, _ := h.login(t, dbOwnerUsername, dbOwnerPassword)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "bearer-wins",
		"role":     string(auth.RoleUser),
		"password": dbUserPassword,
	}, map[string]string{
		"Authorization": "Bearer " + h.token,
		"Cookie":        auth.SessionCookieName + "=" + session,
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("bearer plus cookie POST = %d, want 201 without CSRF (body %s)", res.Code, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodGet, adminSessionPath, nil, map[string]string{
		"Authorization": "not-bearer",
		"Cookie":        auth.SessionCookieName + "=" + session,
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("malformed explicit authorization plus valid cookie = %d, want 401 (body %s)", res.Code, res.Body.String())
	}
}

func TestDBAdminAuth_AuditTrail(t *testing.T) {
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "audited",
		"role":     string(auth.RoleUser),
		"password": dbUserPassword,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create user = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	userID, _ := decodeBody(t, res)["id"].(string)

	// The write used the owner's admin token, so actor_id names that token while
	// user_id indexes the account affected by the mutation.
	var actorType, actorID, subjectUser, targetType, targetID string
	err := h.pool.QueryRow(t.Context(),
		`SELECT actor_type, actor_id::text, user_id::text, target_type, target_id FROM audit_log WHERE action = 'user.create' AND target_id = $1`,
		userID).Scan(&actorType, &actorID, &subjectUser, &targetType, &targetID)
	if err != nil {
		t.Fatalf("read user.create audit row: %v", err)
	}
	if actorType != "token" || actorID == "" || subjectUser != userID || targetType != "user" || targetID != userID {
		t.Fatalf("audit row = %s/%s subject %s target %s/%s, want token/<token id> subject %s target user/%s", actorType, actorID, subjectUser, targetType, targetID, userID, userID)
	}

	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name":    "audited gateway token",
		"kind":    string(auth.TokenKindGateway),
		"user_id": userID,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create target token = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	createdTokenID, _ := decodeBody(t, res)["id"].(string)
	var tokenSubject string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT user_id::text FROM audit_log WHERE action = 'token.create' AND target_id = $1`,
		createdTokenID).Scan(&tokenSubject); err != nil {
		t.Fatalf("read token.create audit row: %v", err)
	}
	if tokenSubject != userID {
		t.Fatalf("token.create subject = %s, want target user %s", tokenSubject, userID)
	}

	// An owner reset over the API must name the acting token too; only the CLI
	// reset legitimately records actor_type = 'system'.
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/users/"+userID+"/password", gin.H{
		"password": dbUserPassword,
	}, bearer(h.token))
	if res.Code != http.StatusNoContent {
		t.Fatalf("owner reset = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
	var resetActor, resetActorID, resetUser string
	err = h.pool.QueryRow(t.Context(),
		`SELECT actor_type, actor_id::text, user_id::text FROM audit_log WHERE action = 'user.password_reset' AND target_id = $1`,
		userID).Scan(&resetActor, &resetActorID, &resetUser)
	if err != nil {
		t.Fatalf("read user.password_reset audit row: %v", err)
	}
	if resetActor != "token" || resetActorID == "" || resetUser != userID {
		t.Fatalf("reset audit row = %s/%s user %s, want token/<token id> user %s", resetActor, resetActorID, resetUser, userID)
	}

	res = makeRequest(t, h.engine, http.MethodPost, adminPasswordPath, gin.H{
		"current_password": dbOwnerPassword,
		"new_password":     "Quartz-Meadow8-Signal!",
	}, bearer(h.token))
	if res.Code != http.StatusNoContent {
		t.Fatalf("bearer password change = %d, want 204 (body %s)", res.Code, res.Body.String())
	}
	var changeActor, changeActorID string
	err = h.pool.QueryRow(t.Context(),
		`SELECT actor_type, actor_id::text FROM audit_log WHERE action = 'user.password_change' AND target_id = $1 ORDER BY id DESC LIMIT 1`,
		h.owner.ID).Scan(&changeActor, &changeActorID)
	if err != nil {
		t.Fatalf("read user.password_change audit row: %v", err)
	}
	if changeActor != "token" || changeActorID == "" {
		t.Fatalf("password change actor = %s/%s, want token/<token id>", changeActor, changeActorID)
	}
}

// TestDBAdminAuth_PlaintextLoginRequiresLoopback pins the transport rule: the
// new password endpoint must not be weaker than the file-token login it
// replaces, which has always refused cleartext logins off loopback.
func TestDBAdminAuth_PlaintextLoginRequiresLoopback(t *testing.T) {
	h := newDBAdminHarness(t)

	exposed := *h.state
	exposed.loopbackBind = false
	engine := gin.New()
	engine.Use(adminAPIAuthorization(&exposed))
	registerAdminAPIRoutes(engine, setupTestPrefs(t), nil, nil, &exposed)

	res := makeRequest(t, engine, http.MethodPost, adminLoginPath, gin.H{
		"username": dbOwnerUsername,
		"password": dbOwnerPassword,
	}, nil)
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "https_required" {
		t.Fatalf("plaintext login on a public bind = %d %s, want 403 https_required", res.Code, res.Body.String())
	}

	// A session cookie captured elsewhere must not be replayed over plaintext.
	res = makeRequest(t, engine, http.MethodGet, adminSessionPath, nil, map[string]string{
		"Cookie": auth.SessionCookieName + "=whatever",
	})
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "https_required" {
		t.Fatalf("plaintext session read on a public bind = %d %s, want 403 https_required", res.Code, res.Body.String())
	}
}

func TestDBAdminAuth_LogoutReportsRevocationFailure(t *testing.T) {
	h := newDBAdminHarness(t)
	h.pool.Close()

	engine := gin.New()
	engine.POST(adminLogoutPath, handleAdminLogout(h.state))
	res := makeRequest(t, engine, http.MethodPost, adminLogoutPath, nil, map[string]string{
		"Cookie": auth.SessionCookieName + "=known-session",
	})
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("logout with unavailable session store = %d, want 500 (body %s)", res.Code, res.Body.String())
	}
}

// TestDBAdminAuth_LockoutDoesNotRevealAccounts keeps the lock threshold
// identical for existing and unknown usernames: a 429 reachable only for real
// accounts would turn sign-in into an existence oracle.
func TestDBAdminAuth_LockoutDoesNotRevealAccounts(t *testing.T) {
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "known",
		"role":     string(auth.RoleUser),
		"password": dbUserPassword,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create user = %d, want 201 (body %s)", res.Code, res.Body.String())
	}

	fail := func(username string) int {
		t.Helper()
		return makeRequest(t, h.engine, http.MethodPost, adminLoginPath, gin.H{
			"username": username,
			"password": "Wrong-Guess7-Anchor!",
		}, nil).Code
	}
	for _, username := range []string{"known", "ghost"} {
		for i := 0; i < 5; i++ {
			if code := fail(username); code != http.StatusUnauthorized {
				t.Fatalf("failure %d for %q = %d, want 401", i+1, username, code)
			}
		}
		if code := fail(username); code != http.StatusTooManyRequests {
			t.Fatalf("sixth failure for %q = %d, want 429", username, code)
		}
	}
}

func TestDBAdminAuth_OversizedLoginUsernameIsBounded(t *testing.T) {
	h := newDBAdminHarness(t)
	username := strings.Repeat("x", 1024)
	res := makeRequest(t, h.engine, http.MethodPost, adminLoginPath, gin.H{
		"username": username,
		"password": "Wrong-Guess7-Anchor!",
	}, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("oversized username login = %d, want 401 (body %s)", res.Code, res.Body.String())
	}
	var storedLength int
	if err := h.pool.QueryRow(t.Context(), `SELECT length(username) FROM login_attempts ORDER BY id DESC LIMIT 1`).Scan(&storedLength); err != nil {
		t.Fatalf("read oversized login attempt: %v", err)
	}
	if storedLength > 64 {
		t.Fatalf("stored login username length = %d, want <= 64", storedLength)
	}
}

func TestDBAdminAuth_CreateUserValidatesBoundedFields(t *testing.T) {
	h := newDBAdminHarness(t)
	for _, tc := range []struct {
		name  string
		body  gin.H
		field string
	}{
		{
			name:  "short username",
			body:  gin.H{"username": "ab", "password": dbUserPassword},
			field: "username",
		},
		{
			name:  "long display name",
			body:  gin.H{"username": "bounded-user", "display_name": strings.Repeat("x", 201), "password": dbUserPassword},
			field: "display_name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/users", tc.body, bearer(h.token))
			if res.Code != http.StatusBadRequest || decodeBody(t, res)["field"] != tc.field {
				t.Fatalf("create user = %d %s, want 400 field %s", res.Code, res.Body.String(), tc.field)
			}
		})
	}
}

func TestVerifyAdminCookieWriteSkipsSafeMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), method, dbTestOrigin+adminSessionPath, http.NoBody)
			if failure := verifyAdminCookieWrite(c); failure != nil {
				t.Fatalf("safe %s request required CSRF: %+v", method, failure)
			}
		})
	}
}

func TestDBAdminAuth_RehashDoesNotOverwriteChangedPassword(t *testing.T) {
	h := newDBAdminHarness(t)
	replacement, err := auth.HashPassword("Quartz-Meadow8-Signal!", h.state.password.Params, h.state.password.Pepper)
	if err != nil {
		t.Fatalf("hash replacement password: %v", err)
	}
	if _, err := h.pool.Exec(t.Context(), `UPDATE users SET password_hash = $2 WHERE id = $1::uuid`, h.owner.ID, replacement); err != nil {
		t.Fatalf("replace password hash: %v", err)
	}

	adminRehashPassword(t.Context(), h.state, h.owner.ID, dbOwnerPassword, h.owner.PasswordHash)
	var got string
	if err := h.pool.QueryRow(t.Context(), `SELECT password_hash FROM users WHERE id = $1::uuid`, h.owner.ID).Scan(&got); err != nil {
		t.Fatalf("read password hash: %v", err)
	}
	if got != replacement {
		t.Fatal("login rehash overwrote a concurrently changed password")
	}
}
