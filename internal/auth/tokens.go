package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// TokenKind separates the gateway surface from the admin surface.
type TokenKind string

const (
	TokenKindGateway TokenKind = "gateway"
	TokenKindAdmin   TokenKind = "admin"
)

// Scope is the admin-token permission; gateway tokens have no scope.
type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
)

const (
	// GatewayTokenDefaultExpiry is 365 days.
	GatewayTokenDefaultExpiry = 365 * 24 * time.Hour
	// AdminTokenDefaultExpiry is 90 days.
	AdminTokenDefaultExpiry = 90 * 24 * time.Hour
	// tokenTouchInterval limits last_used_at writes to once per minute.
	tokenTouchInterval = time.Minute
)

// Valid reports whether k is a persisted token kind.
func (k TokenKind) Valid() bool { return k == TokenKindGateway || k == TokenKindAdmin }

// Valid reports whether s is a persisted admin scope.
func (s Scope) Valid() bool { return s == ScopeRead || s == ScopeWrite }

// ScopeAllows reports whether got satisfies want. Write implies read.
func ScopeAllows(got, want Scope) bool {
	if want == ScopeWrite {
		return got == ScopeWrite
	}
	return got == ScopeRead || got == ScopeWrite
}

// RequireScope returns ErrInsufficientScope when got cannot satisfy want.
func RequireScope(got, want Scope) error {
	if ScopeAllows(got, want) {
		return nil
	}
	return ErrInsufficientScope
}

// TokenExpiry expresses either the default, an explicit time, or (gateway
// only) no expiry at all.
type TokenExpiry struct {
	At       *time.Time
	NoExpiry bool
}

