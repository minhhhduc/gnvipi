package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB creates a throwaway schema with the identity tables and the
// last-owner trigger, so DB-backed tests exercise real FKs, CHECKs and
// trigger/advisory-lock behavior. It skips when no DSN is configured.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed auth tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("schema suffix: %v", err)
	}
	schema := "auth_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		admin.Close()
		t.Fatalf("create citext extension: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET search_path TO "+schema+", public")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test pool: %v", err)
	}
	if _, err := pool.Exec(ctx, authTestSchema); err != nil {
		pool.Close()
		admin.Close()
		t.Fatalf("create auth test schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") //nolint:errcheck // best-effort cleanup/best-effort parse
		admin.Close()
	})
	return pool
}

const authTestSchema = `
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  username citext NOT NULL UNIQUE CHECK (length(username) BETWEEN 3 AND 64),
  email citext UNIQUE,
  display_name text NOT NULL DEFAULT '' CHECK (length(display_name) <= 200),
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'user')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  password_hash text NOT NULL CHECK (length(password_hash) > 0),
  password_updated_at timestamptz NOT NULL DEFAULT now(),
  must_change_password boolean NOT NULL DEFAULT false,
  failed_attempts integer NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
  locked_until timestamptz,
  last_login_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  created_by uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE sessions (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  ip inet,
  user_agent text NOT NULL DEFAULT '' CHECK (length(user_agent) <= 512),
  CHECK (expires_at > created_at)
);

CREATE TABLE api_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('gateway', 'admin')),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  token_prefix text NOT NULL CHECK (length(token_prefix) BETWEEN 4 AND 16),
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  scope text CHECK (scope IN ('read', 'write')),
  expires_at timestamptz,
  last_used_at timestamptz,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'admin' AND scope IS NOT NULL) OR (kind = 'gateway' AND scope IS NULL)),
  CHECK (expires_at IS NULL OR expires_at > created_at)
);

CREATE TABLE login_attempts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  username citext NOT NULL,
  ip inet,
  success boolean NOT NULL
);

CREATE TABLE audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  actor_type text NOT NULL CHECK (actor_type IN ('user', 'token', 'system')),
  actor_id uuid,
  user_id uuid,
  key_id uuid,
  provider_id text,
  request_id uuid,
  action text NOT NULL CHECK (length(action) BETWEEN 1 AND 80),
  target_type text NOT NULL CHECK (length(target_type) BETWEEN 1 AND 40),
  target_id text,
  ip inet,
  details jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
  CHECK (length(details::text) <= 8192)
);

CREATE FUNCTION users_guard_last_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE remaining integer;
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.role = 'owner' AND OLD.status = 'active' THEN
      PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
      SELECT count(*) INTO remaining FROM users
       WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
      IF remaining = 0 THEN
        RAISE EXCEPTION 'last active owner cannot be deleted';
      END IF;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.role = 'owner' AND OLD.status = 'active'
     AND (NEW.role <> 'owner' OR NEW.status <> 'active') THEN
    PERFORM pg_advisory_xact_lock(hashtext('gnvipi:owners'));
    SELECT count(*) INTO remaining FROM users
     WHERE role = 'owner' AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active owner cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER users_guard_last_owner_bu
BEFORE UPDATE OF role, status ON users
FOR EACH ROW EXECUTE FUNCTION users_guard_last_owner();

CREATE TRIGGER users_guard_last_owner_bd
BEFORE DELETE ON users
FOR EACH ROW EXECUTE FUNCTION users_guard_last_owner();
`

