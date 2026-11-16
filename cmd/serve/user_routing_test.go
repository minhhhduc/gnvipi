package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// TestFileModeAutomodeCycleStaysGlobal is the #240 regression guard: without a
// database the config API keeps writing the single process-wide setting.
func TestFileModeAutomodeCycleStaysGlobal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, _ := setupTestV2Prefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	if p.userRouting != nil {
		t.Fatal("file mode must not install per-account routing")
	}
	engine := gin.New()
	registerAdminAPIRoutes(engine, p, nil, nil, nil)

	if out := makeRequest(t, engine, http.MethodPost, "/admin/api/config/automode", map[string]any{"model": "v2-active"}, nil); out.Code != http.StatusOK {
		t.Fatalf("file-mode automode = %d: %s", out.Code, out.Body.String())
	}
	if got := p.autoModelID(); got != "v2-active" {
		t.Fatalf("file-mode global automode = %q, want v2-active", got)
	}
	if out := makeRequest(t, engine, http.MethodGet, "/admin/api/config/automode", nil, nil); out.Code != http.StatusOK {
		t.Fatalf("file-mode automode GET = %d: %s", out.Code, out.Body.String())
	}
	if out := makeRequest(t, engine, http.MethodPost, "/admin/api/config/cycle", map[string]any{"models": []string{"v2-active"}}, nil); out.Code != http.StatusOK {
		t.Fatalf("file-mode cycle = %d: %s", out.Code, out.Body.String())
	}
	if cfg, _ := p.cycleSnapshot(); len(cfg.Models) != 1 || cfg.Models[0] != "v2-active" {
		t.Fatalf("file-mode global cycle = %v, want [v2-active]", cfg.Models)
	}
}

