package auth

import (
	"context"
	"errors"
	"time"
)

// Default retention windows from the design: lockout evidence is kept for 30
// days and audit rows for a year.
const (
	DefaultLoginAttemptRetention = 30 * 24 * time.Hour
	DefaultAuditRetention        = 365 * 24 * time.Hour
)

// retentionLockSQL is the single-flight lock for the retention pass. The
// transaction-scoped variant releases itself on commit or rollback, so a
// crashed pass can never leave the job wedged.
const retentionLockSQL = `SELECT pg_try_advisory_xact_lock(hashtext('gnvipi:retention'))`

// ErrRetentionBusy reports that another gateway process already holds the
// retention lock. The loser of the race skips its turn; it is not a failure.
var ErrRetentionBusy = errors.New("auth: retention pass is running in another process")

// RetentionConfig bounds how long session, lockout and audit evidence is kept.
type RetentionConfig struct {
	Sessions      SessionConfig
	LoginAttempts time.Duration
	Audit         time.Duration
}

// DefaultRetentionConfig returns the design defaults: 30 days of lockout
// evidence, 365 days of audit, and the standard session windows.
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		Sessions:      DefaultSessionConfig(),
		LoginAttempts: DefaultLoginAttemptRetention,
		Audit:         DefaultAuditRetention,
	}
}

func (c RetentionConfig) normalized() RetentionConfig {
	c.Sessions = c.Sessions.normalized()
	if c.LoginAttempts <= 0 {
		c.LoginAttempts = DefaultLoginAttemptRetention
	}
	if c.Audit <= 0 {
		c.Audit = DefaultAuditRetention
	}
	return c
}

// RetentionResult counts the rows removed by one pass.
type RetentionResult struct {
	Sessions      int64
	LoginAttempts int64
	Audit         int64
	// AuditSkipped reports that no maintenance role was configured, so audit
	// rows were left untouched. The application role is denied DELETE.
	AuditSkipped bool
}

// RunRetention is the one runner for the retention job: it deletes expired and
// revoked sessions, lockout evidence older than the login-attempt window, and
// audit rows older than the audit window. Every delete is cutoff-based, so a
// repeated or retried pass is idempotent.
//
// app must be able to delete sessions and login attempts. maintenance must be
// the phase-2 retention role, which owns DELETE on audit_log; a nil maintenance
// skips audit pruning and sets AuditSkipped.
//
// The pass holds pg_try_advisory_xact_lock('gnvipi:retention') for its whole
// duration, taken on the maintenance connection when one is configured and on
// the application connection otherwise. A concurrent caller therefore gets
// ErrRetentionBusy and deletes nothing. Application deletes commit before audit
// pruning, so a failed audit delete never discards a completed session cleanup;
// the next pass retries the audit cutoff.
func RunRetention(ctx context.Context, app, maintenance DB, now time.Time, cfg RetentionConfig) (RetentionResult, error) {
	cfg = cfg.normalized()
	if now.IsZero() {
		now = time.Now()
	}
	var res RetentionResult

	locker := app
	if maintenance != nil {
		locker = maintenance
	}
	lockTx, err := locker.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = lockTx.Rollback(ctx) }() //nolint:errcheck // rollback after a committed transaction is a no-op

	var locked bool
	if err := lockTx.QueryRow(ctx, retentionLockSQL).Scan(&locked); err != nil {
		return res, err
	}
	if !locked {
		return res, ErrRetentionBusy
	}

	if res.Sessions, res.LoginAttempts, err = pruneAuthEvidence(ctx, app, now, cfg); err != nil {
		return res, err
	}
	if maintenance == nil {
		res.AuditSkipped = true
	} else if res.Audit, err = CleanupAudit(ctx, lockTx, now.Add(-cfg.Audit)); err != nil {
		return res, err
	}
	if err := lockTx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// pruneAuthEvidence removes expired/revoked sessions and stale login attempts in
// one transaction owned by the application role.
func pruneAuthEvidence(ctx context.Context, app DB, now time.Time, cfg RetentionConfig) (sessions, attempts int64, err error) {
	tx, err := app.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after a committed transaction is a no-op

	if sessions, err = CleanupSessions(ctx, tx, now, cfg.Sessions); err != nil {
		return 0, 0, err
	}
	if attempts, err = CleanupLoginAttempts(ctx, tx, now.Add(-cfg.LoginAttempts)); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return sessions, attempts, nil
}

// CleanupAudit deletes audit rows older than the retention cutoff. It must run
// as the maintenance role: the application role holds neither UPDATE nor DELETE
// on audit_log.
func CleanupAudit(ctx context.Context, q Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM audit_log WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
