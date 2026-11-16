package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// dbSettingsHarness wires the admin API with database-mode settings on top of
// the real schema, so the mask, deletions and search template are exercised
// against PostgreSQL rows rather than the models file.
func newDBSettingsHarness(t *testing.T) *dbAdminHarness {
	t.Helper()
	h := newDBAdminHarness(t)
	h.prefs.dbSettings = &dbSettingsStore{pool: h.pool}
	return h
}

func TestDBSettings_MaskDeleteSearchRoundTrip(t *testing.T) {
	h := newDBSettingsHarness(t)
	ctx := t.Context()

	var version int64
	if err := h.pool.QueryRow(ctx, `SELECT version FROM config_state`).Scan(&version); err != nil {
		t.Fatalf("read config version: %v", err)
	}

	// Masking writes model_state and must bump the config version, which is what
	// makes every process republish the routing snapshot.
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/models",
		gin.H{"masks_id": []string{"nvidia/masked-a", "nvidia/masked-b"}}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("mask models = %d (body %s)", res.Code, res.Body.String())
	}
	var hidden int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM model_state WHERE hidden`).Scan(&hidden); err != nil {
		t.Fatalf("count hidden rows: %v", err)
	}
	if hidden != 2 {
		t.Fatalf("hidden rows = %d, want 2", hidden)
	}
	var bumped int64
	if err := h.pool.QueryRow(ctx, `SELECT version FROM config_state`).Scan(&bumped); err != nil {
		t.Fatalf("read config version: %v", err)
	}
	if bumped <= version {
		t.Fatalf("config version = %d, want > %d after a mask write", bumped, version)
	}

	// Delete then restore: the intent row is pruned once it carries no flag.
	res = makeRequest(t, h.engine, http.MethodDelete, "/admin/api/models/nvidia/gone", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("delete model = %d (body %s)", res.Code, res.Body.String())
	}
	var deleted bool
	if err := h.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM model_state WHERE model_id = $1`,
		"nvidia/gone").Scan(&deleted); err != nil {
		t.Fatalf("read deleted row: %v", err)
	}
	if !deleted {
		t.Fatal("deleted model has no deleted_at row")
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/models/nvidia/gone/restore", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("restore model = %d (body %s)", res.Code, res.Body.String())
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM model_state WHERE model_id = $1`,
		"nvidia/gone").Scan(&rows); err != nil {
		t.Fatalf("count restored row: %v", err)
	}
	if rows != 0 {
		t.Fatalf("restored model still has %d model_state row(s)", rows)
	}

	// The search template lives in settings and is read back verbatim.
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/config/search",
		gin.H{"url": "https://search.example.test/?q={query}"}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("set search template = %d (body %s)", res.Code, res.Body.String())
	}
	var stored string
	if err := h.pool.QueryRow(ctx, `SELECT value->>'url' FROM settings WHERE key = 'search'`).Scan(&stored); err != nil {
		t.Fatalf("read search setting: %v", err)
	}
	if stored != "https://search.example.test/?q={query}" {
		t.Fatalf("stored search url = %q", stored)
	}

	// A fresh process must observe exactly the committed state.
	restarted := &modelPrefs{dbSettings: &dbSettingsStore{pool: h.pool}}
	if err := restarted.loadStateDB(context.Background()); err != nil {
		t.Fatalf("load state from database: %v", err)
	}
	if !restarted.isHidden("nvidia/masked-a") || !restarted.isHidden("nvidia/masked-b") {
		t.Fatalf("mask not restored: %v", restarted.hidden)
	}
	if restarted.isDeleted("nvidia/gone") {
		t.Fatal("restored model still reported as deleted")
	}
	if got := restarted.searchTemplate(); got != "https://search.example.test/?q={query}" {
		t.Fatalf("search template after reload = %q", got)
	}
}

func TestDBSettings_AuditRecordsMaskChange(t *testing.T) {
	h := newDBSettingsHarness(t)
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/models",
		gin.H{"masks_id": []string{"nvidia/masked"}}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("mask models = %d (body %s)", res.Code, res.Body.String())
	}
	var action, actorType, userID string
	if err := h.pool.QueryRow(t.Context(), `SELECT action, actor_type, coalesce(user_id::text, '')
		FROM audit_log WHERE action = 'model.state.update' ORDER BY ts DESC LIMIT 1`).
		Scan(&action, &actorType, &userID); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if action != "model.state.update" || strings.TrimSpace(userID) != h.owner.ID {
		t.Fatalf("audit row = %q by %q, want model.state.update by %s", action, userID, h.owner.ID)
	}
	if actorType != "token" {
		t.Fatalf("audit actor_type = %q, want token", actorType)
	}
}

func TestDBSettings_EmptyMaskClearsRows(t *testing.T) {
	h := newDBSettingsHarness(t)
	ctx := t.Context()
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/models",
		gin.H{"masks_id": []string{"nvidia/a", "nvidia/b"}}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("mask models = %d (body %s)", res.Code, res.Body.String())
	}
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/models",
		gin.H{"masks_id": []string{}}, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("clear mask = %d (body %s)", res.Code, res.Body.String())
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM model_state`).Scan(&rows); err != nil {
		t.Fatalf("count model_state rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("model_state still has %d row(s) after clearing the mask", rows)
	}
	if h.prefs.isHidden("nvidia/a") {
		t.Fatal("cleared mask still hides a model in memory")
	}
}
