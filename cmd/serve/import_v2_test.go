package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

const importOwnerID = "11111111-1111-4111-8111-111111111111"

func TestRunCLICommandDispatchesImportV2(t *testing.T) {
	wantErr := errors.New("import stopped")
	var got []string
	handled, err := runCLICommand(
		[]string{"admin", "import-v2", "--owner", "legacy-owner", "--force"},
		nil,
		nil,
		func(_ context.Context, args []string, _ func(string) (string, bool)) error {
			got = append([]string(nil), args...)
			return wantErr
		},
	)
	if !handled || !errors.Is(err, wantErr) {
		t.Fatalf("import dispatch = handled %v err %v, want handled sentinel", handled, err)
	}
	want := []string{"--owner", "legacy-owner", "--force"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("import args = %v, want %v", got, want)
	}
}

func TestReadV2SourceCreatesImmutableBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	first := []byte(`{"schema_version":2,"providers_v2":[]}`)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readV2Source(path)
	if err != nil {
		t.Fatalf("read v2 source: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("source bytes = %q, want %q", got, first)
	}
	backupPath := path + ".v2.bak"
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(backup, first) {
		t.Fatalf("backup bytes = %q, want %q", backup, first)
	}

	second := []byte(`{"schema_version":2,"providers_v2":[{"id":"changed"}]}`)
	if err := os.WriteFile(path, second, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = readV2Source(path)
	if err != nil {
		t.Fatalf("reread v2 source: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("reread source bytes = %q, want %q", got, second)
	}
	backup, err = os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("reread backup: %v", err)
	}
	if !bytes.Equal(backup, first) {
		t.Fatalf("immutable backup changed to %q", backup)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, second) {
		t.Fatalf("source was modified to %q", source)
	}
}

func TestImportV2RoundTripScopesModelsAndAPIKeyMapping(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool, "legacy-owner")
	ring := importTestKeyring(t)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [
			{
				"id": "shared",
				"name": "Shared",
				"base_url": "https://shared.example/v1",
				"protocol": "openai-chat",
				"auth": {"scheme": "none"},
				"models": [{"alias": "shared/a", "upstream_model": "a", "legacy_position": 1}],
				"timeouts": {},
				"enabled": true,
				"tls": "default"
			},
			{
				"id": "anthropic",
				"name": "Anthropic",
				"base_url": "https://anthropic.example/v1",
				"protocol": "openai-chat",
				"auth": {"scheme": "x-api-key", "key": "literal-import-secret"},
				"models": [{"alias": "anthropic/b", "upstream_model": "b", "legacy_position": 2}],
				"timeouts": {},
				"enabled": true,
				"tls": "default"
			},
			{
				"id": "custom-header",
				"name": "Custom Header",
				"base_url": "https://custom.example/v1",
				"protocol": "openai-chat",
				"auth": {"scheme": "x-api-key", "header_name": "X-Custom-Key", "key_env": "GNVIPI_KEY_CUSTOM"},
				"models": [{"alias": "custom/c", "upstream_model": "c", "legacy_position": 3}],
				"timeouts": {},
				"enabled": true,
				"tls": "default"
			}
		]
	}`)

	imported, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, ring)
	if err != nil {
		t.Fatalf("import v2: %v", err)
	}
	if !imported {
		t.Fatal("first import reported a no-op")
	}

	rows, _, err := providerstore.NewDBRowSource(pool).LoadConfig(t.Context())
	if err != nil {
		t.Fatalf("load imported config: %v", err)
	}
	if len(rows.Providers) != 3 || len(rows.Keys) != 3 || len(rows.KeyModels) != 3 {
		t.Fatalf("imported row counts = providers %d keys %d models %d, want 3/3/3", len(rows.Providers), len(rows.Keys), len(rows.KeyModels))
	}

	keys := make(map[string]providerstore.KeyRow, len(rows.Keys))
	for _, key := range rows.Keys {
		keys[key.ProviderID] = key
	}
	if key := keys["shared"]; key.OwnerID != "" || key.AuthScheme != providerstore.SchemeNone {
		t.Fatalf("shared key = %+v, want ownerless none", key)
	}
	apiKey := keys["anthropic"]
	if apiKey.OwnerID != importOwnerID || apiKey.AuthScheme != providerstore.SchemeHeader || apiKey.AuthHeaderName != "x-api-key" || !apiKey.HasSecret() {
		t.Fatalf("x-api-key row = %+v", apiKey)
	}
	resolved, err := providerstore.ResolveKeyCredential(t.Context(), apiKey, ring, nil)
	if err != nil || resolved.Raw != "literal-import-secret" {
		t.Fatalf("resolved imported secret = %q, %v", resolved.Raw, err)
	}
	custom := keys["custom-header"]
	if custom.OwnerID != importOwnerID || custom.AuthScheme != providerstore.SchemeHeader || custom.AuthHeaderName != "X-Custom-Key" || custom.KeyEnv != "GNVIPI_KEY_CUSTOM" {
		t.Fatalf("custom header row = %+v", custom)
	}

	views := importedLegacyViews(t, &rows)
	if got := views["shared"]; got.Auth.Scheme != provider.AuthNone || len(got.Models) != 1 || got.Models[0].LegacyPosition != 1 {
		t.Fatalf("shared LegacyView = %+v", got)
	}
	if got := views["anthropic"]; got.Auth.Scheme != provider.AuthAPIKey || got.Auth.HeaderName != "" || got.Auth.Key != "" || got.Models[0].LegacyPosition != 2 {
		t.Fatalf("x-api-key LegacyView = %+v", got)
	}
	if got := views["custom-header"]; got.Auth.Scheme != provider.AuthAPIKey || got.Auth.HeaderName != "X-Custom-Key" || got.Auth.KeyEnv != "GNVIPI_KEY_CUSTOM" || got.Models[0].LegacyPosition != 3 {
		t.Fatalf("custom header LegacyView = %+v", got)
	}
}

func TestImportV2IdempotencyChangedSourceAndForce(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool, "legacy-owner")
	ring := importTestKeyring(t)
	first := importDocument(t, "first", "First")

	imported, err := providerstore.ImportV2(t.Context(), pool, first, "legacy-owner", false, ring)
	if err != nil || !imported {
		t.Fatalf("first import = imported %v err %v", imported, err)
	}
	var firstHash, firstImportedAt, markerOwner string
	if err := pool.QueryRow(t.Context(), `SELECT value->>'source_sha256', value->>'imported_at', value->>'owner' FROM system_state WHERE key='v2_import'`).Scan(&firstHash, &firstImportedAt, &markerOwner); err != nil {
		t.Fatalf("read import marker: %v", err)
	}
	wantSum := sha256.Sum256(first)
	if firstHash != hex.EncodeToString(wantSum[:]) || firstImportedAt == "" || markerOwner != "legacy-owner" {
		t.Fatalf("import marker = hash %q at %q owner %q", firstHash, firstImportedAt, markerOwner)
	}

	imported, err = providerstore.ImportV2(t.Context(), pool, first, "legacy-owner", false, ring)
	if err != nil || imported {
		t.Fatalf("same-source import = imported %v err %v, want no-op", imported, err)
	}
	var sameImportedAt string
	if err := pool.QueryRow(t.Context(), `SELECT value->>'imported_at' FROM system_state WHERE key='v2_import'`).Scan(&sameImportedAt); err != nil {
		t.Fatal(err)
	}
	if sameImportedAt != firstImportedAt {
		t.Fatalf("same-source no-op changed imported_at from %q to %q", firstImportedAt, sameImportedAt)
	}

	changed := importDocument(t, "second", "Second")
	if _, err := providerstore.ImportV2(t.Context(), pool, changed, "legacy-owner", false, ring); !errors.Is(err, providerstore.ErrV2SourceChanged) {
		t.Fatalf("changed source error = %v, want ErrV2SourceChanged", err)
	}
	var currentID string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM providers`).Scan(&currentID); err != nil || currentID != "first" {
		t.Fatalf("rejected changed source left provider %q, err %v", currentID, err)
	}

	imported, err = providerstore.ImportV2(t.Context(), pool, changed, "legacy-owner", true, ring)
	if err != nil || !imported {
		t.Fatalf("forced changed import = imported %v err %v", imported, err)
	}
	var ids []string
	rows, err := pool.Query(t.Context(), `SELECT id FROM providers ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if !reflect.DeepEqual(ids, []string{"second"}) {
		t.Fatalf("forced source providers = %v, want only second", ids)
	}
}

func TestImportV2RollsBackEveryRowOnFailure(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool, "legacy-owner")
	ring := importTestKeyring(t)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [
			{"id":"valid","name":"Valid","base_url":"https://valid.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"valid/m","upstream_model":"m"}],"timeouts":{},"enabled":true},
			{"id":"BadID","name":"Invalid DB ID","base_url":"https://invalid.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"invalid/m","upstream_model":"m"}],"timeouts":{},"enabled":true}
		]
	}`)

	if _, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, ring); err == nil {
		t.Fatal("import with a DB-invalid second provider succeeded")
	}
	for _, table := range []string{"providers", "provider_keys", "key_models"} {
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after rollback = %d, want 0", table, count)
		}
	}
	var marker []byte
	if err := pool.QueryRow(t.Context(), `SELECT value FROM system_state WHERE key='v2_import'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if string(marker) != "{}" {
		t.Fatalf("import marker after rollback = %s, want {}", marker)
	}
}

func requiredImportTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set")
	}
	return dsn
}

func createImportOwner(t *testing.T, pool *pgxpool.Pool, username string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id, username, role, password_hash) VALUES ($1::uuid, $2, 'owner', 'test-hash')`, importOwnerID, username); err != nil {
		t.Fatalf("create import owner: %v", err)
	}
}