// APIToken is persisted token metadata. The plaintext value is returned only
// by CreateAPIToken and is never stored.
type APIToken struct {
	ID          string
	UserID      string
	Kind        TokenKind
	Name        string
	TokenPrefix string
	Scope       Scope
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// CreateAPITokenInput describes a new personal token.
type CreateAPITokenInput struct {
	UserID string
	Kind   TokenKind
	Name   string
	Scope  Scope
	Expiry TokenExpiry
	Now    time.Time
}

const tokenColumns = `id::text, user_id::text, kind, name, token_prefix,
	COALESCE(scope, ''), expires_at, last_used_at, revoked_at, created_at`

func scanToken(row pgx.Row) (*APIToken, error) {
	var t APIToken
	var kind, scope string
	if err := row.Scan(&t.ID, &t.UserID, &kind, &t.Name, &t.TokenPrefix,
		&scope, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt, &t.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.Kind, t.Scope = TokenKind(kind), Scope(scope)
	return &t, nil
}

// CreateAPIToken validates kind/scope/expiry, generates a prefixed random
// token, stores its SHA-256 hash and returns the one-time plaintext.
func CreateAPIToken(ctx context.Context, q Querier, in CreateAPITokenInput) (string, *APIToken, error) {
	if !in.Kind.Valid() {
		return "", nil, ErrInvalidTokenKind
	}
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return "", nil, fmt.Errorf("auth: token name must be 1-100 characters")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	expiresAt, err := resolveTokenExpiry(in.Kind, in.Scope, in.Expiry, now)
	if err != nil {
		return "", nil, err
	}
	raw, prefix, err := generateAPIToken(in.Kind)
	if err != nil {
		return "", nil, err
	}
	scope := any(nil)
	if in.Kind == TokenKindAdmin {
		scope = string(in.Scope)
	}
	t, err := scanToken(q.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, kind, name, token_prefix, token_hash,
		                        scope, expires_at, created_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+tokenColumns,
		in.UserID, string(in.Kind), name, prefix, HashToken(raw), scope, expiresAt, now))
	if err != nil {
		return "", nil, err
	}
	return raw, t, nil
}

func resolveTokenExpiry(kind TokenKind, scope Scope, exp TokenExpiry, now time.Time) (*time.Time, error) {
	switch kind {
	case TokenKindAdmin:
		if !scope.Valid() {
			return nil, ErrInvalidTokenScope
		}
		if exp.NoExpiry {
			return nil, ErrTokenNoExpiryNotAllowed
		}
		if exp.At == nil {
			at := now.Add(AdminTokenDefaultExpiry)
			return &at, nil
		}
		if !exp.At.After(now) {
			return nil, ErrInvalidTokenExpiry
		}
		return exp.At, nil
	case TokenKindGateway:
		if scope != "" {
			return nil, ErrInvalidTokenScope
		}
		if exp.NoExpiry {
			if exp.At != nil {
				return nil, ErrInvalidTokenExpiry
			}
			return nil, nil
		}
		if exp.At == nil {
			at := now.Add(GatewayTokenDefaultExpiry)
			return &at, nil
		}
		if !exp.At.After(now) || exp.At.After(now.Add(GatewayTokenDefaultExpiry)) {
			return nil, ErrInvalidTokenExpiry
		}
		return exp.At, nil
	}
	return nil, ErrInvalidTokenKind
}

// generateAPIToken returns raw, display prefix and hash material. The prefix
// is the kind marker plus the first six base64url characters (4-16 bytes).
func generateAPIToken(kind TokenKind) (raw, prefix string, err error) {
	marker := "gw_"
	if kind == TokenKindAdmin {
		marker = "adm_"
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: read token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	return marker + body, marker + body[:6], nil
}

// TokenIdentity is the authenticated principal behind an API token.
type TokenIdentity struct {
	TokenID            string
	UserID             string
	Kind               TokenKind
	Scope              Scope
	Username           string
	Role               Role
	Status             Status
	MustChangePassword bool
}

// ValidateAPIToken hashes raw and returns the identity only when the token is
// unrevoked, unexpired, of the expected kind and owned by an active user.
// Wrong-kind, revoked and expired tokens all return ErrInvalidToken so the
// surface cannot be probed for token existence.
func ValidateAPIToken(ctx context.Context, q Querier, raw string, want TokenKind, now time.Time) (*TokenIdentity, error) {
	if raw == "" || !want.Valid() {
		return nil, ErrInvalidToken
	}
	if now.IsZero() {
		now = time.Now()
	}
	var v TokenIdentity
	var kind, scope, role, status string
	var expiresAt, revokedAt, lastUsedAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT t.id::text, t.user_id::text, t.kind, COALESCE(t.scope, ''),
		       t.expires_at, t.revoked_at, t.last_used_at,
		       u.username::text, u.role, u.status, u.must_change_password
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1`, HashToken(raw)).
		Scan(&v.TokenID, &v.UserID, &kind, &scope, &expiresAt, &revokedAt,
			&lastUsedAt, &v.Username, &role, &status, &v.MustChangePassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if TokenKind(kind) != want || revokedAt != nil ||
		(expiresAt != nil && !now.Before(*expiresAt)) || Status(status) != StatusActive {
		return nil, ErrInvalidToken
	}
	v.Kind, v.Scope, v.Role, v.Status = TokenKind(kind), Scope(scope), Role(role), Status(status)
	if lastUsedAt == nil || now.Sub(*lastUsedAt) >= tokenTouchInterval {
		//nolint:errcheck // best-effort last-used touch
		_, _ = q.Exec(ctx, `
			UPDATE api_tokens SET last_used_at = $2
			WHERE id = $1::uuid
			  AND (last_used_at IS NULL OR last_used_at < $3)`,
			v.TokenID, now, now.Add(-tokenTouchInterval))
	}
	return &v, nil
}

// RevokeAPIToken marks a token revoked. It is idempotent and returns
// ErrNotFound only when the id does not exist.
func RevokeAPIToken(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_tokens WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// RevokeUserAPITokens revokes every live token of a kind for a user; an empty
// kind revokes both kinds.
func RevokeUserAPITokens(ctx context.Context, q Querier, userID string, kind TokenKind) (int64, error) {
	if kind != "" && !kind.Valid() {
		return 0, ErrInvalidTokenKind
	}
	tag, err := q.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		WHERE user_id = $1::uuid AND revoked_at IS NULL
		  AND ($2::text = '' OR kind = $2)`, userID, string(kind))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListAPITokens returns a user's tokens in stable pages (default 50, max 200).
// An empty kind lists both kinds; kind separation is enforced by the filter.
func ListAPITokens(ctx context.Context, q Querier, userID string, kind TokenKind, limit, offset int) ([]*APIToken, error) {
	if kind != "" && !kind.Valid() {
		return nil, ErrInvalidTokenKind
	}
	limit, offset = clampPage(limit, offset)
	rows, err := q.Query(ctx, `SELECT `+tokenColumns+` FROM api_tokens
		WHERE user_id = $1::uuid AND ($2::text = '' OR kind = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4`,
		userID, string(kind), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []*APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
