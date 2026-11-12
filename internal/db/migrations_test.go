package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/minhhhduc/gnvipi/migrations"
)

// These tests run against a real PostgreSQL database. No mocks: when
// GNVIPI_TEST_DATABASE_URL is unset they skip with instructions.

var (
	migrateOnce sync.Once
	migrateErr  error
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GNVIPI_TEST_DATABASE_URL"))
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; point it at a disposable PostgreSQL database to run schema integration tests")
	}
	return dsn
}

func ensureMigrated(t *testing.T, dsn string) {
	t.Helper()
	migrateOnce.Do(func() {
		migrateErr = Migrate(context.Background(), dsn)
	})
	if migrateErr != nil {
		t.Fatalf("apply migrations: %v", migrateErr)
	}
}

func testConn(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	dsn := testDatabaseURL(t)
	ensureMigrated(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) //nolint:errcheck // best-effort cleanup/best-effort parse
	return conn, ctx
}

// withRollback runs fn in a transaction that is always rolled back, so tests
// do not leak rows between each other.
func withRollback(t *testing.T, ctx context.Context, conn *pgx.Conn, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort cleanup/best-effort parse
	fn(tx)
}

func insertUser(t *testing.T, ctx context.Context, tx pgx.Tx, username, role string) string {
	t.Helper()
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO users (username, role, password_hash) VALUES ($1, $2, 'x') RETURNING id::text`,
		username, role).Scan(&id)
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	return id
}

func insertProvider(t *testing.T, ctx context.Context, tx pgx.Tx, id string) {
	t.Helper()
	if _, err := tx.Exec(ctx,
		`INSERT INTO providers (id, name, protocol, base_url)
		 VALUES ($1, $1, 'openai', 'https://example.com')`, id); err != nil {
		t.Fatalf("insert provider %q: %v", id, err)
	}
}

func insertSecretKey(t *testing.T, ctx context.Context, tx pgx.Tx, providerID, ownerID, name string) string {
	t.Helper()
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO provider_keys
		   (provider_id, owner_id, name, auth_scheme, secret_enc, secret_nonce, secret_kver, secret_last4)
		 VALUES ($1, $2::uuid, $3, 'bearer', $4, $5, 1, 'last')
		 RETURNING id::text`,
		providerID, ownerID, name, []byte{1, 2, 3}, make([]byte, 12)).Scan(&id)
	if err != nil {
		t.Fatalf("insert key %q: %v", name, err)
	}
	return id
}

// assertCheckViolation asserts a CHECK failure (SQLSTATE 23514) on the named
// constraint. PostgreSQL reports only the constraint name, never the
// predicate, so the migration names every CHECK these tests depend on.
func assertCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected CHECK violation %q, got no error", constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is not a PostgreSQL error: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Fatalf("SQLSTATE = %q, want 23514 (check_violation): %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("constraint = %q, want %q: %v", pgErr.ConstraintName, constraint, err)
	}
}

