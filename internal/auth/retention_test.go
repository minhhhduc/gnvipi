package auth

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultRetentionConfig(t *testing.T) {
	cfg := DefaultRetentionConfig()
	if cfg.LoginAttempts != 30*24*time.Hour {
		t.Fatalf("login attempt retention = %v, want 30 days", cfg.LoginAttempts)
	}
	if cfg.Audit != 365*24*time.Hour {
		t.Fatalf("audit retention = %v, want 365 days", cfg.Audit)
	}
	if cfg.Sessions.IdleTimeout != 12*time.Hour || cfg.Sessions.AbsoluteTimeout != 7*24*time.Hour {
		t.Fatalf("session config not carried: %+v", cfg.Sessions)
	}

	// Zero values fall back to the design defaults instead of deleting history
	// immediately or retaining it forever.
	normalized := RetentionConfig{}.normalized()
	if normalized.LoginAttempts != DefaultLoginAttemptRetention || normalized.Audit != DefaultAuditRetention {
		t.Fatalf("normalized = %+v", normalized)
	}
	if normalized.Sessions.IdleTimeout != 12*time.Hour {
		t.Fatalf("normalized session config = %+v", normalized.Sessions)
	}
}

// TestRunRetentionPrunesAllTablesDB covers the phase-2 contract: one pass
// removes expired and revoked sessions, login attempts older than 30 days and
// audit rows older than the retention window, at their exact cutoffs.
func TestRunRetentionPrunesAllTablesDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	u := createTestUser(t, db, "retention-user", RoleUser, "correct horse battery staple")
	cfg := DefaultRetentionConfig()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	type session struct {
		token    string
		created  time.Time
		lastSeen time.Time
		revoked  bool
	}
	// Absolute expiry deletes expires_at <= now; idle expiry deletes
	// last_seen_at <= now-12h; revoked rows go regardless of expiry.
	sessions := []session{
		{token: "absolute-expired", created: now.Add(-8 * 24 * time.Hour), lastSeen: now.Add(-8 * 24 * time.Hour)},
		{token: "absolute-boundary", created: now.Add(-7 * 24 * time.Hour), lastSeen: now.Add(-time.Hour)},
		{token: "idle-expired", created: now.Add(-13 * time.Hour), lastSeen: now.Add(-13 * time.Hour)},
		{token: "idle-boundary", created: now.Add(-24 * time.Hour), lastSeen: now.Add(-cfg.Sessions.IdleTimeout)},
		{token: "active-inside-idle", created: now.Add(-24 * time.Hour), lastSeen: now.Add(-cfg.Sessions.IdleTimeout + time.Second)},
		{token: "revoked", created: now.Add(-time.Hour), lastSeen: now.Add(-time.Hour), revoked: true},
		{token: "active", created: now, lastSeen: now},
	}
	for _, s := range sessions {
		var revokedAt any
		if s.revoked {
			revokedAt = now
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at,
			                      expires_at, revoked_at)
			VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
			HashToken(s.token), u.ID, s.created, s.lastSeen,
			s.created.Add(cfg.Sessions.AbsoluteTimeout), revokedAt); err != nil {
			t.Fatalf("insert session %s: %v", s.token, err)
		}
	}

	attemptCutoff := now.Add(-cfg.LoginAttempts)
	for _, attempt := range []struct {
		username string
		ts       time.Time
	}{
		{"retention-old", attemptCutoff.Add(-time.Second)},
		{"retention-boundary", attemptCutoff},
		{"retention-recent", now.Add(-time.Hour)},
	} {
		if err := RecordLoginAttempt(ctx, db, LoginAttempt{
			Username: attempt.username, Now: attempt.ts,
		}, DefaultLockoutConfig()); err != nil {
			t.Fatalf("record attempt %s: %v", attempt.username, err)
		}
	}

	auditCutoff := now.Add(-cfg.Audit)
	for _, row := range []struct {
		target string
		ts     time.Time
	}{
		{"audit-old", auditCutoff.Add(-time.Second)},
		{"audit-boundary", auditCutoff},
		{"audit-recent", now.Add(-24 * time.Hour)},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO audit_log (ts, actor_type, action, target_type, target_id)
			VALUES ($1, 'system', 'retention.test', 'retention', $2)`, row.ts, row.target); err != nil {
			t.Fatalf("insert audit %s: %v", row.target, err)
		}
	}

	res, err := RunRetention(ctx, db, db, now, cfg)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if res.Sessions != 5 || res.LoginAttempts != 1 || res.Audit != 1 || res.AuditSkipped {
		t.Fatalf("retention result = %+v, want sessions=5 attempts=1 audit=1", res)
	}

	for _, want := range []struct {
		token   string
		present bool
	}{
		{"absolute-expired", false},
		{"absolute-boundary", false},
		{"idle-expired", false},
		{"idle-boundary", false},
		{"active-inside-idle", true},
		{"revoked", false},
		{"active", true},
	} {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = $1`,
			HashToken(want.token)).Scan(&n); err != nil {
			t.Fatalf("count session %s: %v", want.token, err)
		}
		if (n == 1) != want.present {
			t.Fatalf("session %s present = %v, want %v", want.token, n == 1, want.present)
		}
	}

	for _, want := range []struct {
		username string
		present  bool
	}{
		{"retention-old", false},
		{"retention-boundary", true},
		{"retention-recent", true},
	} {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE username = $1`,
			want.username).Scan(&n); err != nil {
			t.Fatalf("count attempt %s: %v", want.username, err)
		}
		if (n == 1) != want.present {
			t.Fatalf("attempt %s present = %v, want %v", want.username, n == 1, want.present)
		}
	}

	for _, want := range []struct {
		target  string
		present bool
	}{
		{"audit-old", false},
		{"audit-boundary", true},
		{"audit-recent", true},
	} {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = $1`,
			want.target).Scan(&n); err != nil {
			t.Fatalf("count audit %s: %v", want.target, err)
		}
		if (n == 1) != want.present {
			t.Fatalf("audit %s present = %v, want %v", want.target, n == 1, want.present)
		}
	}

	// A second pass on unchanged data is a no-op, so a retried or duplicated
	// schedule cannot delete retained rows.
	again, err := RunRetention(ctx, db, db, now, cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.Sessions != 0 || again.LoginAttempts != 0 || again.Audit != 0 {
		t.Fatalf("second run deleted rows: %+v", again)
	}
}