//nolint:unparam // test helper keeps explicit call-site shape
func createTestUser(t *testing.T, db *pgxpool.Pool, username string, role Role, password string) *User {
	t.Helper()
	hash, err := HashPassword(password, fastParams(), nil)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	u, err := CreateUser(t.Context(), db, CreateUserInput{
		Username: username, Role: role, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create test user %s: %v", username, err)
	}
	return u
}

func activeOwners(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE role = 'owner' AND status = 'active'`).Scan(&n); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	return n
}

func TestUsersDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()

	u := createTestUser(t, db, "alice", RoleOwner, "correct horse battery staple")
	if u.Role != RoleOwner || u.Status != StatusActive {
		t.Fatalf("unexpected user: %+v", u)
	}

	got, err := GetUserByUsername(ctx, db, "ALICE")
	if err != nil || got.ID != u.ID {
		t.Fatalf("case-insensitive lookup: %v %+v", err, got)
	}
	if _, err := GetUserByID(ctx, db, u.ID); err != nil {
		t.Fatalf("get by id: %v", err)
	}

	display := "Alice"
	mustChange := true
	yes := "alice@example.com"
	u, err = UpdateUser(ctx, db, u.ID, UpdateUserInput{
		DisplayName: &display, Email: &yes, MustChangePassword: &mustChange,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if u.DisplayName != display || u.Email != yes || !u.MustChangePassword {
		t.Fatalf("update not applied: %+v", u)
	}

	if _, err := CreateUser(ctx, db, CreateUserInput{
		Username: "alice", Role: RoleAdmin, PasswordHash: u.PasswordHash,
	}); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username: %v", err)
	}
	if _, err := CreateUser(ctx, db, CreateUserInput{
		Username: "bad-role", Role: "root", PasswordHash: u.PasswordHash,
	}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: %v", err)
	}

	if err := DeleteUser(ctx, db, u.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("deleting last owner: %v", err)
	}
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Role: rolePtr(RoleAdmin)}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting last owner: %v", err)
	}
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusDisabled)}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disabling last owner: %v", err)
	}

	second := createTestUser(t, db, "bob", RoleOwner, "correct horse battery staple")
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Role: rolePtr(RoleAdmin)}); err != nil {
		t.Fatalf("demoting with another owner: %v", err)
	}
	carol := createTestUser(t, db, "carol", RoleUser, "correct horse battery staple")
	if err := DeleteUser(ctx, db, carol.ID); err != nil {
		t.Fatalf("delete non-owner: %v", err)
	}
	if err := DeleteUser(ctx, db, second.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("delete remaining owner: %v", err)
	}
	if activeOwners(t, db) != 1 {
		t.Fatal("expected one active owner")
	}
	if users, err := ListUsers(ctx, db, 1, 0); err != nil || len(users) != 1 {
		t.Fatalf("list users: %d %v", len(users), err)
	}
}

func TestDisableRevokesCredentialsDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	u := createTestUser(t, db, "disable-user", RoleUser, "correct horse battery staple")

	raw, _, err := CreateSession(ctx, db, CreateSessionInput{UserID: u.ID}, DefaultSessionConfig())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	gwRaw, _, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: u.ID, Kind: TokenKindGateway, Name: "laptop", Expiry: TokenExpiry{NoExpiry: true},
	})
	if err != nil {
		t.Fatalf("create gateway token: %v", err)
	}
	admRaw, _, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: u.ID, Kind: TokenKindAdmin, Name: "ci", Scope: ScopeRead,
	})
	if err != nil {
		t.Fatalf("create admin token: %v", err)
	}

	// Disabling must revoke both token kinds and sessions in the same
	// transaction as the status change.
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusDisabled)}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw, time.Now(), DefaultSessionConfig()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("disabled session still valid: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindGateway, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled gateway token still valid: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, admRaw, TokenKindAdmin, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled admin token still valid: %v", err)
	}

	// Re-enabling the account must not resurrect credentials revoked by the
	// disable transition.
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusActive)}); err != nil {
		t.Fatalf("re-enable user: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw, time.Now(), DefaultSessionConfig()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("session resurrected after re-enable: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindGateway, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("gateway token resurrected after re-enable: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, admRaw, TokenKindAdmin, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("admin token resurrected after re-enable: %v", err)
	}
}

func TestConcurrentLastOwnerDemotion(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	a := createTestUser(t, db, "owner-a", RoleOwner, "correct horse battery staple")
	b := createTestUser(t, db, "owner-b", RoleOwner, "correct horse battery staple")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{a.ID, b.ID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := UpdateUser(ctx, db, id, UpdateUserInput{Role: rolePtr(RoleAdmin)})
			errs <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)

	succeeded, lastOwner := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLastOwner):
			lastOwner++
		default:
			t.Fatalf("unexpected demotion error: %v", err)
		}
	}
	if succeeded != 1 || lastOwner != 1 {
		t.Fatalf("want one success and one last-owner error, got %d/%d", succeeded, lastOwner)
	}
	if activeOwners(t, db) != 1 {
		t.Fatal("concurrent demotions left no active owner")
	}
}

func TestSessionsDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cfg := DefaultSessionConfig()
	u := createTestUser(t, db, "session-user", RoleUser, "correct horse battery staple")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	raw, sess, err := CreateSession(ctx, db, CreateSessionInput{
		UserID: u.ID, IP: "192.0.2.10", UserAgent: "test-agent", Now: now,
	}, cfg)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.ExpiresAt.Sub(now) != cfg.AbsoluteTimeout {
		t.Fatalf("absolute expiry: %v", sess.ExpiresAt)
	}
	v, err := ValidateSession(ctx, db, raw, now.Add(cfg.TouchInterval/2), cfg)
	if err != nil || v.UserID != u.ID || v.Username != u.Username {
		t.Fatalf("validate: %v %+v", err, v)
	}
	if _, err := ValidateSession(ctx, db, "not-a-token", now, cfg); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("bogus token: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw, now.Add(cfg.IdleTimeout+time.Second), cfg); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("idle expiry: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw, now.Add(cfg.AbsoluteTimeout+time.Second), cfg); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("absolute expiry: %v", err)
	}

	if err := RevokeSession(ctx, db, raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw, now, cfg); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked session: %v", err)
	}
	if n, err := CleanupSessions(ctx, db, now.Add(cfg.AbsoluteTimeout+time.Second), cfg); err != nil || n == 0 {
		t.Fatalf("cleanup: %d %v", n, err)
	}

	raw2, _, err := CreateSession(ctx, db, CreateSessionInput{UserID: u.ID, Now: now}, cfg)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusDisabled)}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := ValidateSession(ctx, db, raw2, now, cfg); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("disabled user session: %v", err)
	}
}

func TestTokensDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	u := createTestUser(t, db, "token-user", RoleUser, "correct horse battery staple")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	gwRaw, gw, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: u.ID, Kind: TokenKindGateway, Name: "laptop",
		Expiry: TokenExpiry{NoExpiry: true}, Now: now,
	})
	if err != nil {
		t.Fatalf("gateway token: %v", err)
	}
	if gw.ExpiresAt != nil || gw.Scope != "" || gw.TokenPrefix == "" {
		t.Fatalf("unexpected gateway token: %+v", gw)
	}
	id, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindGateway, now)
	if err != nil || id.UserID != u.ID || id.Kind != TokenKindGateway {
		t.Fatalf("validate gateway: %v %+v", err, id)
	}
	if _, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindAdmin, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("kind separation: %v", err)
	}

	admRaw, adm, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: u.ID, Kind: TokenKindAdmin, Name: "ci", Scope: ScopeRead, Now: now,
	})
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}
	if adm.ExpiresAt == nil || adm.ExpiresAt.Sub(now) != AdminTokenDefaultExpiry {
		t.Fatalf("admin default expiry: %+v", adm)
	}
	admID, err := ValidateAPIToken(ctx, db, admRaw, TokenKindAdmin, now)
	if err != nil || admID.Scope != ScopeRead {
		t.Fatalf("validate admin: %v %+v", err, admID)
	}
	if err := RequireScope(admID.Scope, ScopeWrite); !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("read token write scope: %v", err)
	}
	var used *time.Time
	if err := db.QueryRow(ctx, `SELECT last_used_at FROM api_tokens WHERE id = $1::uuid`, adm.ID).Scan(&used); err != nil || used == nil {
		t.Fatalf("last_used_at not updated: %v %v", used, err)
	}

	shortAt := now.Add(time.Hour)
	expRaw, _, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: u.ID, Kind: TokenKindGateway, Name: "short", Expiry: TokenExpiry{At: &shortAt}, Now: now,
	})
	if err != nil {
		t.Fatalf("short gateway token: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, expRaw, TokenKindGateway, shortAt.Add(time.Second)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}

	if err := RevokeAPIToken(ctx, db, adm.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, admRaw, TokenKindAdmin, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token: %v", err)
	}
	if err := RevokeAPIToken(ctx, db, adm.ID); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}

	tokens, err := ListAPITokens(ctx, db, u.ID, TokenKindGateway, 2, 0)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("list gateway: %d %v", len(tokens), err)
	}
	for _, tok := range tokens {
		if tok.Kind != TokenKindGateway {
			t.Fatalf("kind filter leaked %s", tok.Kind)
		}
	}
	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusDisabled)}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindGateway, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled user token: %v", err)
	}
}

func TestLoginAndLockoutDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	const password = "correct horse battery staple"
	u := createTestUser(t, db, "login-user", RoleUser, password)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	cfg := LoginConfig{Params: fastParams()}

	if _, needsRehash, err := Login(ctx, db, "login-user", password, "192.0.2.20", now, cfg); err != nil || needsRehash {
		t.Fatalf("first login: %v rehash=%v", err, needsRehash)
	}
	for i := 0; i < DefaultLockoutConfig().AccountThreshold; i++ {
		if _, _, err := Login(ctx, db, "login-user", "wrong password", "192.0.2.20", now.Add(time.Duration(i)*time.Second), cfg); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if err := CheckLockout(ctx, db, "login-user", "", now.Add(time.Minute), DefaultLockoutConfig()); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("account not locked: %v", err)
	}
	if _, _, err := Login(ctx, db, "login-user", password, "192.0.2.20", now.Add(time.Minute), cfg); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("locked login: %v", err)
	}
	// The first lock lasts one minute; a correct login after it succeeds and
	// clears the counters.
	if _, _, err := Login(ctx, db, "login-user", password, "192.0.2.20", now.Add(3*time.Minute), cfg); err != nil {
		t.Fatalf("login after lock: %v", err)
	}
	var failed int
	if err := db.QueryRow(ctx, `SELECT failed_attempts FROM users WHERE id = $1::uuid`, u.ID).Scan(&failed); err != nil || failed != 0 {
		t.Fatalf("failed_attempts not reset: %d %v", failed, err)
	}

	// Unknown users are indistinguishable from wrong passwords but still get a
	// login_attempts row.
	if _, _, err := Login(ctx, db, "nobody", "whatever-password", "192.0.2.21", now, cfg); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE username = 'nobody'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unknown-user attempt row: %d %v", n, err)
	}

	// Unknown-user locks follow the same backoff window as stored accounts.
	// Keeping an unknown name locked for the entire IP window leaks whether an
	// account exists once the real account's one-minute lock expires.
	for i := range DefaultLockoutConfig().AccountThreshold {
		if err := RecordLoginAttempt(ctx, db, LoginAttempt{
			Username: "unknown-duration",
			IP:       "192.0.2.22",
			Now:      now.Add(time.Duration(i) * time.Second),
		}, DefaultLockoutConfig()); err != nil {
			t.Fatalf("record unknown-user failure %d: %v", i+1, err)
		}
	}
	if err := CheckLockout(ctx, db, "unknown-duration", "", now.Add(time.Minute), DefaultLockoutConfig()); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("unknown account not locked during first window: %v", err)
	}
	if err := CheckLockout(ctx, db, "unknown-duration", "", now.Add(2*time.Minute), DefaultLockoutConfig()); err != nil {
		t.Fatalf("unknown account remained locked beyond first window: %v", err)
	}

	// 20 recent failures from one address lock that address.
	for i := 0; i < DefaultLockoutConfig().IPThreshold; i++ {
		if _, err := db.Exec(ctx, `INSERT INTO login_attempts (ts, username, ip, success)
			VALUES ($1, 'ip-user', '198.51.100.7', false)`, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("insert IP failure: %v", err)
		}
	}
	if err := CheckLockout(ctx, db, "unrelated", "198.51.100.7", now.Add(time.Minute), DefaultLockoutConfig()); !errors.Is(err, ErrIPLocked) {
		t.Fatalf("IP not locked: %v", err)
	}

	if _, err := UpdateUser(ctx, db, u.ID, UpdateUserInput{Status: statusPtr(StatusDisabled)}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, _, err := Login(ctx, db, "login-user", password, "192.0.2.20", now.Add(time.Hour), cfg); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login: %v", err)
	}
}

func TestCleanupLoginAttemptsDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, attempt := range []LoginAttempt{
		{Username: "expired-attempt", Now: cutoff.Add(-time.Second)},
		{Username: "retained-attempt", Now: cutoff.Add(time.Second)},
	} {
		if err := RecordLoginAttempt(ctx, db, attempt, DefaultLockoutConfig()); err != nil {
			t.Fatalf("record %s: %v", attempt.Username, err)
		}
	}

	deleted, err := CleanupLoginAttempts(ctx, db, cutoff)
	if err != nil {
		t.Fatalf("cleanup login attempts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted login attempts = %d, want 1", deleted)
	}
	var retained int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE username = 'retained-attempt'`).Scan(&retained); err != nil {
		t.Fatalf("count retained login attempt: %v", err)
	}
	if retained != 1 {
		t.Fatalf("retained login attempts = %d, want 1", retained)
	}
}

func TestBootstrapAndRecoveryDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cfg := BootstrapConfig{Params: fastParams()}
	const password = "correct horse battery staple"

	owner, err := CreateFirstOwner(ctx, db, CreateOwnerInput{
		Username: "owner", Email: "owner@example.com", Password: password,
	}, cfg)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if owner.Role != RoleOwner {
		t.Fatalf("bootstrap role: %s", owner.Role)
	}
	if _, err := CreateFirstOwner(ctx, db, CreateOwnerInput{
		Username: "second", Password: password,
	}, cfg); !errors.Is(err, ErrUsersExist) {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n := auditCount(t, db, "user.bootstrap"); n != 1 {
		t.Fatalf("bootstrap audit rows: %d", n)
	}

	raw, _, err := CreateSession(ctx, db, CreateSessionInput{UserID: owner.ID}, DefaultSessionConfig())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	gwRaw, _, err := CreateAPIToken(ctx, db, CreateAPITokenInput{
		UserID: owner.ID, Kind: TokenKindGateway, Name: "laptop", Expiry: TokenExpiry{NoExpiry: true},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	const resetPassword = "marble-orchard-77"
	if err := ResetPassword(ctx, db, owner.ID, resetPassword, cfg); err != nil {
		t.Fatalf("reset password: %v", err)
	}
	after, err := GetUserByID(ctx, db, owner.ID)
	if err != nil || !after.MustChangePassword {
		t.Fatalf("reset did not force change: %+v %v", after, err)
	}
	if _, err := ValidateSession(ctx, db, raw, time.Now(), DefaultSessionConfig()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("reset left session valid: %v", err)
	}
	if _, err := ValidateAPIToken(ctx, db, gwRaw, TokenKindGateway, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reset left token valid: %v", err)
	}
	if n := auditCount(t, db, "user.password_reset"); n != 1 {
		t.Fatalf("reset audit rows: %d", n)
	}

	current, _, err := CreateSession(ctx, db, CreateSessionInput{UserID: owner.ID}, DefaultSessionConfig())
	if err != nil {
		t.Fatalf("current session: %v", err)
	}
	other, _, err := CreateSession(ctx, db, CreateSessionInput{UserID: owner.ID}, DefaultSessionConfig())
	if err != nil {
		t.Fatalf("other session: %v", err)
	}
	const newPassword = "velvet-cascade-42"
	if err := ChangePassword(ctx, db, owner.ID, current, resetPassword, newPassword, cfg, AuditEntry{ActorType: "user", ActorID: owner.ID}); err != nil {
		t.Fatalf("change password: %v", err)
	}
	after, err = GetUserByID(ctx, db, owner.ID)
	if err != nil || after.MustChangePassword {
		t.Fatalf("change did not clear flag: %+v %v", after, err)
	}
	if _, _, err := VerifyPassword(newPassword, after.PasswordHash, fastParams(), nil); err != nil {
		t.Fatalf("new hash: %v", err)
	}
	if _, err := ValidateSession(ctx, db, other, time.Now(), DefaultSessionConfig()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("other session not revoked: %v", err)
	}
	if _, err := ValidateSession(ctx, db, current, time.Now(), DefaultSessionConfig()); err != nil {
		t.Fatalf("current session should remain valid: %v", err)
	}
	if n := auditCount(t, db, "user.password_change"); n != 1 {
		t.Fatalf("change audit rows: %d", n)
	}
}

func TestConcurrentBootstrap(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cfg := BootstrapConfig{Params: fastParams()}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"owner-one", "owner-two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			_, err := CreateFirstOwner(ctx, db, CreateOwnerInput{
				Username: name, Password: "correct horse battery staple",
			}, cfg)
			errs <- err
		}(name)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, exists := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrUsersExist):
			exists++
		default:
			t.Fatalf("unexpected bootstrap error: %v", err)
		}
	}
	if success != 1 || exists != 1 {
		t.Fatalf("want one owner and one ErrUsersExist, got %d/%d", success, exists)
	}
	if n, err := CountUsers(ctx, db); err != nil || n != 1 {
		t.Fatalf("user count after concurrent bootstrap: %d %v", n, err)
	}
}

func TestAuditWriteDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	u := createTestUser(t, db, "audit-user", RoleUser, "correct horse battery staple")
	err := WriteAudit(ctx, db, AuditEntry{
		ActorType: "user", ActorID: u.ID, UserID: u.ID,
		Action: "user.test", TargetType: "user", TargetID: u.ID,
		IP: "192.0.2.30",
		Details: map[string]any{
			"role": "user", "secret": "do-not-store",
			"nested": map[string]any{"token": "do-not-store"},
		},
	})
	if err != nil {
		t.Fatalf("write audit: %v", err)
	}
	var raw string
	if err := db.QueryRow(ctx, `SELECT details::text FROM audit_log WHERE action = 'user.test'`).Scan(&raw); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	details := []byte(raw)
	for _, forbidden := range []string{"do-not-store"} {
		if bytes.Contains(details, []byte(forbidden)) {
			t.Fatalf("audit row contains %q: %s", forbidden, raw)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(details, &got); err != nil {
		t.Fatalf("decode audit details: %v", err)
	}
	if got["role"] != "user" {
		t.Fatalf("audit row lost metadata: %s", raw)
	}
}

func auditCount(t *testing.T, db *pgxpool.Pool, action string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return n
}

func rolePtr(r Role) *Role       { return &r }
func statusPtr(s Status) *Status { return &s }
