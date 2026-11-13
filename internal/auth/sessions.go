package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const SessionCookieName = "gw_session"

// SessionConfig controls browser sessions.
type SessionConfig struct {
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	TouchInterval   time.Duration
}

// DefaultSessionConfig is the design default: 12h idle, 7d absolute, and at
// most one last_seen_at write per minute.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		IdleTimeout:     12 * time.Hour,
		AbsoluteTimeout: 7 * 24 * time.Hour,
		TouchInterval:   time.Minute,
	}
}

func (c SessionConfig) normalized() SessionConfig {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 12 * time.Hour
	}
	if c.AbsoluteTimeout <= 0 {
		c.AbsoluteTimeout = 7 * 24 * time.Hour
	}
	if c.TouchInterval <= 0 {
		c.TouchInterval = time.Minute
	}
	return c
}

// NewSessionToken returns 32 random bytes as base64url (43 characters).
func NewSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken is the SHA-256 digest stored for sessions and API tokens.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// Session is persisted session metadata; the raw token is never stored.
type Session struct {
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
}

// CreateSession inserts a session row and returns the one-time plaintext token.
// The caller sets the cookie; only the hash reaches the database.
func CreateSession(ctx context.Context, q Querier, in CreateSessionInput, cfg SessionConfig) (string, *Session, error) {
	cfg = cfg.normalized()
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	raw, err := NewSessionToken()
	if err != nil {
		return "", nil, err
	}
	s := &Session{
		UserID:     in.UserID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(cfg.AbsoluteTimeout),
		IP:         in.IP,
		UserAgent:  in.UserAgent,
	}
	_, err = q.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at,
		                      expires_at, ip, user_agent)
		VALUES ($1, $2::uuid, $3, $4, $5, $6::inet, $7)`,
		HashToken(raw), in.UserID, s.CreatedAt, s.LastSeenAt, s.ExpiresAt,
		nullableIP(in.IP), in.UserAgent)
	if err != nil {
		return "", nil, err
	}
	return raw, s, nil
}

// CreateSessionInput describes a new login session.
type CreateSessionInput struct {
	UserID    string
	IP        string
	UserAgent string
	Now       time.Time
}

// ValidatedSession is the authenticated identity behind a session cookie.
type ValidatedSession struct {
	UserID             string
	Username           string
	Role               Role
	Status             Status
	MustChangePassword bool
	ExpiresAt          time.Time
	LastSeenAt         time.Time
}

// ValidateSession looks up a raw token, rejects revoked, idle-expired,
// absolutely expired and disabled-user sessions, and refreshes last_seen_at at
// most once per TouchInterval. All failures are ErrInvalidSession so callers
// cannot distinguish the reason.
func ValidateSession(ctx context.Context, q Querier, raw string, now time.Time, cfg SessionConfig) (*ValidatedSession, error) {
	cfg = cfg.normalized()
	if raw == "" {
		return nil, ErrInvalidSession
	}
	if now.IsZero() {
		now = time.Now()
	}
	var v ValidatedSession
	var role, status string
	var revokedAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT s.user_id::text, s.last_seen_at, s.expires_at, s.revoked_at,
		       u.username::text, u.role, u.status, u.must_change_password
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, HashToken(raw)).
		Scan(&v.UserID, &v.LastSeenAt, &v.ExpiresAt, &revokedAt,
			&v.Username, &role, &status, &v.MustChangePassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidSession
	}
	if err != nil {
		return nil, err
	}
	v.Role, v.Status = Role(role), Status(status)
	if revokedAt != nil || !now.Before(v.ExpiresAt) ||
		now.Sub(v.LastSeenAt) > cfg.IdleTimeout || v.Status != StatusActive {
		return nil, ErrInvalidSession
	}
	if now.Sub(v.LastSeenAt) >= cfg.TouchInterval {
		_, _ = q.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1`, //nolint:errcheck // best-effort cleanup/best-effort parse
			HashToken(raw), now)
	}
	return &v, nil
}

// RevokeSession marks one session revoked. It is idempotent.
func RevokeSession(ctx context.Context, q Querier, raw string) error {
	if raw == "" {
		return nil
	}
	_, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`, HashToken(raw))
	return err
}

// RevokeAllSessions revokes every session for a user.
func RevokeAllSessions(ctx context.Context, q Querier, userID string) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RevokeUserSessionsExcept revokes all but one session token (the current
// session after a password change). An empty keep token revokes all.
func RevokeUserSessionsExcept(ctx context.Context, q Querier, userID, keepToken string) (int64, error) {
	var keep any
	if keepToken != "" {
		keep = HashToken(keepToken)
	}
	tag, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1::uuid AND revoked_at IS NULL
		  AND ($2::bytea IS NULL OR token_hash <> $2)`, userID, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupSessions deletes absolutely expired, idle-expired and revoked
// sessions. It is safe to run on startup and periodically.
func CleanupSessions(ctx context.Context, q Querier, now time.Time, cfg SessionConfig) (int64, error) {
	cfg = cfg.normalized()
	if now.IsZero() {
		now = time.Now()
	}
	tag, err := q.Exec(ctx, `
		DELETE FROM sessions
		WHERE expires_at <= $1 OR last_seen_at <= $2 OR revoked_at IS NOT NULL`,
		now, now.Add(-cfg.IdleTimeout))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SessionCookie builds the gw_session cookie: HttpOnly, SameSite=Strict,
// Path=/admin and Secure when TLS is enabled.
func SessionCookie(token string, secure bool, cfg SessionConfig) *http.Cookie {
	cfg = cfg.normalized()
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(cfg.AbsoluteTimeout.Seconds()),
	}
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
}

// nullableIP converts an optional address to a pgx inet value.
func nullableIP(ip string) any {
	if ip == "" {
		return nil
	}
	return ip
}