func importTestKeyring(t *testing.T) *providerstore.Keyring {
	t.Helper()
	ring, err := providerstore.NewKeyring(7, bytes.Repeat([]byte{0x42}, providerstore.MasterKeySize), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func importDocument(t *testing.T, id, name string) []byte {
	t.Helper()
	doc := map[string]any{
		"schema_version": 2,
		"providers_v2": []any{map[string]any{
			"id": id, "name": name, "base_url": "https://" + id + ".example/v1",
			"protocol": "openai-chat", "auth": map[string]any{"scheme": "none"},
			"models":   []any{map[string]any{"alias": id + "/m", "upstream_model": "m"}},
			"timeouts": map[string]any{}, "enabled": true,
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func importedLegacyViews(t *testing.T, rows *providerstore.Rows) map[string]provider.Provider {
	t.Helper()
	out := make(map[string]provider.Provider, len(rows.Providers))
	for i := range rows.Providers {
		row := &rows.Providers[i]
		var keys []providerstore.KeyRow
		var models []providerstore.KeyModelRow
		for j := range rows.Keys {
			key := &rows.Keys[j]
			if key.ProviderID == row.ID {
				keys = append(keys, *key)
			}
		}
		for j := range rows.KeyModels {
			model := &rows.KeyModels[j]
			if model.ProviderID == row.ID {
				models = append(models, *model)
			}
		}
		view, err := providerstore.LegacyView(*row, keys, models, providerstore.LegacyViewOptions{IncludeKeyEnv: true, IncludeKeyRef: true})
		if err != nil {
			t.Fatalf("LegacyView %s: %v", row.ID, err)
		}
		out[row.ID] = view
	}
	return out
}