// TestRunRetentionSkipsAuditWithoutMaintenanceDB pins the grant boundary: the
// application role is denied DELETE on audit_log, so audit pruning is opt-in
// through the maintenance role and everything else still runs.
func TestRunRetentionSkipsAuditWithoutMaintenanceDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cfg := DefaultRetentionConfig()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	if _, err := db.Exec(ctx, `
		INSERT INTO audit_log (ts, actor_type, action, target_type, target_id)
		VALUES ($1, 'system', 'retention.test', 'retention', 'audit-retained')`,
		now.Add(-cfg.Audit-time.Hour)); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	if err := RecordLoginAttempt(ctx, db, LoginAttempt{
		Username: "retention-orphan", Now: now.Add(-cfg.LoginAttempts - time.Hour),
	}, DefaultLockoutConfig()); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	res, err := RunRetention(ctx, db, nil, now, cfg)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if !res.AuditSkipped || res.Audit != 0 {
		t.Fatalf("audit handling = %+v, want skipped", res)
	}
	if res.LoginAttempts != 1 {
		t.Fatalf("login attempts deleted = %d, want 1", res.LoginAttempts)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = 'audit-retained'`).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows deleted without a maintenance role: %d", n)
	}
}

// TestRunRetentionAdvisoryLockDB proves the pass is single-flight: a process
// that does not hold the lock reports ErrRetentionBusy and touches nothing.
func TestRunRetentionAdvisoryLockDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cfg := DefaultRetentionConfig()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	if err := RecordLoginAttempt(ctx, db, LoginAttempt{
		Username: "retention-locked", Now: now.Add(-cfg.LoginAttempts - time.Hour),
	}, DefaultLockoutConfig()); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }() //nolint:errcheck // best-effort test cleanup
	var locked bool
	if err := holder.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtext('gnvipi:retention'))`).Scan(&locked); err != nil || !locked {
		t.Fatalf("acquire lock in holder: locked=%v err=%v", locked, err)
	}

	// The application connection is the lock holder when no maintenance role is
	// configured, so a lock taken elsewhere must make this pass skip.
	res, err := RunRetention(ctx, db, nil, now, cfg)
	if !errors.Is(err, ErrRetentionBusy) {
		t.Fatalf("concurrent pass error = %v, want ErrRetentionBusy", err)
	}
	if res != (RetentionResult{}) {
		t.Fatalf("concurrent pass reported work: %+v", res)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE username = 'retention-locked'`).Scan(&n); err != nil {
		t.Fatalf("count attempt: %v", err)
	}
	if n != 1 {
		t.Fatalf("concurrent pass deleted %d attempts", 1-n)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	res, err = RunRetention(ctx, db, nil, now, cfg)
	if err != nil {
		t.Fatalf("pass after lock release: %v", err)
	}
	if res.LoginAttempts != 1 {
		t.Fatalf("pass after lock release deleted %d attempts, want 1", res.LoginAttempts)
	}
}

func TestCleanupAuditDB(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		target string
		ts     time.Time
	}{
		{"cleanup-old", cutoff.Add(-time.Second)},
		{"cleanup-boundary", cutoff},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO audit_log (ts, actor_type, action, target_type, target_id)
			VALUES ($1, 'system', 'retention.test', 'retention', $2)`, row.ts, row.target); err != nil {
			t.Fatalf("insert audit %s: %v", row.target, err)
		}
	}
	deleted, err := CleanupAudit(ctx, db, cutoff)
	if err != nil {
		t.Fatalf("cleanup audit: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted audit rows = %d, want 1", deleted)
	}
}
