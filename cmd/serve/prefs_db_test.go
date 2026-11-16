package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
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
	if action != "model.state.update" || strings.TrimSpace(userID) != h.superuser.ID {
		t.Fatalf("audit row = %q by %q, want model.state.update by %s", action, userID, h.superuser.ID)
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

// The captcha champion is runtime state, so database mode stores it in
// system_state instead of the user-cache JSON file: a database deployment must
// survive a restart without writing state files, and two instances share one
// selection.
func TestDBChampionStateRoundTrip(t *testing.T) {
	h := newDBAdminHarness(t)
	ctx := t.Context()
	db := &appDB{pool: h.pool}

	if got := loadChampionState(ctx, db); got.ChampionURL != "" {
		t.Fatalf("champion before any benchmark = %q, want empty", got.ChampionURL)
	}

	saved := &captcha.ChampionState{
		ChampionURL:  "https://playground.example.com/chat",
		ScoreMS:      812,
		SuccessRate:  0.75,
		LastOK:       time.Unix(1_700_000_000, 0).UTC(),
		FailStreak:   1,
		FallbackURLs: []string{"https://backup-a.example.com/chat", "https://backup-b.example.com/chat"},
	}
	saveChampionState(ctx, db, saved)

	var stored int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM system_state WHERE key = $1`, championStateKey).Scan(&stored); err != nil {
		t.Fatalf("count champion documents: %v", err)
	}
	if stored != 1 {
		t.Fatalf("champion documents = %d, want 1", stored)
	}

	loaded := loadChampionState(ctx, db)
	if loaded.ChampionURL != saved.ChampionURL || loaded.ScoreMS != saved.ScoreMS ||
		loaded.SuccessRate != saved.SuccessRate || loaded.FailStreak != saved.FailStreak {
		t.Fatalf("loaded champion = %+v, want %+v", loaded, saved)
	}
	if len(loaded.FallbackURLs) != len(saved.FallbackURLs) || loaded.FallbackURLs[1] != saved.FallbackURLs[1] {
		t.Fatalf("loaded fallbacks = %v, want %v", loaded.FallbackURLs, saved.FallbackURLs)
	}
	if loaded.Updated.IsZero() {
		t.Fatal("loaded champion has no updated timestamp")
	}

	// A second benchmark overwrites the single document rather than appending.
	second := &captcha.ChampionState{ChampionURL: "https://next.example.com/chat", ScoreMS: 640}
	saveChampionState(ctx, db, second)
	if got := loadChampionState(ctx, db); got.ChampionURL != second.ChampionURL || len(got.FallbackURLs) != 0 {
		t.Fatalf("champion after rebenchmark = %+v, want %+v", got, second)
	}
}

func TestDBSettings_TwoInstanceSequentialAndRemoteRefresh(t *testing.T) {
	h := newDBAdminHarness(t)
	ctx := t.Context()

	prefsA := &modelPrefs{dbSettings: &dbSettingsStore{pool: h.pool}}
	runtimeA, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(h.pool), providerRuntimeOptions{
		OnPublish: prefsA.applySnapshotDB,
	})
	if err != nil {
		t.Fatalf("open runtimeA: %v", err)
	}
	t.Cleanup(runtimeA.Close)
	prefsA.dbSettings.runtime = runtimeA

	prefsB := &modelPrefs{dbSettings: &dbSettingsStore{pool: h.pool}}
	runtimeB, err := openProviderRuntime(ctx, providerstore.NewDBRowSource(h.pool), providerRuntimeOptions{
		OnPublish: prefsB.applySnapshotDB,
	})
	if err != nil {
		t.Fatalf("open runtimeB: %v", err)
	}
	t.Cleanup(runtimeB.Close)
	prefsB.dbSettings.runtime = runtimeB

	// Instance A hides model X.
	if err := prefsA.setHiddenAs(nil, []string{"nvidia/masked-x"}); err != nil {
		t.Fatalf("A setHiddenAs: %v", err)
	}

	// Before B reloads its snapshot (B's local hidden set is still empty),
	// B saves a search URL. This must NOT erase A's committed mask.
	if err := prefsB.setSearchURLAs(nil, "https://search.example.test/?q={query}"); err != nil {
		t.Fatalf("B setSearchURLAs: %v", err)
	}

	hidden, _, err := providerstore.LoadModelState(ctx, h.pool)
	if err != nil {
		t.Fatalf("load model state: %v", err)
	}
	if len(hidden) != 1 || hidden[0] != "nvidia/masked-x" {
		t.Fatalf("committed hidden = %v, want [nvidia/masked-x]", hidden)
	}
	if !prefsB.isHidden("nvidia/masked-x") {
		t.Fatal("B lost A's hidden model in local state")
	}

	// Instance A reloads its snapshot and must observe B's search URL while
	// keeping its own mask.
	if err := runtimeA.store.Reload(ctx); err != nil {
		t.Fatalf("A reload: %v", err)
	}
	if got := prefsA.searchTemplate(); got != "https://search.example.test/?q={query}" {
		t.Fatalf("A searchTemplate after reload = %q, want B's URL", got)
	}
	if !prefsA.isHidden("nvidia/masked-x") {
		t.Fatal("A lost hidden model after reload")
	}
}