func TestUserRoutingPrefsPerAccountDB(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping per-user routing tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)

	createUser := func(username, role string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role, password_hash) VALUES ($1, $2, 'x') RETURNING id::text`,
			username, role).Scan(&id); err != nil {
			t.Fatalf("create user %s: %v", username, err)
		}
		return id
	}
	alice := createUser("route-alice", "user")
	bob := createUser("route-bob", "user")

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (id, name, protocol, base_url)
		 VALUES ('db240', 'DB 240', 'openai-chat', 'https://example.com/v1')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	seedKey := func(owner, alias, upstream string) {
		t.Helper()
		var keyID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO provider_keys (provider_id, owner_id, name, auth_scheme)
			 VALUES ('db240', $1::uuid, $2, 'none') RETURNING id::text`,
			owner, "key-"+alias).Scan(&keyID); err != nil {
			t.Fatalf("insert key for %s: %v", alias, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, $2, $3)`,
			keyID, alias, upstream); err != nil {
			t.Fatalf("insert alias %s: %v", alias, err)
		}
	}
	seedKey(alice, "alias-a", "up-a")
	seedKey(alice, "alias-a2", "up-a2")
	seedKey(bob, "alias-b", "up-b")
	seedKey(bob, "alias-b2", "up-b2")

	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), providerRuntimeOptions{})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	p := setupTestPrefs(t)
	p.userRouting = newUserRoutingPrefs(pool, runtime.router, p)
	u := p.userRouting
	aliceIdentity := &adminIdentity{UserID: alice, Role: auth.RoleUser}
	bobIdentity := &adminIdentity{UserID: bob, Role: auth.RoleUser}

	if err := u.setAutomode(ctx, aliceIdentity, alice, "alias-a"); err != nil {
		t.Fatalf("alice automode: %v", err)
	}
	if err := u.setAutomode(ctx, bobIdentity, bob, "alias-b"); err != nil {
		t.Fatalf("bob automode: %v", err)
	}
	if got, err := u.automodeTarget(ctx, alice); err != nil || got != "alias-a" {
		t.Fatalf("alice target = %q, %v; want alias-a", got, err)
	}
	if got, err := u.automodeTarget(ctx, bob); err != nil || got != "alias-b" {
		t.Fatalf("bob target = %q, %v; want alias-b", got, err)
	}

	// A target that exists only in another account's key is rejected on write
	// and cannot resolve.
	if err := u.validateTarget(ctx, alice, "alias-b"); err == nil {
		t.Fatal("alice resolved bob's alias")
	}
	if _, err := runtime.router.Resolve(ctx, alice, "alias-b", ""); err == nil {
		t.Fatal("key-aware router resolved bob's alias for alice")
	}

	if err := u.setCycle(ctx, aliceIdentity, alice, []string{"alias-a", "alias-a2"}); err != nil {
		t.Fatalf("alice cycle: %v", err)
	}
	if err := u.setCycle(ctx, bobIdentity, bob, []string{"alias-b", "alias-b2"}); err != nil {
		t.Fatalf("bob cycle: %v", err)
	}
	if got, err := u.cycleTargets(ctx, alice); err != nil || len(got) != 2 || got[0] != "alias-a" {
		t.Fatalf("alice cycle = %v, %v", got, err)
	}
	if got, err := u.cycleTargets(ctx, bob); err != nil || len(got) != 2 || got[0] != "alias-b" {
		t.Fatalf("bob cycle = %v, %v", got, err)
	}
	if u.cycleStart(alice) != 0 || u.cycleStart(bob) != 0 {
		t.Fatalf("initial cursors = alice %d bob %d, want 0/0", u.cycleStart(alice), u.cycleStart(bob))
	}
	u.advanceCycle(alice, []string{"alias-a", "alias-a2"}, "alias-a")
	if u.cycleStart(alice) != 1 {
		t.Fatalf("alice cursor = %d, want 1", u.cycleStart(alice))
	}
	if u.cycleStart(bob) != 0 {
		t.Fatalf("bob cursor leaked alice's advance: %d", u.cycleStart(bob))
	}

	// Audit rows stay identifiers-and-counts only.
	var count, list, auditAction string
	if err := pool.QueryRow(ctx, `
		SELECT action, COALESCE(details->>'count', ''), COALESCE(details->>'models', '')
		  FROM audit_log
		 WHERE user_id = $1::uuid AND action = 'routing.cycle.update'
		 ORDER BY id DESC LIMIT 1`, alice).Scan(&auditAction, &count, &list); err != nil {
		t.Fatalf("read routing audit: %v", err)
	}
	if auditAction != "routing.cycle.update" || count != "2" || list != "alias-a,alias-a2" {
		t.Fatalf("audit = %q count=%q models=%q", auditAction, count, list)
	}

	// Deleting the account cascades its preferences.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, bob); err != nil {
		t.Fatalf("delete bob: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_routing_prefs WHERE user_id = $1::uuid`, bob).Scan(&remaining); err != nil {
		t.Fatalf("count bob prefs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("bob preferences survived account deletion: %d", remaining)
	}
}

