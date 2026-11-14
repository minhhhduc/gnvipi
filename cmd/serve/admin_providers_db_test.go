package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

const dbProviderSecret = "sk-issue204-secret-abcd1234"

func providersAPIPath(id string) string {
	if id == "" {
		return "/admin/api/providers"
	}
	return "/admin/api/providers/" + id
}

func doJSONRequest(engine *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(raw))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func createDBProvider(t *testing.T, h *dbAdminHarness, body gin.H) map[string]any {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), body, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create provider = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	return decodeBody(t, res)
}

func createDBProviderKey(t *testing.T, h *dbAdminHarness, providerID string, body gin.H) map[string]any {
	t.Helper()
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(providerID)+"/keys", body, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create provider key = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	return decodeBody(t, res)
}

func providerAuditActions(t *testing.T, h *dbAdminHarness, providerID string) []string {
	t.Helper()
	rows, err := h.pool.Query(t.Context(), `
		SELECT action FROM audit_log WHERE provider_id = $1 ORDER BY id`, providerID)
	if err != nil {
		t.Fatalf("read provider audit rows: %v", err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan provider audit action: %v", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read provider audit rows: %v", err)
	}
	return actions
}

func configVersion(t *testing.T, h *dbAdminHarness) int64 {
	t.Helper()
	var version int64
	if err := h.pool.QueryRow(t.Context(), `SELECT version FROM config_state WHERE id`).Scan(&version); err != nil {
		t.Fatalf("read config_state.version: %v", err)
	}
	return version
}

func providerNameEnabled(t *testing.T, h *dbAdminHarness, providerID string) (string, bool) {
	t.Helper()
	var name string
	var enabled bool
	if err := h.pool.QueryRow(t.Context(), `SELECT name, enabled FROM providers WHERE id = $1`, providerID).Scan(&name, &enabled); err != nil {
		t.Fatalf("read provider %s: %v", providerID, err)
	}
	return name, enabled
}

