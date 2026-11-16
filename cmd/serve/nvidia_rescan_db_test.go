package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

func restoreModels(t *testing.T) {
	t.Helper()
	saved := models.Snapshot()
	t.Cleanup(func() { models.Replace(saved) })
}

func TestDBAdminNvidiaRescanRoleMatrix(t *testing.T) {
	restoreModels(t)
	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/rescan-matrix", Slug: "rescan-matrix", Namespace: "qc69jvmznzxy", FunctionID: "66666666-6666-6666-6666-666666666666"},
	))
	h := newDBAdminHarness(t)

	adminID := createContractUser(t, h, "rescan-admin", auth.RoleAdmin)
	clearMustChangePassword(t, h, adminID)
	adminToken, _ := createContractAdminToken(t, h, adminID, bearer(h.token))
	userID := createContractUser(t, h, "rescan-user", auth.RoleUser)
	clearMustChangePassword(t, h, userID)
	userSession, userCSRF := h.login(t, "rescan-user", dbUserPassword)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"superuser", bearer(h.token), http.StatusOK},
		{"admin", bearer(adminToken), http.StatusOK},
		{"user", cookieAuth(userSession, userCSRF), http.StatusForbidden},
		{"anonymous", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, tc.headers)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

func TestDBAdminNvidiaRescanAuditCountsOnly(t *testing.T) {
	const canary = "captcha-canary-4d8e"
	restoreModels(t)
	// Start from an empty catalog so the counts under test do not depend on
	// whatever the checkout's model file happens to contain in CI.
	models.Replace(map[string]models.ModelInfo{})
	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/" + canary, Slug: canary, Namespace: "qc69jvmznzxy", FunctionID: "77777777-7777-7777-7777-777777777777"},
	))
	h := newDBAdminHarness(t)

	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("rescan = %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), canary) {
		t.Fatalf("response leaked the model token: %s", res.Body.String())
	}

	var count int
	if err := h.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log WHERE action = 'nvidia.catalog.rescan'`).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit rows = %d, want 1", count)
	}
	var actorType, detailsRaw string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT actor_type, details::text FROM audit_log WHERE action = 'nvidia.catalog.rescan'`).
		Scan(&actorType, &detailsRaw); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actorType != "token" {
		t.Fatalf("actor_type = %q, want token", actorType)
	}
	if strings.Contains(detailsRaw, canary) {
		t.Fatalf("audit details leaked the model token: %s", detailsRaw)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(detailsRaw), &details); err != nil {
		t.Fatalf("decode audit details: %v", err)
	}
	if details["added"] != float64(1) || details["updated"] != float64(0) || details["removed"] != float64(0) {
		t.Fatalf("audit details = %v", details)
	}
}

func TestDBNvidiaRescan_CrossInstanceRefresh(t *testing.T) {
	restoreModels(t)
	h := newDBAdminHarness(t)
	ctx := t.Context()

	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/role-test", Slug: "role-test", Namespace: "qc69jvmznzxy", FunctionID: "77777777-7777-7777-7777-777777777777"},
	))

	// Instance B starts with an empty catalog.
	models.Replace(map[string]models.ModelInfo{})
	prefsB := &modelPrefs{catalogRead: h.pool}
	runtimeB, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(h.pool), providerRuntimeOptions{
		OnPublish: prefsB.applySnapshotDB,
	})
	if err != nil {
		t.Fatalf("open runtimeB: %v", err)
	}
	t.Cleanup(runtimeB.Close)
	vBefore := runtimeB.store.Version()

	// Instance A rescans and commits to nvidia_models.
	prefsA := &modelPrefs{catalogRead: h.pool, catalogWrite: h.pool}
	res, err := prefsA.rescanNvidiaCatalog(ctx)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("res.Added = %d, want 1", res.Added)
	}

	// Reset local registry to verify Instance B's snapshot reload publishes it.
	models.Replace(map[string]models.ModelInfo{})
	if err := runtimeB.store.Reload(ctx); err != nil {
		t.Fatalf("runtimeB reload: %v", err)
	}
	if runtimeB.store.Version() <= vBefore {
		t.Fatalf("runtimeB version = %d, want > %d", runtimeB.store.Version(), vBefore)
	}
	if _, err := models.Lookup("acme/role-test"); err != nil {
		t.Fatalf("Instance B did not publish new catalog model: %v", err)
	}
}