func TestAdminRoutingConfigPerCallerDB(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping per-user routing tests")
	}
	ctx := t.Context()
	pool := newAdminTestPool(t, dsn)

	createUser := func(username, role string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role, password_hash) VALUES ($1, $2, 'x') RETURNING id::text`,
			username, role).Scan(&id); err != nil {
			t.Fatalf("create user %s: %v", username, err)
		}
		return id
	}
	superuser := createUser("route-superuser", "superuser")
	admin := createUser("route-admin", "admin")
	member := createUser("route-member", "user")

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (id, name, protocol, base_url)
		 VALUES ('db240', 'DB 240', 'openai-chat', 'https://example.com/v1')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	seedKey := func(ownerID, alias string) {
		t.Helper()
		var keyID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO provider_keys (provider_id, owner_id, name, auth_scheme)
			 VALUES ('db240', $1::uuid, $2, 'none') RETURNING id::text`,
			ownerID, "key-"+alias).Scan(&keyID); err != nil {
			t.Fatalf("insert key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, $2, $3)`,
			keyID, alias, alias+"-up"); err != nil {
			t.Fatalf("insert alias: %v", err)
		}
	}
	seedKey(superuser, "superuser-model")
	seedKey(admin, "admin-model")
	seedKey(member, "member-model")

	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(pool), providerRuntimeOptions{})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	t.Cleanup(runtime.Close)

	gin.SetMode(gin.TestMode)
	p := setupTestPrefs(t)
	p.userRouting = newUserRoutingPrefs(pool, runtime.router, p)
	state := &authState{pool: pool}
	newEngine := func(identity *adminIdentity) *gin.Engine {
		engine := gin.New()
		engine.Use(func(c *gin.Context) {
			c.Set(adminIdentityKey, identity)
			c.Next()
		})
		registerAdminAPIRoutes(engine, p, nil, nil, state)
		return engine
	}
	adminEngine := newEngine(&adminIdentity{UserID: admin, Role: auth.RoleAdmin})
	superuserEngine := newEngine(&adminIdentity{UserID: superuser, Role: auth.RoleSuperuser})

	// Admin edits their own account and cannot address another one.
	if out := makeRequest(t, adminEngine, http.MethodPost, "/admin/api/config/automode",
		map[string]any{"model": "admin-model"}, nil); out.Code != http.StatusOK {
		t.Fatalf("admin own automode = %d: %s", out.Code, out.Body.String())
	}
	if out := makeRequest(t, adminEngine, http.MethodPost, "/admin/api/config/automode",
		map[string]any{"model": "admin-model", "user_id": superuser}, nil); out.Code != http.StatusNotFound {
		t.Fatalf("admin foreign automode = %d, want 404: %s", out.Code, out.Body.String())
	}
	if out := makeRequest(t, adminEngine, http.MethodGet, "/admin/api/config/cycle?user_id="+superuser, nil, nil); out.Code != http.StatusNotFound {
		t.Fatalf("admin foreign cycle GET = %d, want 404: %s", out.Code, out.Body.String())
	}
	// An alias reachable only through the member's key is rejected for admin.
	if out := makeRequest(t, adminEngine, http.MethodPost, "/admin/api/config/automode",
		map[string]any{"model": "member-model"}, nil); out.Code != http.StatusBadRequest {
		t.Fatalf("admin cross-account target = %d, want 400: %s", out.Code, out.Body.String())
	}

	// The superuser may address another account.
	if out := makeRequest(t, superuserEngine, http.MethodPost, "/admin/api/config/automode",
		map[string]any{"model": "member-model", "user_id": member}, nil); out.Code != http.StatusOK {
		t.Fatalf("superuser foreign automode = %d: %s", out.Code, out.Body.String())
	}
	if out := makeRequest(t, superuserEngine, http.MethodGet,
		"/admin/api/config/automode?user_id="+member, nil, nil); out.Code != http.StatusOK {
		t.Fatalf("superuser foreign automode GET = %d: %s", out.Code, out.Body.String())
	} else if body := decodeBody(t, out); body["model"] != "member-model" {
		t.Fatalf("superuser read member automode = %v", body["model"])
	}

	// Unknown aliases are a field error, and PUT is equivalent to POST.
	if out := makeRequest(t, adminEngine, http.MethodPut, "/admin/api/config/cycle",
		map[string]any{"models": []string{"missing-alias"}}, nil); out.Code != http.StatusBadRequest {
		t.Fatalf("unknown cycle model = %d, want 400: %s", out.Code, out.Body.String())
	} else if body := decodeBody(t, out); body["field_errors"] == nil {
		t.Fatalf("missing field_errors in %s", out.Body.String())
	}
	if out := makeRequest(t, adminEngine, http.MethodPut, "/admin/api/config/cycle",
		map[string]any{"models": []string{"admin-model"}}, nil); out.Code != http.StatusOK {
		t.Fatalf("admin cycle PUT = %d: %s", out.Code, out.Body.String())
	}
}
