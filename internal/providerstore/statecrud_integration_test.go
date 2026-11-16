package providerstore

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests cover the two database helpers that keep a database-backed
// gateway free of JSON files: the free-form system_state documents and the
// one-time conversion of key_ref rows into stored ciphertext. They run against
// GNVIPI_TEST_DATABASE_URL and skip otherwise, exactly like the other
// integration tests in this package.

func stateTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func stateTestKeyring(t *testing.T) *Keyring {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("read master key: %v", err)
	}
	keyring, err := NewKeyring(1, key, nil)
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	return keyring
}

func TestSystemStateRoundTrip(t *testing.T) {
	dsn := integrationDSN(t)
	integrationEnsureSchema(t, dsn)
	pool := integrationPool(t, dsn)
	ctx := stateTestContext(t)
	key := "test-state-" + integrationUniqueSuffix()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM system_state WHERE key = $1`, key); err != nil {
			t.Logf("cleanup system_state %s: %v", key, err)
		}
	})

	var missing struct {
		URL string `json:"url"`
	}
	if ok, err := LoadSystemState(ctx, pool, key, &missing); err != nil {
		t.Fatalf("load missing system state: %v", err)
	} else if ok {
		t.Fatalf("missing system state reported as present")
	}

	first := struct {
		URL        string   `json:"url"`
		Fallbacks  []string `json:"fallbacks"`
		RetryCount int      `json:"retry_count"`
	}{URL: "https://first.example.com/widget", Fallbacks: []string{"https://backup.example.com/widget"}, RetryCount: 2}
	if err := SaveSystemState(ctx, pool, key, first); err != nil {
		t.Fatalf("save system state: %v", err)
	}
	var loaded struct {
		URL        string   `json:"url"`
		Fallbacks  []string `json:"fallbacks"`
		RetryCount int      `json:"retry_count"`
	}
	ok, err := LoadSystemState(ctx, pool, key, &loaded)
	if err != nil || !ok {
		t.Fatalf("load system state = (%v, %v), want present", ok, err)
	}
	if loaded.URL != first.URL || loaded.RetryCount != first.RetryCount || !slices.Equal(loaded.Fallbacks, first.Fallbacks) {
		t.Fatalf("loaded system state = %+v, want %+v", loaded, first)
	}

	// A second save must overwrite the document, not create a second one: the
	// table is keyed by name and the caller stores one value per key.
	second := struct {
		URL        string   `json:"url"`
		Fallbacks  []string `json:"fallbacks"`
		RetryCount int      `json:"retry_count"`
	}{URL: "https://second.example.com/widget"}
	if err := SaveSystemState(ctx, pool, key, second); err != nil {
		t.Fatalf("overwrite system state: %v", err)
	}
	loaded = struct {
		URL        string   `json:"url"`
		Fallbacks  []string `json:"fallbacks"`
		RetryCount int      `json:"retry_count"`
	}{}
	if _, err := LoadSystemState(ctx, pool, key, &loaded); err != nil {
		t.Fatalf("reload system state: %v", err)
	}
	if loaded.URL != second.URL || loaded.RetryCount != 0 || len(loaded.Fallbacks) != 0 {
		t.Fatalf("overwritten system state = %+v, want %+v", loaded, second)
	}
}

func TestMaterializeKeyRefs(t *testing.T) {
	dsn := integrationDSN(t)
	integrationEnsureSchema(t, dsn)
	pool := integrationPool(t, dsn)
	ctx := stateTestContext(t)

	suffix := integrationUniqueSuffix()
	ownerID := integrationNewUser(t, ctx, pool, "keyref-"+suffix)
	providerID := "keyref-" + suffix
	integrationNewProvider(t, ctx, pool, providerID)
	integrationCleanup(t, pool, providerID, []string{ownerID}, nil)

	keyring := stateTestKeyring(t)
	const secret = "sk-materialized-secret"
	refRow := insertTestKeyRef(t, ctx, pool, providerID, ownerID, "resolvable", "import:shared")
	orphanRow := insertTestKeyRef(t, ctx, pool, providerID, ownerID, "orphan", "env:GNVIPI_KEY_ABSENT")

	var unresolvedRefs []string
	// A nil resolver cannot resolve anything, so nothing is rewritten.
	migrated, unresolved, err := MaterializeKeyRefs(ctx, pool, keyring, nil)
	if err != nil {
		t.Fatalf("materialize without resolver: %v", err)
	}
	if migrated != 0 {
		t.Fatalf("materialize without resolver migrated %d row(s)", migrated)
	}
	if !slices.Contains(unresolved, orphanRow.ID) || !slices.Contains(unresolved, refRow.ID) {
		t.Fatalf("unresolved without resolver = %v, want both key ids", unresolved)
	}

	migrated, unresolvedRefs, err = MaterializeKeyRefs(ctx, pool, keyring, MapRefResolver{"import:shared": secret})
	if err != nil {
		t.Fatalf("materialize key references: %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}
	if !slices.Equal(unresolvedRefs, []string{orphanRow.ID}) {
		t.Fatalf("unresolved = %v, want [%s]", unresolvedRefs, orphanRow.ID)
	}

	record, err := GetProviderKey(ctx, pool, providerID, refRow.ID, false)
	if err != nil {
		t.Fatalf("read migrated key: %v", err)
	}
	if record.KeyRef != "" {
		t.Fatalf("migrated key still carries key_ref %q", record.KeyRef)
	}
	if !record.HasSecret() || record.SecretLast4 == "" {
		t.Fatalf("migrated key has no stored secret: %+v", record.KeyRow)
	}
	plain, err := keyring.Decrypt(record.SecretEnc, record.SecretNonce, record.ID,
		record.ScopeOwner(), record.AuthScheme, record.SecretKVer)
	if err != nil {
		t.Fatalf("decrypt migrated key: %v", err)
	}
	if string(plain) != secret {
		t.Fatalf("decrypted secret = %q, want %q", plain, secret)
	}

	// An unresolved row keeps its reference so an operator can still fix the
	// imported secrets file and restart.
	orphan, err := GetProviderKey(ctx, pool, providerID, orphanRow.ID, false)
	if err != nil {
		t.Fatalf("read unresolved key: %v", err)
	}
	if orphan.KeyRef != "env:GNVIPI_KEY_ABSENT" || orphan.HasSecret() {
		t.Fatalf("unresolved key = %+v, want the reference preserved", orphan.KeyRow)
	}

	// A second run finds nothing to convert: only the orphan is reported.
	migrated, unresolvedRefs, err = MaterializeKeyRefs(ctx, pool, keyring, MapRefResolver{"import:shared": secret})
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if migrated != 0 {
		t.Fatalf("second materialize migrated %d row(s), want 0", migrated)
	}
	if !slices.Equal(unresolvedRefs, []string{orphanRow.ID}) {
		t.Fatalf("second unresolved = %v, want [%s]", unresolvedRefs, orphanRow.ID)
	}

	if _, _, err := MaterializeKeyRefs(ctx, pool, nil, MapRefResolver{"import:shared": secret}); !errors.Is(err, ErrMissingMasterKey) {
		t.Fatalf("materialize without keyring = %v, want ErrMissingMasterKey", err)
	}
}

// insertTestKeyRef stores one enabled bearer key whose only credential source
// is a key_ref, the shape a v2 import leaves behind before materialization.
func insertTestKeyRef(t *testing.T, ctx context.Context, pool *pgxpool.Pool, providerID, ownerID, name, ref string) KeyRecord {
	t.Helper()
	keyID, err := NewKeyID(ctx, pool)
	if err != nil {
		t.Fatalf("allocate key id: %v", err)
	}
	// A credentialed key must be personal: the schema forbids a shared key
	// with a credential, so the owner scope also proves the AAD binding.
	record, err := InsertProviderKey(ctx, pool, KeyRow{
		ID:         keyID,
		ProviderID: providerID,
		OwnerID:    ownerID,
		Name:       name,
		Enabled:    true,
		AuthScheme: SchemeBearer,
		KeyRef:     ref,
	})
	if err != nil {
		t.Fatalf("insert key %s: %v", name, err)
	}
	return record
}
