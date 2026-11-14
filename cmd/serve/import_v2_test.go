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
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
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
	createImportOwner(t, pool)
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

	imported, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, ring, nil)
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
	createImportOwner(t, pool)
	ring := importTestKeyring(t)
	first := importDocument(t, "first", "First")

	imported, err := providerstore.ImportV2(t.Context(), pool, first, "legacy-owner", false, ring, nil)
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

	imported, err = providerstore.ImportV2(t.Context(), pool, first, "legacy-owner", false, ring, nil)
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
	if _, err := providerstore.ImportV2(t.Context(), pool, changed, "legacy-owner", false, ring, nil); !errors.Is(err, providerstore.ErrV2SourceChanged) {
		t.Fatalf("changed source error = %v, want ErrV2SourceChanged", err)
	}
	var currentID string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM providers`).Scan(&currentID); err != nil || currentID != "first" {
		t.Fatalf("rejected changed source left provider %q, err %v", currentID, err)
	}

	imported, err = providerstore.ImportV2(t.Context(), pool, changed, "legacy-owner", true, ring, nil)
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

func TestImportV2ForcePreservesUnimportedProviders(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	ring := importTestKeyring(t)
	if _, err := providerstore.ImportV2(t.Context(), pool, importDocument(t, "first", "First"), "legacy-owner", false, ring, nil); err != nil {
		t.Fatalf("first import: %v", err)
	}
	seedDatabaseProvider(t, pool, "manual")

	if _, err := providerstore.ImportV2(t.Context(), pool, importDocument(t, "second", "Second"), "legacy-owner", true, ring, nil); err != nil {
		t.Fatalf("forced import: %v", err)
	}
	if got, want := databaseProviderIDs(t, pool), []string{"manual", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers after forced import = %v, want %v", got, want)
	}
	var manualKeys int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM provider_keys WHERE provider_id = 'manual'`).Scan(&manualKeys); err != nil {
		t.Fatal(err)
	}
	if manualKeys != 1 {
		t.Fatalf("database-managed provider keys after forced import = %d, want 1", manualKeys)
	}
}

func TestImportV2ForceRejectsProviderOwnedByDatabase(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	ring := importTestKeyring(t)
	if _, err := providerstore.ImportV2(t.Context(), pool, importDocument(t, "first", "First"), "legacy-owner", false, ring, nil); err != nil {
		t.Fatalf("first import: %v", err)
	}
	seedDatabaseProvider(t, pool, "manual")

	_, err := providerstore.ImportV2(t.Context(), pool, importDocument(t, "manual", "Conflicting"), "legacy-owner", true, ring, nil)
	if !errors.Is(err, providerstore.ErrV2ProviderConflict) || !strings.Contains(err.Error(), "manual") {
		t.Fatalf("conflicting forced import error = %v, want ErrV2ProviderConflict naming manual", err)
	}
	if got, want := databaseProviderIDs(t, pool), []string{"first", "manual"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers after rejected forced import = %v, want %v", got, want)
	}
}

func TestImportV2RejectsBearerHeaderOverride(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [{
			"id":"custom-bearer",
			"name":"Custom Bearer",
			"base_url":"https://bearer.example/v1",
			"protocol":"openai-chat",
			"auth":{"scheme":"bearer","key":"secret","header_name":"X-Vendor-Key"},
			"models":[{"alias":"bearer/m","upstream_model":"m"}],
			"timeouts":{},
			"enabled":true
		}]
	}`)

	_, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, importTestKeyring(t), nil)
	if err == nil || !strings.Contains(err.Error(), "auth.header_name") {
		t.Fatalf("bearer header override error = %v, want auth.header_name rejection", err)
	}
	var providers int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM providers`).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if providers != 0 {
		t.Fatalf("providers after rejected bearer override = %d, want 0", providers)
	}
}

func TestImportV2RejectsUnresolvedKeyRef(t *testing.T) {
	for name, refs := range map[string]providerstore.RefResolver{
		"no resolver":   nil,
		"missing entry": providerstore.MapRefResolver{},
	} {
		t.Run(name, func(t *testing.T) {
			pool := newAdminTestPool(t, requiredImportTestDSN(t))
			createImportOwner(t, pool)
			raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [{
			"id":"referenced-key",
			"name":"Referenced Key",
			"base_url":"https://ref.example/v1",
			"protocol":"openai-chat",
			"auth":{"scheme":"bearer","key_ref":"provider-secret"},
			"models":[{"alias":"ref/m","upstream_model":"m"}],
			"timeouts":{},
			"enabled":true
		}]
	}`)

			_, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, importTestKeyring(t), refs)
			if err == nil || !strings.Contains(err.Error(), "auth.key_ref") {
				t.Fatalf("unresolved key_ref error = %v, want auth.key_ref rejection", err)
			}
			var providers int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM providers`).Scan(&providers); err != nil {
				t.Fatal(err)
			}
			if providers != 0 {
				t.Fatalf("providers after unresolved key_ref = %d, want 0", providers)
			}
		})
	}
}

