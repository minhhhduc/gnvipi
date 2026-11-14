package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// These tests need a disposable PostgreSQL database (GNVIPI_TEST_DATABASE_URL)
// and skip otherwise. The harness installs the production schema, including
// 0003_nvidia_catalog.

const (
	dbCatalogAlpha = "fixture/alpha"
	dbCatalogBeta  = "fixture/beta"
)

func catalogRow(model, functionID string) models.PlaygroundRow {
	return models.PlaygroundRow{
		Model:      model,
		Slug:       model[len("fixture/"):],
		Namespace:  models.Namespace,
		FunctionID: functionID,
	}
}

func commitCatalog(t *testing.T, h *dbAdminHarness, rows ...models.PlaygroundRow) {
	t.Helper()
	tx, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin catalog transaction: %v", err)
	}
	if err := providerstore.ReplaceNvidiaCatalog(t.Context(), tx, rows); err != nil {
		_ = tx.Rollback(context.Background()) //nolint:errcheck // test cleanup
		t.Fatalf("replace catalog: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit catalog: %v", err)
	}
}

func storedCatalog(t *testing.T, h *dbAdminHarness) []models.PlaygroundRow {
	t.Helper()
	rows, err := providerstore.LoadNvidiaCatalog(t.Context(), h.pool)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return rows
}

func TestDBNvidiaCatalogEmptyTableIsEmpty(t *testing.T) {
	h := newDBAdminHarness(t)
	restoreModels(t)
	models.Replace(map[string]models.ModelInfo{
		dbCatalogAlpha: {Slug: "alpha", Namespace: models.Namespace, FunctionID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"},
	})

	if rows := storedCatalog(t, h); len(rows) != 0 {
		t.Fatalf("stored catalog = %v, want empty", rows)
	}
	if err := h.prefs.loadStoredNvidiaCatalog(t.Context()); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if got := len(models.Snapshot()); got != 0 {
		t.Fatalf("registry entries = %d, want an empty catalog", got)
	}
}

func TestDBNvidiaCatalogStartupLoadsFromTable(t *testing.T) {
	h := newDBAdminHarness(t)
	restoreModels(t)
	commitCatalog(t, h, catalogRow(dbCatalogAlpha, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	models.Replace(map[string]models.ModelInfo{})

	if err := h.prefs.loadStoredNvidiaCatalog(t.Context()); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	info, err := models.Lookup(dbCatalogAlpha)
	if err != nil {
		t.Fatalf("stored model is not routable after startup load: %v", err)
	}
	if info.Slug != "alpha" || info.Namespace != models.Namespace || info.FunctionID != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("loaded model = %+v", info)
	}
	var unknown *models.ErrUnknownModel
	if _, err := models.Lookup(dbCatalogBeta); !errors.As(err, &unknown) {
		t.Fatalf("unexpected model lookup = %v", err)
	}
}

func TestDBNvidiaCatalogReplaceIsAllOrNothing(t *testing.T) {
	h := newDBAdminHarness(t)
	restoreModels(t)
	commitCatalog(t, h, catalogRow(dbCatalogAlpha, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))

	cases := []struct {
		name string
		rows []models.PlaygroundRow
	}{
		{"incomplete row", []models.PlaygroundRow{{Model: dbCatalogBeta, Slug: "", Namespace: models.Namespace, FunctionID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}}},
		{"invalid uuid", []models.PlaygroundRow{catalogRow(dbCatalogBeta, "not-a-uuid")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := h.pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := providerstore.ReplaceNvidiaCatalog(t.Context(), tx, tc.rows); err == nil {
				_ = tx.Rollback(context.Background()) //nolint:errcheck // test cleanup
				t.Fatal("invalid catalog was accepted")
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			rows := storedCatalog(t, h)
			if len(rows) != 1 || rows[0].Model != dbCatalogAlpha {
				t.Fatalf("failed replace changed the table: %v", rows)
			}
		})
	}

	commitCatalog(t, h, catalogRow(dbCatalogBeta, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	rows := storedCatalog(t, h)
	if len(rows) != 1 || rows[0].Model != dbCatalogBeta {
		t.Fatalf("successful replace = %v, want only %s", rows, dbCatalogBeta)
	}
}

func TestDBNvidiaRescanWritesThenServes(t *testing.T) {
	h := newDBAdminHarness(t)
	restoreModels(t)
	models.Replace(map[string]models.ModelInfo{})

	stubNvidiaDiscovery(t, rescanFixture(catalogRow(dbCatalogAlpha, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")))
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("rescan = %d: %s", res.Code, res.Body.String())
	}
	rows := storedCatalog(t, h)
	if len(rows) != 1 || rows[0].Model != dbCatalogAlpha {
		t.Fatalf("stored catalog = %v", rows)
	}
	if _, err := models.Lookup(dbCatalogAlpha); err != nil {
		t.Fatalf("rescanned model is not routable without a restart: %v", err)
	}

	// Idempotent second rescan.
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("second rescan = %d: %s", res.Code, res.Body.String())
	}
	if rows := storedCatalog(t, h); len(rows) != 1 {
		t.Fatalf("second rescan changed the table: %v", rows)
	}

	// A retired model disappears from the table and stops being routable.
	stubNvidiaDiscovery(t, rescanFixture(catalogRow(dbCatalogBeta, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")))
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusOK {
		t.Fatalf("third rescan = %d: %s", res.Code, res.Body.String())
	}
	rows = storedCatalog(t, h)
	if len(rows) != 1 || rows[0].Model != dbCatalogBeta {
		t.Fatalf("reconciled catalog = %v", rows)
	}
	var unknown *models.ErrUnknownModel
	if _, err := models.Lookup(dbCatalogAlpha); !errors.As(err, &unknown) {
		t.Fatalf("retired model lookup = %v, want *ErrUnknownModel", err)
	}
	if _, err := models.Lookup(dbCatalogBeta); err != nil {
		t.Fatalf("new model is not routable: %v", err)
	}
}

func TestDBNvidiaRescanFailureLeavesTableUnchanged(t *testing.T) {
	h := newDBAdminHarness(t)
	restoreModels(t)
	commitCatalog(t, h, catalogRow(dbCatalogAlpha, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	if err := h.prefs.loadStoredNvidiaCatalog(t.Context()); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	stubNvidiaDiscovery(t, func(context.Context) ([]models.PlaygroundGroup, error) {
		return nil, &models.DiscoveryError{Op: "models page", Err: errors.New("status 503")}
	})
	res := makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("failed discovery = %d, want 502: %s", res.Code, res.Body.String())
	}
	rows := storedCatalog(t, h)
	if len(rows) != 1 || rows[0].Model != dbCatalogAlpha {
		t.Fatalf("failed discovery changed the table: %v", rows)
	}
	if _, err := models.Lookup(dbCatalogAlpha); err != nil {
		t.Fatalf("failed discovery changed the registry: %v", err)
	}

	// A row the database rejects leaves the committed catalog in place.
	stubNvidiaDiscovery(t, rescanFixture(catalogRow(dbCatalogBeta, "not-a-uuid")))
	res = makeRequest(t, h.engine, http.MethodPost, "/admin/api/nvidia/models/rescan", nil, bearer(h.token))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("failed write = %d, want 500: %s", res.Code, res.Body.String())
	}
	rows = storedCatalog(t, h)
	if len(rows) != 1 || rows[0].Model != dbCatalogAlpha {
		t.Fatalf("failed transaction changed the table: %v", rows)
	}
	if _, err := models.Lookup(dbCatalogBeta); err == nil {
		t.Fatal("failed transaction changed the registry")
	}
}