func TestDBNvidiaRescan_LeastPrivilege(t *testing.T) {
	restoreModels(t)
	h := newDBAdminHarness(t)
	ctx := t.Context()

	var schema string
	if err := h.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("query current_schema: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("role suffix: %v", err)
	}
	suffixHex := hex.EncodeToString(suffix[:])
	appRole := "rescan_app_" + suffixHex
	maintRole := "rescan_maint_" + suffixHex
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	appRoleSQL := pgx.Identifier{appRole}.Sanitize()
	maintRoleSQL := pgx.Identifier{maintRole}.Sanitize()

	// Verify runtime role cannot write nvidia_models while maintenance role can.
	if _, err := h.pool.Exec(ctx, "CREATE ROLE "+appRoleSQL+" NOLOGIN; CREATE ROLE "+maintRoleSQL+" NOLOGIN;"); err != nil {
		t.Skipf("test database user cannot create roles: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = h.pool.Exec(cleanupCtx, "REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA "+schemaSQL+" FROM "+appRoleSQL+", "+maintRoleSQL) //nolint:errcheck // best-effort cleanup
		_, _ = h.pool.Exec(cleanupCtx, "REVOKE ALL PRIVILEGES ON SCHEMA "+schemaSQL+" FROM "+appRoleSQL+", "+maintRoleSQL)               //nolint:errcheck // best-effort cleanup
		_, _ = h.pool.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+appRoleSQL+", "+maintRoleSQL)                                              //nolint:errcheck // best-effort cleanup
	})
	if _, err := h.pool.Exec(ctx, `
		GRANT USAGE ON SCHEMA `+schemaSQL+` TO `+appRoleSQL+`, `+maintRoleSQL+`;
		GRANT SELECT ON nvidia_models, config_state, providers, provider_keys, key_models, model_state, settings TO `+appRoleSQL+`;
		REVOKE INSERT, UPDATE, DELETE ON nvidia_models FROM `+appRoleSQL+`;
		GRANT SELECT, INSERT, DELETE ON nvidia_models TO `+maintRoleSQL+`;
	`); err != nil {
		t.Fatalf("configure role grants: %v", err)
	}

	openRolePool := func(roleSQL string) *pgxpool.Pool {
		t.Helper()
		cfg := h.pool.Config().Copy()
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET search_path TO "+schemaSQL+", public; SET ROLE "+roleSQL)
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("open pool for %s: %v", roleSQL, err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	appPool := openRolePool(appRoleSQL)
	maintPool := openRolePool(maintRoleSQL)

	stubNvidiaDiscovery(t, rescanFixture(
		models.PlaygroundRow{Model: "acme/role-test", Slug: "role-test", Namespace: "qc69jvmznzxy", FunctionID: "77777777-7777-7777-7777-777777777777"},
	))

	// Runtime role must fail to write nvidia_models with SQLSTATE 42501.
	deniedPrefs := &modelPrefs{catalogRead: appPool, catalogWrite: appPool}
	_, err := deniedPrefs.rescanNvidiaCatalog(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected SQLSTATE 42501 with runtime role, got: %v", err)
	}

	// Maintenance role must succeed in writing nvidia_models.
	prefsA := &modelPrefs{catalogRead: appPool, catalogWrite: maintPool}
	res, err := prefsA.rescanNvidiaCatalog(ctx)
	if err != nil {
		t.Fatalf("rescan with maintenance role: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("res.Added = %d, want 1", res.Added)
	}
}
