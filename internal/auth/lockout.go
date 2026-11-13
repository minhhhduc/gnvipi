package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// LockoutConfig implements per-account and per-IP login throttling.
type LockoutConfig struct {
	AccountThreshold int           // consecutive failures that trigger a lock
	BaseLock         time.Duration // first lock duration
	MaxLock          time.Duration // cap while failures keep growing
	IPThreshold      int           // failures from one IP inside IPWindow
	IPWindow         time.Duration
}

// DefaultLockoutConfig mirrors the design: 5 failures -> 1 minute, doubling to
// 15 minutes; 20 failures per IP in 15 minutes.
func DefaultLockoutConfig() LockoutConfig {
	return LockoutConfig{
		AccountThreshold: 5,
		BaseLock:         time.Minute,
		MaxLock:          15 * time.Minute,
		IPThreshold:      20,
		IPWindow:         15 * time.Minute,
	}
}

func (c LockoutConfig) normalized() LockoutConfig {
	if c.AccountThreshold <= 0 {
		c.AccountThreshold = 5
	}
	if c.BaseLock <= 0 {
		c.BaseLock = time.Minute
	}
	if c.MaxLock <= 0 {
		c.MaxLock = 15 * time.Minute
	}
	if c.IPThreshold <= 0 {
		c.IPThreshold = 20
	}
	if c.IPWindow <= 0 {
		c.IPWindow = 15 * time.Minute
	}
	return c
}

// AccountLockDuration is the lock after n consecutive failures: 1m, 2m, 4m,
// 8m, then the 15m cap.
func (c LockoutConfig) AccountLockDuration(n int) time.Duration {
	c = c.normalized()
	if n < c.AccountThreshold {
		return 0
	}
	d := c.BaseLock
	for i := c.AccountThreshold; i < n && d < c.MaxLock; i++ {
		d *= 2
	}
	if d > c.MaxLock {
		d = c.MaxLock
	}
	return d
}

// LoginAttempt is one row in login_attempts.
type LoginAttempt struct {
	Username string
	IP       string
	Success  bool
	Now      time.Time
}

// RecordLoginAttempt appends evidence to login_attempts and updates the
// account's failed_attempts/locked_until counters in one transaction.
func RecordLoginAttempt(ctx context.Context, db DB, in LoginAttempt, cfg LockoutConfig) error {
	cfg = cfg.normalized()
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup/best-effort parse

	if _, err := tx.Exec(ctx, `
		INSERT INTO login_attempts (ts, username, ip, success)
		VALUES ($1, $2, $3::inet, $4)`,
		now, in.Username, nullableIP(in.IP), in.Success); err != nil {
		return err
	}
	if in.Success {
		_, err = tx.Exec(ctx, `
			UPDATE users SET failed_attempts = 0, locked_until = NULL,
			                 last_login_at = $2, updated_at = now()
			WHERE username = $1`, in.Username, now)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE users SET
				failed_attempts = failed_attempts + 1,
				locked_until = CASE WHEN failed_attempts + 1 >= $2::int
					THEN $3::timestamptz + (
						LEAST($4::bigint * (1 << LEAST(failed_attempts + 1 - $2::int, 4)::int),
						      $5::bigint) * interval '1 second')
					ELSE locked_until END,
				updated_at = now()
			WHERE username = $1`,
			in.Username, cfg.AccountThreshold,
			now, int64(cfg.BaseLock/time.Second), int64(cfg.MaxLock/time.Second))
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CleanupLoginAttempts removes lockout evidence older than the retention
// cutoff. Callers schedule it; the auth package owns only the deletion.
func CleanupLoginAttempts(ctx context.Context, q Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM login_attempts WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CheckLockout returns ErrAccountLocked when the account is inside a lock
// window and ErrIPLocked when the address has too many recent failures.
func CheckLockout(ctx context.Context, q Querier, username, ip string, now time.Time, cfg LockoutConfig) error {
	cfg = cfg.normalized()
	if now.IsZero() {
		now = time.Now()
	}
	var lockedUntil *time.Time
	err := q.QueryRow(ctx, `SELECT locked_until FROM users WHERE username = $1`,
		username).Scan(&lockedUntil)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Unknown usernames follow the same exponential window as stored
		// accounts. A fixed rolling window makes lock duration an account-
		// existence oracle after a real account's shorter first lock expires.
		var failures int
		var lastFailure *time.Time
		if err := q.QueryRow(ctx, `
			SELECT count(*), max(ts) FROM login_attempts
			WHERE username = $1 AND success = false`, username).Scan(&failures, &lastFailure); err != nil {
			return err
		}
		if lastFailure != nil && now.Before(lastFailure.Add(cfg.AccountLockDuration(failures))) {
			return ErrAccountLocked
		}
	case err != nil:
		return err
	case lockedUntil != nil && now.Before(*lockedUntil):
		return ErrAccountLocked
	}
	if ip == "" {
		return nil
	}
	var failures int
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM login_attempts
		WHERE ip = $1::inet AND success = false AND ts > $2`,
		ip, now.Add(-cfg.IPWindow)).Scan(&failures); err != nil {
		return err
	}
	if failures >= cfg.IPThreshold {
		return ErrIPLocked
	}
	return nil
}

// LoginConfig bundles the hashing and lockout settings for Login.
type LoginConfig struct {
	Params  Params
	Pepper  []byte
	Lockout LockoutConfig
}

func (c LoginConfig) normalized() LoginConfig {
	if c.Params.Memory == 0 {
		c.Params = DefaultParams()
	}
	c.Lockout = c.Lockout.normalized()
	return c
}

// Login verifies a username/password pair. Unknown users and disabled accounts
// burn comparable hashing work and return the same ErrInvalidCredentials as a
// wrong password. needsRehash tells the caller to rewrite the stored hash after
// a successful login.
func Login(ctx context.Context, db DB, username, password, ip string, now time.Time, cfg LoginConfig) (*User, bool, error) {
	cfg = cfg.normalized()
	if now.IsZero() {
		now = time.Now()
	}
	username = strings.TrimSpace(username)
	if err := CheckLockout(ctx, db, username, ip, now, cfg.Lockout); err != nil {
		return nil, false, err
	}
	u, err := GetUserByUsername(ctx, db, username)
	if errors.Is(err, ErrUserNotFound) {
		dummyVerification(password, cfg.Pepper)
		_ = RecordLoginAttempt(ctx, db, LoginAttempt{Username: username, IP: ip, Now: now}, cfg.Lockout) //nolint:errcheck // best-effort cleanup/best-effort parse
		return nil, false, ErrInvalidCredentials
	}
	if err != nil {
		return nil, false, err
	}
	ok, needsRehash, verifyErr := VerifyPassword(password, u.PasswordHash, cfg.Params, cfg.Pepper)
	if verifyErr != nil || !ok || u.Status != StatusActive {
		_ = RecordLoginAttempt(ctx, db, LoginAttempt{Username: username, IP: ip, Now: now}, cfg.Lockout) //nolint:errcheck // best-effort cleanup/best-effort parse
		return nil, false, ErrInvalidCredentials
	}
	if err := RecordLoginAttempt(ctx, db, LoginAttempt{Username: username, IP: ip, Success: true, Now: now}, cfg.Lockout); err != nil {
		return nil, false, err
	}
	return u, needsRehash, nil
}
