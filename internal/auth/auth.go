// Package auth implements the GNVIPI authentication core: Argon2id password
// hashing, users, browser sessions, API tokens, login lockout, CSRF helpers,
// audit writes and owner bootstrap/recovery.
//
// The package stores only hashes for sessions and API tokens. Plaintext
// passwords and tokens never leave the caller.
package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgx used by single-statement helpers. Both
// *pgxpool.Pool and pgx.Tx satisfy it.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// DB additionally starts transactions. *pgxpool.Pool satisfies DB.
type DB interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Role is a user role. There is deliberately no viewer role.
type Role string

const (
	RoleOwner Role = "owner"
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Valid reports whether r is one of the persisted roles.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleUser:
		return true
	}
	return false
}

// Status is a user account status.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// Valid reports whether s is one of the persisted statuses.
func (s Status) Valid() bool {
	return s == StatusActive || s == StatusDisabled
}

var (
	ErrNotFound                = errors.New("auth: not found")
	ErrUserNotFound            = errors.New("auth: user not found")
	ErrInvalidRole             = errors.New("auth: invalid role")
	ErrInvalidStatus           = errors.New("auth: invalid status")
	ErrUsernameTaken           = errors.New("auth: username already exists")
	ErrEmailTaken              = errors.New("auth: email already exists")
	ErrLastOwner               = errors.New("auth: last active owner cannot be removed, demoted or disabled")
	ErrInvalidCredentials      = errors.New("auth: invalid credentials")
	ErrAccountLocked           = errors.New("auth: account temporarily locked")
	ErrIPLocked                = errors.New("auth: too many failed attempts from this address")
	ErrPasswordChangeRequired  = errors.New("auth: password change required")
	ErrInvalidSession          = errors.New("auth: invalid or expired session")
	ErrInvalidToken            = errors.New("auth: invalid or expired token")
	ErrInsufficientScope       = errors.New("auth: token scope does not permit this action")
	ErrInvalidTokenKind        = errors.New("auth: token kind must be gateway or admin")
	ErrInvalidTokenScope       = errors.New("auth: admin tokens require a read or write scope; gateway tokens have none")
	ErrInvalidTokenExpiry      = errors.New("auth: token expiry must be a future time within the allowed window")
	ErrTokenNoExpiryNotAllowed = errors.New("auth: admin tokens require an expiry")
	ErrUsersExist              = errors.New("auth: users already exist; bootstrap creates the first owner only")
	ErrAuditDetailsTooLarge    = errors.New("auth: audit details exceed 8192 bytes")
	ErrAuditInvalidDetails     = errors.New("auth: audit details must be an object of scalar metadata")
)