// TestDBAdminProviders_V2CRUDAndAudit drives create/list/get/update/delete and
// asserts each write leaves exactly one audit row in the same transaction.
func TestDBAdminProviders_V2CRUDAndAudit(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-crud"
	base := gin.H{
		"id":       providerID,
		"name":     "Issue 204",
		"protocol": "openai-chat",
		"base_url": "https://issue204.example/v1",
		"enabled":  true,
		"models":   []any{},
	}

	created := createDBProvider(t, h, base)
	if created["id"] != providerID || created["name"] != "Issue 204" || created["enabled"] != true {
		t.Fatalf("created provider = %v", created)
	}
	assertNoKeySecretMaterial(t, created)
	if authBlock, _ := created["auth"].(map[string]any); authBlock["scheme"] != "none" {
		t.Fatalf("keyless provider auth = %v, want none", created["auth"])
	}

	res := makeRequest(t, h.engine, http.MethodGet, providersAPIPath(""), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("list providers = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode provider list: %v", err)
	}
	found := false
	for _, item := range list {
		if item["id"] == providerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider list missing %s: %s", providerID, res.Body.String())
	}

	res = makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("get provider = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	assertNoKeySecretMaterial(t, decodeBody(t, res))

	res = makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), base, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate provider = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "id")

	update := gin.H{
		"id":       providerID,
		"name":     "Issue 204 renamed",
		"protocol": "openai-chat",
		"base_url": "https://issue204.example/v2",
		"enabled":  false,
		"models":   []any{},
	}
	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), update, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("update provider = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	updated := decodeBody(t, res)
	if updated["name"] != "Issue 204 renamed" || updated["enabled"] != false {
		t.Fatalf("updated provider = %v", updated)
	}
	name, enabled := providerNameEnabled(t, h, providerID)
	if name != "Issue 204 renamed" || enabled {
		t.Fatalf("stored provider = %q enabled=%v, want the committed update", name, enabled)
	}

	res = makeRequest(t, h.engine, http.MethodDelete, providersAPIPath(providerID), nil, bearer(h.token))
	if res.Code != http.StatusOK || decodeBody(t, res)["deleted"] != providerID {
		t.Fatalf("delete provider = %d %s", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(h.token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("get deleted provider = %d, want 404 (body %s)", res.Code, res.Body.String())
	}

	if actions := providerAuditActions(t, h, providerID); len(actions) != 3 ||
		actions[0] != "provider.create" || actions[1] != "provider.update" || actions[2] != "provider.delete" {
		t.Fatalf("provider audit actions = %v, want create/update/delete", actions)
	}
	var leaked int
	if err := h.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_log WHERE details::text LIKE '%' || $1 || '%'`,
		dbProviderSecret).Scan(&leaked); err != nil {
		t.Fatalf("scan audit leakage: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d audit rows contain a credential", leaked)
	}
}

// TestDBAdminProviders_LegacyViewAndModelsWire pins the v2 projection and the
// single-key models write, including the multi-key 409 and alias field error.
func TestDBAdminProviders_LegacyViewAndModelsWire(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-v2"
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Issue 204 v2", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	})

	key := createDBProviderKey(t, h, providerID, gin.H{
		"name": "v2 key", "auth_scheme": "header", "auth_header_name": "X-Issue204",
		"secret": dbProviderSecret,
	})
	keyID, _ := key["id"].(string)

	res := makeRequest(t, h.engine, http.MethodGet, providersAPIPath(providerID), nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("get provider = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	view := decodeBody(t, res)
	authBlock, _ := view["auth"].(map[string]any)
	if authBlock["scheme"] != "x-api-key" || authBlock["header_name"] != "X-Issue204" {
		t.Fatalf("legacy auth = %v, want x-api-key with the custom header", view["auth"])
	}
	if _, present := authBlock["key"]; present {
		t.Fatalf("legacy view returned auth.key: %v", authBlock)
	}
	assertNoKeySecretMaterial(t, view)

	models := []any{
		gin.H{"alias": "issue204/*", "upstream_model": ""},
		gin.H{"alias": "gpt-issue204", "upstream_model": "gpt-4o"},
	}
	body := gin.H{
		"id": providerID, "name": "Issue 204 v2", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true, "models": models,
	}
	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), body, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("provider models write = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	got := decodeBody(t, res)["models"].([]any)
	if len(got) != 2 {
		t.Fatalf("legacy models = %v, want the two written mappings", got)
	}
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM key_models WHERE key_id = $1::uuid`, keyID).Scan(&count); err != nil {
		t.Fatalf("count key models: %v", err)
	}
	if count != 2 {
		t.Fatalf("key_models rows = %d, want 2", count)
	}

	// A repeated alias inside one provider-level write is a field error, and the
	// failed write leaves the previous mappings intact.
	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), gin.H{
		"id": providerID, "name": "Issue 204 v2", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": []any{
			gin.H{"alias": "dup-alias", "upstream_model": "a"},
			gin.H{"alias": "dup-alias", "upstream_model": "b"},
		},
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate alias write = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "alias")

	createDBProviderKey(t, h, providerID, gin.H{
		"name": "v2 second", "auth_scheme": "header", "auth_header_name": "X-Second",
		"secret": dbProviderSecret,
	})

	res = makeRequest(t, h.engine, http.MethodPut, providersAPIPath(providerID), gin.H{
		"id": providerID, "name": "must not stick", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": []any{gin.H{"alias": "one-alias", "upstream_model": "m"}},
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("multi-key models write = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	conflict := decodeBody(t, res)
	assertFieldError(t, conflict, "models")
	if !bytes.Contains(res.Body.Bytes(), []byte("/keys/{keyID}/models")) {
		t.Fatalf("multi-key 409 lacks guidance: %s", res.Body.String())
	}
	if name, _ := providerNameEnabled(t, h, providerID); name != "Issue 204 v2" {
		t.Fatalf("provider name = %q after a refused write, want the previous value", name)
	}
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM key_models WHERE key_id = $1::uuid`, keyID).Scan(&count); err != nil {
		t.Fatalf("count key models: %v", err)
	}
	if count != 2 {
		t.Fatalf("key_models rows = %d after a refused write, want the two originals", count)
	}
}

// TestDBAdminProviders_TransactionRollback proves a rejected statement rolls
// back the provider row and its config version bump together.
func TestDBAdminProviders_TransactionRollback(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-rollback"

	before := configVersion(t, h)
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "Rollback", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
		"models": []any{gin.H{"alias": "rollback/model", "upstream_model": "m"}},
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("models without a key = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "models")
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM providers WHERE id = $1`, providerID).Scan(&count); err != nil {
		t.Fatalf("count providers: %v", err)
	}
	if count != 0 {
		t.Fatalf("providers = %d after a rejected create, want the whole transaction rolled back", count)
	}
	if got := configVersion(t, h); got != before {
		t.Fatalf("config_state.version = %d after a rolled-back create, want %d", got, before)
	}

	// A duplicate id fails inside the same transaction and must not leak an
	// audit row or a version bump.
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Rollback", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	})
	settled := configVersion(t, h)
	res = makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "Duplicate", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	if got := configVersion(t, h); got != settled {
		t.Fatalf("config_state.version = %d after a refused create, want %d", got, settled)
	}
	if actions := providerAuditActions(t, h, providerID); len(actions) != 1 || actions[0] != "provider.create" {
		t.Fatalf("audit actions = %v after a refused create, want only the first create", actions)
	}
}

// TestDBAdminProviders_SnapshotRefreshesAfterCommit proves the published
// snapshot advances only once the admin write commits, driven by the config
// version bump and the existing watcher.
func TestDBAdminProviders_SnapshotRefreshesAfterCommit(t *testing.T) {
	h := newDBAdminHarness(t)
	ctx := t.Context()
	before := listenerSessions(t, ctx, h.pool)
	runtime, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(h.pool), providerRuntimeOptions{
		ListenPool:   h.pool,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("open provider runtime: %v", err)
	}
	runtime.healthTTL = -1
	t.Cleanup(runtime.Close)
	waitForListenerSessions(t, ctx, h.pool, before+1)

	baseline := runtime.health(ctx).SnapshotVersion
	const providerID = "issue204-watch"
	res := makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "Watch", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	}, bearer(h.token))
	if res.Code != http.StatusCreated {
		t.Fatalf("create provider = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	committed := configVersion(t, h)
	if committed <= baseline {
		t.Fatalf("config_state.version = %d after a committed write, want > %d", committed, baseline)
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.health(ctx).SnapshotVersion != committed {
		if time.Now().After(deadline) {
			t.Fatalf("snapshot version = %d after 10s, want %d (last error: %v)",
				runtime.health(ctx).SnapshotVersion, committed, runtime.store.LastError())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := runtime.store.Current().Provider(providerID); !ok {
		t.Fatal("snapshot is missing the provider written by the admin API")
	}

	// A refused write never bumps the version, so the snapshot must not move.
	res = makeRequest(t, h.engine, http.MethodPost, providersAPIPath(""), gin.H{
		"id": providerID, "name": "Duplicate", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	}, bearer(h.token))
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	if got := configVersion(t, h); got != committed {
		t.Fatalf("config_state.version = %d after a refused write, want %d", got, committed)
	}
	if got := runtime.store.Version(); got != committed {
		t.Fatalf("snapshot version = %d after a refused write, want %d", got, committed)
	}
}

// TestDBAdminProviders_AliasConcurrency races two owners' aliases in the same
// owner scope; the unique index admits exactly one.
func TestDBAdminProviders_AliasConcurrency(t *testing.T) {
	h := newDBAdminHarness(t)
	const providerID = "issue204-alias"
	createDBProvider(t, h, gin.H{
		"id": providerID, "name": "Alias race", "protocol": "openai-chat",
		"base_url": "https://issue204.example/v1", "enabled": true,
	})
	first := createDBProviderKey(t, h, providerID, gin.H{"name": "race one", "secret": dbProviderSecret})
	second := createDBProviderKey(t, h, providerID, gin.H{"name": "race two", "secret": dbProviderSecret + "-two"})
	paths := []string{
		providersAPIPath(providerID) + "/keys/" + first["id"].(string) + "/models",
		providersAPIPath(providerID) + "/keys/" + second["id"].(string) + "/models",
	}
	payload := gin.H{"models": []any{gin.H{"alias": "race-alias", "upstream_model": "shared"}}}

	results := make([]*httptest.ResponseRecorder, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			results[i] = doJSONRequest(h.engine, http.MethodPut, path, payload, bearer(h.token))
		}(i, path)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, res := range results {
		switch res.Code {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
			if fields := fieldErrorFields(decodeBody(t, res)); len(fields) != 1 || fields[0] != "alias" {
				t.Fatalf("conflict field_errors = %v, want [alias] (body %s)", fields, res.Body.String())
			}
		default:
			t.Fatalf("concurrent alias write = %d (body %s)", res.Code, res.Body.String())
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("alias race = %d ok / %d conflict, want exactly one of each", okCount, conflictCount)
	}
}

// TestDBAdminUsers_LastOwnerConcurrentDemotions queues a demotion behind the
// advisory lock the last-owner trigger takes; exactly one demotion can win.
func TestDBAdminUsers_LastOwnerConcurrentDemotions(t *testing.T) {
	h := newDBAdminHarness(t)
	secondOwnerID := createContractUser(t, h, "owner-two", auth.RoleOwner)
	clearMustChangePassword(t, h, secondOwnerID)
	ctx := t.Context()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('gnvipi:owners'))`); err != nil {
		t.Fatalf("hold last-owner lock: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doJSONRequest(h.engine, http.MethodPut, "/admin/api/users/"+h.owner.ID, gin.H{"role": "admin"}, bearer(h.token))
	}()

	// Demote the second owner while holding the serialization lock.
	if _, err := tx.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1::uuid`, secondOwnerID); err != nil {
		t.Fatalf("demote second owner under the lock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit lock holder: %v", err)
	}

	res := <-done
	if res.Code != http.StatusConflict {
		t.Fatalf("queued demotion = %d, want 409 (body %s)", res.Code, res.Body.String())
	}
	assertFieldError(t, decodeBody(t, res), "role")
	var role string
	if err := h.pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1::uuid`, h.owner.ID).Scan(&role); err != nil {
		t.Fatalf("read remaining owner role: %v", err)
	}
	if role != string(auth.RoleOwner) {
		t.Fatalf("last owner role = %q, want owner", role)
	}
}