func TestImportV2EncryptsResolvedKeyRef(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	ring := importTestKeyring(t)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [{
			"id":"referenced-key",
			"name":"Referenced Key",
			"base_url":"https://ref.example/v1",
			"protocol":"openai-chat",
			"auth":{"scheme":"bearer","key_ref":"provider-secret"},
			"models":[{"alias":"ref/m","upstream_model":"m"}],
			"timeouts":{},
			"enabled":true
		}]
	}`)
	refs := providerstore.MapRefResolver{"provider-secret": "resolved-secret"}
	if _, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, ring, refs); err != nil {
		t.Fatalf("import resolved key_ref: %v", err)
	}
	rows, _, err := providerstore.NewDBRowSource(pool).LoadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Keys) != 1 || rows.Keys[0].KeyRef != "" || !rows.Keys[0].HasSecret() {
		t.Fatalf("resolved key_ref row = %+v, want encrypted-only credential", rows.Keys)
	}
	credential, err := providerstore.ResolveKeyCredential(t.Context(), rows.Keys[0], ring, nil)
	if err != nil || credential.Raw != "resolved-secret" {
		t.Fatalf("resolved imported key_ref = %q, %v", credential.Raw, err)
	}
}

func TestImportV2PreservesUnpositionedSourceOrder(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [
			{"id":"z-primary","name":"Primary","base_url":"https://primary.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"shared","upstream_model":"primary"}],"timeouts":{},"enabled":true},
			{"id":"a-backup","name":"Backup","base_url":"https://backup.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"shared","upstream_model":"backup"}],"timeouts":{},"enabled":true}
		]
	}`)
	if _, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, importTestKeyring(t), nil); err != nil {
		t.Fatalf("import v2: %v", err)
	}
	store := providerstore.NewStore(providerstore.NewDBRowSource(pool), providerstore.StoreOptions{})
	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("load imported snapshot: %v", err)
	}
	route, err := providerstore.NewRouter(store).Resolve(t.Context(), "user", "shared", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("resolve imported alias: %v", err)
	}
	if route.UpstreamModel != "primary" {
		t.Fatalf("imported alias resolved to %q, want first source provider primary", route.UpstreamModel)
	}
}

func TestImportV2RollsBackEveryRowOnFailure(t *testing.T) {
	pool := newAdminTestPool(t, requiredImportTestDSN(t))
	createImportOwner(t, pool)
	ring := importTestKeyring(t)
	raw := []byte(`{
		"schema_version": 2,
		"providers_v2": [
			{"id":"valid","name":"Valid","base_url":"https://valid.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"valid/m","upstream_model":"m"}],"timeouts":{},"enabled":true},
			{"id":"BadID","name":"Invalid DB ID","base_url":"https://invalid.example/v1","protocol":"openai-chat","auth":{"scheme":"none"},"models":[{"alias":"invalid/m","upstream_model":"m"}],"timeouts":{},"enabled":true}
		]
	}`)

	if _, err := providerstore.ImportV2(t.Context(), pool, raw, "legacy-owner", false, ring, nil); err == nil {
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

func createImportOwner(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id, username, role, password_hash) VALUES ($1::uuid, 'legacy-owner', 'owner', 'test-hash')`, importOwnerID); err != nil {
		t.Fatalf("create import owner: %v", err)
	}
}

// seedDatabaseProvider inserts a provider and key that the v2 import did not
// create, as an operator would through the admin API.
func seedDatabaseProvider(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO providers (id, name, protocol, base_url)
		VALUES ($1, $1, 'openai-chat', 'https://' || $1 || '.example/v1')`, id); err != nil {
		t.Fatalf("seed provider %s: %v", id, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO provider_keys (provider_id, name, auth_scheme)
		VALUES ($1, 'manual', 'none')`, id); err != nil {
		t.Fatalf("seed provider key %s: %v", id, err)
	}
}

func databaseProviderIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT id FROM providers ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
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
