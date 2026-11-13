package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	migratedb "github.com/golang-migrate/migrate/v4/database"
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

func assertUniqueViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected unique violation %q, got no error", constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is not a PostgreSQL error: %v", err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("SQLSTATE = %q, want 23505 (unique_violation): %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("constraint = %q, want %q: %v", pgErr.ConstraintName, constraint, err)
	}
}

type advisoryLockCoordinates struct {
	database int64
	classID  int64
	objectID int64
	subID    int32
}

func heldAdvisoryLockCoordinates(t *testing.T, ctx context.Context, conn *pgx.Conn) advisoryLockCoordinates {
	t.Helper()
	var lock advisoryLockCoordinates
	if err := conn.QueryRow(ctx, `SELECT database::bigint, classid::bigint, objid::bigint, objsubid
		FROM pg_locks
		WHERE pid = pg_backend_pid() AND locktype = 'advisory' AND granted
		ORDER BY classid, objid, objsubid
		LIMIT 1`).Scan(&lock.database, &lock.classID, &lock.objectID, &lock.subID); err != nil {
		t.Fatalf("read held advisory lock: %v", err)
	}
	return lock
}

func advisoryLockExists(t *testing.T, ctx context.Context, conn *pgx.Conn, lock advisoryLockCoordinates, granted bool) bool {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory'
		  AND database::bigint = $1 AND classid::bigint = $2
		  AND objid::bigint = $3 AND objsubid = $4 AND granted = $5
	)`, lock.database, lock.classID, lock.objectID, lock.subID, granted).Scan(&exists); err != nil {
		t.Fatalf("inspect advisory lock: %v", err)
	}
	return exists
}

func waitForAdvisoryLock(t *testing.T, ctx context.Context, conn *pgx.Conn, lock advisoryLockCoordinates, granted bool) {
	t.Helper()
	for {
		if advisoryLockExists(t, ctx, conn, lock, granted) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for advisory lock (granted=%v): %v", granted, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMigrateWaitsForAdvisoryLock(t *testing.T) {
	dsn := testDatabaseURL(t)
	ensureMigrated(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock holder: %v", err)
	}
	defer func() { _ = holder.Close(context.Background()) }() //nolint:errcheck // best-effort test cleanup
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtext('gnvipi:migrations'))`); err != nil {
		t.Fatalf("hold migration lock: %v", err)
	}
	lock := heldAdvisoryLockCoordinates(t, ctx, holder)
	locked := true
	defer func() {
		if locked {
			_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('gnvipi:migrations'))`) //nolint:errcheck // best-effort test cleanup
		}
	}()

	done := make(chan error, 1)
	go func() { done <- Migrate(ctx, dsn) }()
	waitForAdvisoryLock(t, ctx, holder, lock, false)
	select {
	case err := <-done:
		t.Fatalf("concurrent migration completed while advisory lock was held: %v", err)
	default:
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('gnvipi:migrations'))`); err != nil {
		t.Fatalf("release migration lock: %v", err)
	}
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after lock release: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("migration did not finish after lock release: %v", ctx.Err())
	}
}

func TestMigrateHoldsNamedLockForMigrationRun(t *testing.T) {
	dsn := testDatabaseURL(t)
	ensureMigrated(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock probe: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }() //nolint:errcheck // best-effort test cleanup
	if _, err := probe.Exec(ctx, `SELECT pg_advisory_lock(hashtext('gnvipi:migrations'))`); err != nil {
		t.Fatalf("probe named migration lock: %v", err)
	}
	namedLock := heldAdvisoryLockCoordinates(t, ctx, probe)
	if _, err := probe.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('gnvipi:migrations'))`); err != nil {
		t.Fatalf("release probed migration lock: %v", err)
	}

	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	driverLockText, err := migratedb.GenerateAdvisoryLockId(parsed.Database, "public", "schema_migrations")
	if err != nil {
		t.Fatalf("derive driver lock: %v", err)
	}
	driverLockID, err := strconv.ParseInt(driverLockText, 10, 64)
	if err != nil {
		t.Fatalf("parse driver lock: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT pg_advisory_lock($1)`, driverLockID); err != nil {
		t.Fatalf("hold migrate driver lock: %v", err)
	}
	driverLock := heldAdvisoryLockCoordinates(t, ctx, probe)
	driverLocked := true
	defer func() {
		if driverLocked {
			_, _ = probe.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, driverLockID) //nolint:errcheck // best-effort test cleanup
		}
	}()

	done := make(chan error, 1)
	go func() { done <- Migrate(ctx, dsn) }()
	waitForAdvisoryLock(t, ctx, probe, driverLock, false)
	if !advisoryLockExists(t, ctx, probe, namedLock, true) {
		t.Fatal("named migration lock was not held while migration execution was blocked")
	}
	if _, err := probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, driverLockID); err != nil {
		t.Fatalf("release migrate driver lock: %v", err)
	}
	driverLocked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after driver lock release: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("migration did not finish after driver lock release: %v", ctx.Err())
	}
}