func TestMigrationsApply(t *testing.T) {
	conn, ctx := testConn(t)

	tables := []string{
		"users", "sessions", "api_tokens", "login_attempts", "audit_log",
		"providers", "provider_keys", "key_models", "model_state",
		"settings", "system_state", "config_state",
	}
	for _, table := range tables {
		var exists bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s is missing", table)
		}
	}

	for _, table := range []string{"provider_keys", "key_models"} {
		var generated string
		err := conn.QueryRow(ctx,
			`SELECT is_generated FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'scope_owner'`,
			table).Scan(&generated)
		if err != nil {
			t.Fatalf("%s.scope_owner: %v", table, err)
		}
		if generated != "ALWAYS" {
			t.Fatalf("%s.scope_owner is_generated = %q, want ALWAYS", table, generated)
		}
	}

	var version int64
	if err := conn.QueryRow(ctx, `SELECT version FROM config_state WHERE id`).Scan(&version); err != nil {
		t.Fatalf("config_state singleton: %v", err)
	}
	if version < 1 {
		t.Fatalf("config_state.version = %d, want >= 1", version)
	}
	var imports int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM system_state WHERE key = 'v2_import'`).Scan(&imports); err != nil {
		t.Fatalf("system_state v2_import: %v", err)
	}
	if imports != 1 {
		t.Fatalf("system_state v2_import rows = %d, want 1", imports)
	}

	wantTriggers := map[string][]string{
		"users":         {"users_set_updated_at", "users_guard_last_owner_bu", "users_guard_last_owner_bd", "users_bump_config_on_delete"},
		"providers":     {"providers_set_updated_at", "providers_bump_config"},
		"provider_keys": {"provider_keys_set_updated_at", "provider_keys_scope_immutable_bu", "provider_keys_bump_config"},
		"key_models":    {"key_models_sync_scope_bi", "key_models_sync_scope_bu", "key_models_bump_config"},
		"model_state":   {"model_state_set_updated_at", "model_state_bump_config"},
		"settings":      {"settings_set_updated_at", "settings_bump_config"},
		"system_state":  {"system_state_set_updated_at"},
		"config_state":  {"config_state_set_updated_at"},
	}
	for table, want := range wantTriggers {
		have := map[string]bool{}
		rows, err := conn.Query(ctx,
			`SELECT tgname FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = $1::regclass`,
			table)
		if err != nil {
			t.Fatalf("list triggers on %s: %v", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("scan trigger on %s: %v", table, err)
			}
			have[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("list triggers on %s: %v", table, err)
		}
		for _, name := range want {
			if !have[name] {
				t.Fatalf("trigger %s on %s is missing (have %v)", name, table, have)
			}
		}
	}

	// Constraint definitions carry the exact spec predicates.
	constraintChecks := map[string][]string{
		"provider_keys": {
			"owner_id IS NOT NULL",
			"num_nonnulls(secret_enc, key_env, key_ref) <= 1",
			"num_nonnulls(secret_enc, key_env, key_ref) = 1",
			"octet_length(secret_nonce) = 12",
			"UNIQUE (id, provider_id, scope_owner)",
		},
		"key_models": {
			"FOREIGN KEY (key_id, provider_id, scope_owner)",
			"jsonb_array_length(fallbacks) <= 16",
		},
		"providers": {
			"jsonb_typeof(endpoint_paths) = 'object'",
			"NOT (extra_body ? 'model'",
		},
		"users": {
			"role = ANY",
		},
	}
	for table, wants := range constraintChecks {
		var defs string
		if err := conn.QueryRow(ctx,
			`SELECT COALESCE(string_agg(pg_get_constraintdef(oid), E'\n'), '')
			 FROM pg_constraint WHERE conrelid = $1::regclass`, table).Scan(&defs); err != nil {
			t.Fatalf("read constraints on %s: %v", table, err)
		}
		for _, want := range wants {
			if !strings.Contains(defs, want) {
				t.Fatalf("constraint on %s missing %q; have:\n%s", table, want, defs)
			}
		}
	}

	partial := map[string]string{
		"provider_keys_owner_name_uq":  "owner_id IS NOT NULL",
		"provider_keys_shared_name_uq": "owner_id IS NULL",
		"key_models_owned_alias_uq":    "owner_id IS NOT NULL",
		"key_models_shared_alias_uq":   "owner_id IS NULL",
	}
	for name, want := range partial {
		var def string
		if err := conn.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
			name).Scan(&def); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		if !strings.Contains(def, "WHERE") || !strings.Contains(def, want) {
			t.Fatalf("index %s is not a partial index on %q: %s", name, want, def)
		}
	}

	latest, err := LatestVersion(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, conn, latest); err != nil {
		t.Fatalf("schema guard rejected an up-to-date schema: %v", err)
	}
	if err := CheckSchema(ctx, conn, latest+1); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("schema guard = %v, want ErrSchemaTooOld", err)
	}
}

func TestProviderKeyCredentialChecks(t *testing.T) {
	conn, ctx := testConn(t)
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "cred-provider")
		owner := insertUser(t, ctx, tx, "cred-owner", "user")
		other := insertUser(t, ctx, tx, "cred-other", "user")
		_ = other

		secret := []byte{1, 2, 3}
		nonce := make([]byte, 12)

		type kase struct {
			name           string
			owner          any
			scheme         string
			enabled        bool
			header         any
			keyEnv         any
			keyRef         any
			enc            any
			nonce          any
			kver           any
			last4          any
			wantConstraint string
		}
		cases := []kase{
			{name: "enabled bearer without credential", owner: owner, scheme: "bearer", enabled: true, wantConstraint: "provider_keys_credential_required"},
			{name: "enabled bearer with secret", owner: owner, scheme: "bearer", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last"},
			{name: "enabled bearer with two sources", owner: owner, scheme: "bearer", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last", keyEnv: "GNVIPI_KEY_TEST", wantConstraint: "provider_keys_credential_single"},
			{name: "disabled bearer without credential", owner: owner, scheme: "bearer", enabled: false},
			{name: "none with secret", owner: owner, scheme: "none", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last", wantConstraint: "provider_keys_none_clears_credentials"},
			{name: "shared none key", owner: nil, scheme: "none", enabled: true},
			{name: "shared bearer key", owner: nil, scheme: "bearer", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last", wantConstraint: "provider_keys_owner_none"},
			{name: "header without header name", owner: owner, scheme: "header", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last", wantConstraint: "provider_keys_header_name_required"},
			{name: "header with header name", owner: owner, scheme: "header", enabled: true, header: "x-api-key", enc: secret, nonce: nonce, kver: 1, last4: "last"},
			{name: "bad nonce length", owner: owner, scheme: "bearer", enabled: true, enc: secret, nonce: make([]byte, 11), kver: 1, last4: "last", wantConstraint: "provider_keys_nonce_length"},
			{name: "key_ref control characters", owner: owner, scheme: "bearer", enabled: true, keyRef: "bad\x01ref", wantConstraint: "provider_keys_key_ref_syntax"},
			{name: "key_env outside credential namespace", owner: owner, scheme: "bearer", enabled: true, keyEnv: "GNVIPI_MASTER_KEY", wantConstraint: "provider_keys_key_env_syntax"},
			{name: "empty key_env namespace suffix", owner: owner, scheme: "bearer", enabled: true, keyEnv: "GNVIPI_KEY_", wantConstraint: "provider_keys_key_env_syntax"},
			{name: "key_env source", owner: owner, scheme: "bearer", enabled: true, keyEnv: "GNVIPI_KEY_TEST"},
			{name: "key_ref source", owner: owner, scheme: "bearer", enabled: true, keyRef: "vault://my/ref"},
		}
		for i, tc := range cases {
			if _, err := tx.Exec(ctx, "SAVEPOINT credential_case"); err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO provider_keys
				   (provider_id, owner_id, name, enabled, auth_scheme, auth_header_name,
				    key_env, key_ref, secret_enc, secret_nonce, secret_kver, secret_last4)
				 VALUES ('cred-provider', $1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				tc.owner, fmt.Sprintf("key-%02d", i), tc.enabled, tc.scheme,
				tc.header, tc.keyEnv, tc.keyRef, tc.enc, tc.nonce, tc.kver, tc.last4)
			if tc.wantConstraint == "" {
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", tc.name, err)
				}
				if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT credential_case"); err != nil {
					t.Fatalf("release savepoint: %v", err)
				}
				continue
			}
			assertCheckViolation(t, err, tc.wantConstraint)
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT credential_case"); err != nil {
				t.Fatalf("rollback savepoint: %v", err)
			}
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT credential_case"); err != nil {
				t.Fatalf("release savepoint: %v", err)
			}
		}
	})
}

func TestProviderKeyScopeImmutable(t *testing.T) {
	conn, ctx := testConn(t)
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "scope-provider")
		insertProvider(t, ctx, tx, "scope-other")
		owner := insertUser(t, ctx, tx, "scope-owner", "user")
		other := insertUser(t, ctx, tx, "scope-other-user", "user")
		keyID := insertSecretKey(t, ctx, tx, "scope-provider", owner, "key-one")

		if _, err := tx.Exec(ctx, `UPDATE provider_keys SET owner_id = $1::uuid WHERE id = $2::uuid`, other, keyID); err == nil ||
			!strings.Contains(err.Error(), "scope is immutable") {
			t.Fatalf("owner change = %v, want scope immutability error", err)
		}
	})
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "scope-provider")
		insertProvider(t, ctx, tx, "scope-other")
		owner := insertUser(t, ctx, tx, "scope-owner", "user")
		keyID := insertSecretKey(t, ctx, tx, "scope-provider", owner, "key-one")

		if _, err := tx.Exec(ctx, `UPDATE provider_keys SET provider_id = 'scope-other' WHERE id = $1::uuid`, keyID); err == nil ||
			!strings.Contains(err.Error(), "scope is immutable") {
			t.Fatalf("provider change = %v, want scope immutability error", err)
		}
	})
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "scope-provider")
		owner := insertUser(t, ctx, tx, "scope-owner", "user")
		keyID := insertSecretKey(t, ctx, tx, "scope-provider", owner, "key-one")

		if _, err := tx.Exec(ctx, `UPDATE provider_keys SET name = 'renamed' WHERE id = $1::uuid`, keyID); err != nil {
			t.Fatalf("renaming a key must be allowed: %v", err)
		}
	})
}

func TestKeyModelsScopeDriftAndCompositeFK(t *testing.T) {
	conn, ctx := testConn(t)
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "model-provider")
		owner := insertUser(t, ctx, tx, "model-owner", "user")
		keyID := insertSecretKey(t, ctx, tx, "model-provider", owner, "model-key")

		if _, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, provider_id, owner_id, alias, upstream_model)
			 VALUES ($1::uuid, 'model-provider', $2::uuid, 'gpt-test', 'gpt-test')`,
			keyID, owner); err != nil {
			t.Fatalf("insert key model: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE key_models SET provider_id = 'somewhere-else' WHERE key_id = $1::uuid`, keyID); err == nil ||
			!strings.Contains(err.Error(), "immutable") {
			t.Fatalf("provider drift = %v, want immutable error", err)
		}
	})

	// The BEFORE INSERT trigger overwrites the derived columns, so the
	// composite FK only becomes reachable with the trigger disabled. Skip when
	// the test role is not the table owner.
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "model-provider")
		owner := insertUser(t, ctx, tx, "model-owner", "user")
		keyID := insertSecretKey(t, ctx, tx, "model-provider", owner, "model-key")

		if _, err := tx.Exec(ctx, `ALTER TABLE key_models DISABLE TRIGGER key_models_sync_scope_bi`); err != nil {
			t.Skipf("cannot exercise the composite FK directly: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, provider_id, owner_id, alias, upstream_model)
			 VALUES ($1::uuid, 'wrong-provider', $2::uuid, 'gpt-test', 'gpt-test')`,
			keyID, owner)
		if err == nil {
			t.Fatal("mismatched key_models scope was accepted; want composite FK violation")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Fatalf("mismatched scope error = %v, want foreign key violation", err)
		}
	})
}

func TestLastActiveOwnerProtected(t *testing.T) {
	conn, ctx := testConn(t)

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertUser(t, ctx, tx, "owner-one", "owner")
		if _, err := tx.Exec(ctx, `UPDATE users SET role = 'user' WHERE username = 'owner-one'`); err == nil ||
			!strings.Contains(err.Error(), "last active owner") {
			t.Fatalf("demote = %v, want last-owner error", err)
		}
	})
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertUser(t, ctx, tx, "owner-one", "owner")
		if _, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE username = 'owner-one'`); err == nil ||
			!strings.Contains(err.Error(), "last active owner") {
			t.Fatalf("disable = %v, want last-owner error", err)
		}
	})
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertUser(t, ctx, tx, "owner-one", "owner")
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE username = 'owner-one'`); err == nil ||
			!strings.Contains(err.Error(), "last active owner") {
			t.Fatalf("delete = %v, want last-owner error", err)
		}
	})
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertUser(t, ctx, tx, "owner-one", "owner")
		insertUser(t, ctx, tx, "owner-two", "owner")
		if _, err := tx.Exec(ctx, `UPDATE users SET role = 'user' WHERE username = 'owner-one'`); err != nil {
			t.Fatalf("demoting one of two owners must be allowed: %v", err)
		}
	})
}

func TestConfigVersionBumpsPerStatement(t *testing.T) {
	conn, ctx := testConn(t)
	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		current := func() int64 {
			var v int64
			if err := tx.QueryRow(ctx, `SELECT version FROM config_state WHERE id`).Scan(&v); err != nil {
				t.Fatalf("read config version: %v", err)
			}
			return v
		}
		expectBump := func(want int64, what string) {
			t.Helper()
			if got := current(); got != want {
				t.Fatalf("%s: config version = %d, want %d", what, got, want)
			}
		}

		v := current()
		if _, err := tx.Exec(ctx,
			`INSERT INTO providers (id, name, protocol, base_url) VALUES
			   ('bump-a', 'A', 'openai', 'https://example.com'),
			   ('bump-b', 'B', 'openai', 'https://example.com'),
			   ('bump-c', 'C', 'openai', 'https://example.com')`); err != nil {
			t.Fatalf("insert providers: %v", err)
		}
		v++
		expectBump(v, "one INSERT of three providers")

		if _, err := tx.Exec(ctx, `UPDATE providers SET name = name || '-x'`); err != nil {
			t.Fatalf("update providers: %v", err)
		}
		v++
		expectBump(v, "one UPDATE statement")

		if _, err := tx.Exec(ctx, `DELETE FROM providers WHERE id LIKE 'bump-%'`); err != nil {
			t.Fatalf("delete providers: %v", err)
		}
		v += 2 // providers delete cascades to provider_keys, whose statement trigger also bumps
		expectBump(v, "one DELETE statement plus cascade")

		if _, err := tx.Exec(ctx, `INSERT INTO model_state (model_id) VALUES ('m-a'), ('m-b')`); err != nil {
			t.Fatalf("insert model_state: %v", err)
		}
		v++
		expectBump(v, "model_state insert")

		if _, err := tx.Exec(ctx,
			`INSERT INTO settings (key, value) VALUES ('automode', '{}'::jsonb)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
			t.Fatalf("upsert settings: %v", err)
		}
		v += 2 // ON CONFLICT DO UPDATE fires both INSERT and UPDATE statement events
		expectBump(v, "settings upsert")

		insertUser(t, ctx, tx, "bump-user", "user")
		expectBump(v, "user insert must not bump")
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE username = 'bump-user'`); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		v += 2 // users delete cascades through provider_keys, whose statement trigger also bumps
		expectBump(v, "user DELETE statement plus cascade")

		insertProvider(t, ctx, tx, "bump-provider")
		v++
		expectBump(v, "provider insert")

		if _, err := tx.Exec(ctx,
			`INSERT INTO provider_keys (provider_id, name, auth_scheme)
			 VALUES ('bump-provider', 'shared', 'none')`); err != nil {
			t.Fatalf("insert provider key: %v", err)
		}
		v++
		expectBump(v, "provider_keys insert")

		if _, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model)
			 SELECT id, 'bump-model', 'bump-model' FROM provider_keys
			 WHERE provider_id = 'bump-provider' AND name = 'shared'`); err != nil {
			t.Fatalf("insert key model: %v", err)
		}
		v++
		expectBump(v, "key_models insert")
	})
}