func TestMigrateCancellationStopsDriverWait(t *testing.T) {
	dsn := testDatabaseURL(t)
	ensureMigrated(t, dsn)
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()

	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect driver lock blocker: %v", err)
	}
	defer func() { _ = blocker.Close(context.Background()) }() //nolint:errcheck // best-effort test cleanup
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	driverLockText, err := migratedb.GenerateAdvisoryLockId(parsed.Database, "public", "schema_migrations")
	if err != nil {
		t.Fatalf("derive driver lock: %v", err)
	}
	driverLockID, err := strconv.ParseInt(driverLockText, 10, 64)
	if err != nil {
		t.Fatalf("parse driver lock: %v", err)
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, driverLockID); err != nil {
		t.Fatalf("hold migrate driver lock: %v", err)
	}
	driverLock := heldAdvisoryLockCoordinates(t, ctx, blocker)
	defer func() {
		_, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, driverLockID) //nolint:errcheck // best-effort test cleanup
	}()

	migrateCtx, cancelMigrate := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Migrate(migrateCtx, dsn) }()
	waitForAdvisoryLock(t, ctx, blocker, driverLock, false)
	cancelMigrate()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled migration = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled migration remained blocked in the driver")
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

	var auditType, auditIdentity, auditGeneration string
	if err := conn.QueryRow(ctx,
		`SELECT data_type, is_identity, identity_generation
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'audit_log' AND column_name = 'id'`).
		Scan(&auditType, &auditIdentity, &auditGeneration); err != nil {
		t.Fatalf("audit_log.id metadata: %v", err)
	}
	if auditType != "bigint" || auditIdentity != "YES" || auditGeneration != "ALWAYS" {
		t.Fatalf("audit_log.id = type %q identity %q generation %q, want bigint identity ALWAYS", auditType, auditIdentity, auditGeneration)
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
		"users": {
			"length((username)::text) >= 3", "length(display_name) <= 200", "role = ANY",
			"status = ANY", "length(password_hash) > 0", "failed_attempts >= 0",
		},
		"sessions": {
			"octet_length(token_hash) = 32", "length(user_agent) <= 512", "expires_at > created_at",
		},
		"api_tokens": {
			"kind = ANY", "length(name) >= 1", "length(token_prefix) >= 4",
			"octet_length(token_hash) = 32", "scope = ANY", "scope IS NOT NULL", "expires_at > created_at",
		},
		"login_attempts": {"length((username)::text) <= 64"},
		"audit_log": {
			"actor_type = ANY", "length(action) >= 1", "length(target_type) >= 1",
			"jsonb_typeof(details) = 'object'", "length((details)::text) <= 8192",
		},
		"providers": {
			"id ~ '^[a-z0-9]", "length(name) >= 1", "length(protocol) >= 1", "base_url ~ '^https?://'",
			"proxy_url ~ '^(https?|socks5h?)://'", "tls = ANY", "jsonb_typeof(endpoint_paths) = 'object'",
			"jsonb_typeof(forward_headers) = 'array'", "jsonb_typeof(headers) = 'object'",
			"jsonb_typeof(extra_body) = 'object'", "NOT (extra_body ? 'model'", "NOT (extra_body ? 'stream'",
			"jsonb_typeof(extensions) = 'object'", "max_concurrency >= 0", "queue_timeout >= 0",
			"jsonb_typeof(timeouts) = 'object'",
		},
		"provider_keys": {
			"auth_scheme = ANY", "owner_id IS NOT NULL", "secret_enc IS NULL", "octet_length(secret_nonce) = 12",
			"secret_kver > 0", "length(secret_last4) <= 8", "key_env ~ '^GNVIPI_KEY_", "key_ref !~ '[[:cntrl:]]'",
			"auth_header_name ~ '^[!#$%&", "num_nonnulls(secret_enc, key_env, key_ref) <= 1",
			"num_nonnulls(secret_enc, key_env, key_ref) >= 1", "UNIQUE (id, provider_id, scope_owner)",
		},
		"key_models": {
			"length(alias) >= 1", "length(upstream_model) <= 200", "jsonb_typeof(fallbacks) = 'array'",
			"jsonb_array_length(fallbacks) <= 16", "jsonb_typeof(fallback_statuses) = 'array'",
			"jsonb_array_length(fallback_statuses) <= 32", "legacy_position >= 0", "alias <> '/*'",
			"FOREIGN KEY (key_id, provider_id, scope_owner)",
		},
		"model_state":  {"length(model_id) >= 1", "length(model_id) <= 200"},
		"settings":     {"key = ANY", "jsonb_typeof(value) = 'object'"},
		"config_state": {"CHECK (id)", "version > 0"},
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

func TestSchemaGuardReadsDatabaseStates(t *testing.T) {
	conn, ctx := testConn(t)
	latest, err := LatestVersion(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate string
		args   []any
		want   error
	}{
		{name: "dirty", mutate: `UPDATE schema_migrations SET dirty = true`, want: ErrSchemaDirty},
		{name: "older", mutate: `UPDATE schema_migrations SET version = $1`, args: []any{int64(latest - 1)}, want: ErrSchemaTooOld},
		{name: "newer", mutate: `UPDATE schema_migrations SET version = $1`, args: []any{int64(latest + 1)}, want: ErrSchemaTooNew},
		{name: "missing", mutate: `DELETE FROM schema_migrations`, want: ErrSchemaNotMigrated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withRollback(t, ctx, conn, func(tx pgx.Tx) {
				if _, err := tx.Exec(ctx, tc.mutate, tc.args...); err != nil {
					t.Fatalf("set schema state: %v", err)
				}
				if err := CheckSchema(ctx, tx, latest); !errors.Is(err, tc.want) {
					t.Fatalf("schema guard = %v, want %v", err, tc.want)
				}
			})
		})
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
			{name: "basic encrypted JSON secret", owner: owner, scheme: "basic", enabled: true, enc: secret, nonce: nonce, kver: 1, last4: "last"},
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

func TestRoleAndPartialUniqueConstraints(t *testing.T) {
	conn, ctx := testConn(t)

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx,
			`INSERT INTO users (username, role, password_hash) VALUES ('no-viewer', 'viewer', 'x')`)
		assertCheckViolation(t, err, "users_role_check")
	})

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "owned-name-provider")
		owner := insertUser(t, ctx, tx, "owned-name-user", "user")
		other := insertUser(t, ctx, tx, "owned-name-other", "user")
		insertSecretKey(t, ctx, tx, "owned-name-provider", owner, "same-name")
		insertSecretKey(t, ctx, tx, "owned-name-provider", other, "same-name")
		_, err := tx.Exec(ctx,
			`INSERT INTO provider_keys
			   (provider_id, owner_id, name, auth_scheme, secret_enc, secret_nonce, secret_kver, secret_last4)
			 VALUES ('owned-name-provider', $1::uuid, 'same-name', 'bearer', $2, $3, 1, 'last')`,
			owner, []byte{1}, make([]byte, 12))
		assertUniqueViolation(t, err, "provider_keys_owner_name_uq")
	})

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "shared-name-provider")
		if _, err := tx.Exec(ctx,
			`INSERT INTO provider_keys (provider_id, name, auth_scheme)
			 VALUES ('shared-name-provider', 'same-name', 'none')`); err != nil {
			t.Fatalf("insert first shared key: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO provider_keys (provider_id, name, auth_scheme)
			 VALUES ('shared-name-provider', 'same-name', 'none')`)
		assertUniqueViolation(t, err, "provider_keys_shared_name_uq")
	})

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "owned-alias-provider")
		owner := insertUser(t, ctx, tx, "owned-alias-user", "user")
		other := insertUser(t, ctx, tx, "owned-alias-other", "user")
		first := insertSecretKey(t, ctx, tx, "owned-alias-provider", owner, "first")
		second := insertSecretKey(t, ctx, tx, "owned-alias-provider", owner, "second")
		third := insertSecretKey(t, ctx, tx, "owned-alias-provider", other, "third")
		if _, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, 'same-alias', 'upstream')`, first); err != nil {
			t.Fatalf("insert first owned alias: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, 'same-alias', 'upstream')`, third); err != nil {
			t.Fatalf("same alias for another owner: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, 'same-alias', 'upstream')`, second)
		assertUniqueViolation(t, err, "key_models_owned_alias_uq")
	})

	withRollback(t, ctx, conn, func(tx pgx.Tx) {
		insertProvider(t, ctx, tx, "shared-alias-provider")
		var first, second string
		if err := tx.QueryRow(ctx,
			`INSERT INTO provider_keys (provider_id, name, auth_scheme)
			 VALUES ('shared-alias-provider', 'first', 'none') RETURNING id::text`).Scan(&first); err != nil {
			t.Fatalf("insert first shared key: %v", err)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO provider_keys (provider_id, name, auth_scheme)
			 VALUES ('shared-alias-provider', 'second', 'none') RETURNING id::text`).Scan(&second); err != nil {
			t.Fatalf("insert second shared key: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, 'same-alias', 'upstream')`, first); err != nil {
			t.Fatalf("insert first shared alias: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO key_models (key_id, alias, upstream_model) VALUES ($1::uuid, 'same-alias', 'upstream')`, second)
		assertUniqueViolation(t, err, "key_models_shared_alias_uq")
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
		v += 4 // PostgreSQL fires statement triggers for the user delete and its FK cascade actions
		expectBump(v, "user DELETE statement plus cascades")

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

func TestRuntimeRoleHasLeastPrivilegeGrants(t *testing.T) {
	dsn := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }() //nolint:errcheck // best-effort test cleanup
	if _, err := admin.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gnvipi_app') THEN
			CREATE ROLE gnvipi_app NOLOGIN;
		END IF;
	END $$`); err != nil {
		t.Skipf("test database user cannot create roles: %v", err)
	}

	database := fmt.Sprintf("gnvipi_grants_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+identifier+` TEMPLATE template0`); err != nil {
		t.Skipf("test database user cannot create databases: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+identifier+` WITH (FORCE)`) //nolint:errcheck // best-effort test cleanup
	}()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.Database = database
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect isolated database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // best-effort test cleanup
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset isolated public schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		t.Fatalf("create migration metadata: %v", err)
	}
	v1, err := migrations.FS.ReadFile("0001_init.up.sql")
	if err != nil {
		t.Fatalf("read v1 migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(v1)); err != nil {
		t.Fatalf("apply v1 migration: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		GRANT CREATE ON SCHEMA public TO gnvipi_app;
		GRANT ALL PRIVILEGES ON audit_log TO gnvipi_app`); err != nil {
		t.Fatalf("seed excessive v1 privileges: %v", err)
	}
	v2, err := migrations.FS.ReadFile("0002_runtime_role_grants.up.sql")
	if err != nil {
		t.Fatalf("read v2 migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(v2)); err != nil {
		t.Fatalf("apply v2 migration: %v", err)
	}

	checks := []struct {
		name  string
		query string
		want  bool
	}{
		{"schema usage", `SELECT has_schema_privilege('gnvipi_app', 'public', 'USAGE')`, true},
		{"schema create denied", `SELECT has_schema_privilege('gnvipi_app', 'public', 'CREATE')`, false},
		{"users DML", `SELECT has_table_privilege('gnvipi_app', 'users', 'SELECT')
			AND has_table_privilege('gnvipi_app', 'users', 'INSERT')
			AND has_table_privilege('gnvipi_app', 'users', 'UPDATE')
			AND has_table_privilege('gnvipi_app', 'users', 'DELETE')`, true},
		{"schema migrations read", `SELECT has_table_privilege('gnvipi_app', 'schema_migrations', 'SELECT')`, true},
		{"audit append", `SELECT has_table_privilege('gnvipi_app', 'audit_log', 'SELECT')
			AND has_table_privilege('gnvipi_app', 'audit_log', 'INSERT')`, true},
		{"audit mutation denied", `SELECT NOT has_table_privilege('gnvipi_app', 'audit_log', 'UPDATE')
			AND NOT has_table_privilege('gnvipi_app', 'audit_log', 'DELETE')`, true},
		{"runtime role does not own tables", `SELECT NOT EXISTS (
			SELECT 1 FROM pg_class WHERE relkind = 'r' AND pg_get_userbyid(relowner) = 'gnvipi_app')`, true},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			var got bool
			if err := conn.QueryRow(ctx, check.query).Scan(&got); err != nil {
				t.Fatalf("check grant: %v", err)
			}
			if got != check.want {
				t.Fatalf("grant check = %v, want %v", got, check.want)
			}
		})
	}
	down, err := migrations.FS.ReadFile("0002_runtime_role_grants.down.sql")
	if err != nil {
		t.Fatalf("read v2 down migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply v2 down migration: %v", err)
	}
	var v1Usable bool
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege('gnvipi_app', 'users', 'SELECT')
		AND has_table_privilege('gnvipi_app', 'users', 'INSERT')
		AND has_table_privilege('gnvipi_app', 'audit_log', 'INSERT')`).Scan(&v1Usable); err != nil {
		t.Fatalf("check downgraded v1 grants: %v", err)
	}
	if !v1Usable {
		t.Fatal("v2 down migration left the v1 runtime role unusable")
	}
}
