package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// User is a row from users. PasswordHash is never returned by an API; the
// repository exposes it because login has to verify it.
type User struct {
	ID                 string
	Username           string
	Email              string
	DisplayName        string
	Role               Role
	Status             Status
	PasswordHash       string
	PasswordUpdatedAt  time.Time
	MustChangePassword bool
	FailedAttempts     int
	LockedUntil        *time.Time
	LastLoginAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CreatedBy          string
}

const userColumns = `id::text, username::text, COALESCE(email::text, ''), display_name,
	role, status, password_hash, password_updated_at, must_change_password,
	failed_attempts, locked_until, last_login_at, created_at, updated_at,
	COALESCE(created_by::text, '')`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	var role, status string
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName,
		&role, &status, &u.PasswordHash, &u.PasswordUpdatedAt,
		&u.MustChangePassword, &u.FailedAttempts, &u.LockedUntil,
		&u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt, &u.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Role, u.Status = Role(role), Status(status)
	return &u, nil
}

func mapUserWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		switch {
		case strings.Contains(pgErr.ConstraintName, "username"):
			return ErrUsernameTaken
		case strings.Contains(pgErr.ConstraintName, "email"):
			return ErrEmailTaken
		}
	case "P0001":
		if strings.Contains(pgErr.Message, "last active owner") {
			return ErrLastOwner
		}
	}
	return err
}

// CreateUserInput is a new account. The caller must have already hashed the
// password. MustChangePassword is normally true for operator-created accounts.
type CreateUserInput struct {
	Username           string
	Email              string
	DisplayName        string
	Role               Role
	PasswordHash       string
	MustChangePassword bool
	CreatedBy          string
}

// CreateUser inserts a user. Role must be owner, admin or user. The database
// enforces username/email uniqueness and the last-owner invariant.
func CreateUser(ctx context.Context, q Querier, in CreateUserInput) (*User, error) {
	username := strings.TrimSpace(in.Username)
	if n := utf8.RuneCountInString(username); n < 3 || n > 64 {
		return nil, fmt.Errorf("auth: username must be 3-64 characters")
	}
	if !in.Role.Valid() {
		return nil, ErrInvalidRole
	}
	if in.PasswordHash == "" {
		return nil, errors.New("auth: password hash must not be empty")
	}
	return scanUser(q.QueryRow(ctx, `
		INSERT INTO users (username, email, display_name, role, status,
		                   password_hash, must_change_password, created_by)
		VALUES ($1::citext, NULLIF($2, '')::citext, $3, $4, 'active', $5, $6,
		        NULLIF($7, '')::uuid)
		RETURNING `+userColumns,
		username, in.Email, in.DisplayName, string(in.Role), in.PasswordHash,
		in.MustChangePassword, in.CreatedBy))
}

// UpdateUserInput patches a user. A nil field is unchanged. Email points to ""
// to clear it.
type UpdateUserInput struct {
	DisplayName        *string
	Email              *string
	Role               *Role
	Status             *Status
	MustChangePassword *bool
}

// UpdateUser applies a patch in one transaction. Changing the last active
// owner's role or status returns ErrLastOwner from the database trigger.
// Setting status to disabled also revokes every session and API token in the
// same transaction, so re-enabling the account does not resurrect credentials
// that were live before the disable.
func UpdateUser(ctx context.Context, db DB, id string, in UpdateUserInput) (*User, error) {
	var role, status *string
	if in.Role != nil {
		if !in.Role.Valid() {
			return nil, ErrInvalidRole
		}
		v := string(*in.Role)
		role = &v
	}
	if in.Status != nil {
		if !in.Status.Valid() {
			return nil, ErrInvalidStatus
		}
		v := string(*in.Status)
		status = &v
	}
	emailSet := in.Email != nil
	email := ""
	if emailSet {
		email = *in.Email
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users SET
			display_name = COALESCE($2::text, display_name),
			email = CASE WHEN $3::boolean THEN NULLIF($4::text, '') ELSE email END,
			role = COALESCE($5::text, role),
			status = COALESCE($6::text, status),
			must_change_password = COALESCE($7::boolean, must_change_password),
			updated_at = now()
		WHERE id = $1::uuid
		RETURNING `+userColumns,
		id, in.DisplayName, emailSet, email, role, status, in.MustChangePassword))
	if err != nil {
		return nil, mapUserWriteError(err)
	}
	if in.Status != nil && *in.Status == StatusDisabled {
		if _, err := RevokeAllSessions(ctx, tx, id); err != nil {
			return nil, err
		}
		if _, err := RevokeUserAPITokens(ctx, tx, id, ""); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// DeleteUser removes a user. Deleting the last active owner returns
// ErrLastOwner from the database trigger.
func DeleteUser(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, id)
	if err != nil {
		return mapUserWriteError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// GetUserByID returns a user or ErrUserNotFound.
func GetUserByID(ctx context.Context, q Querier, id string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1::uuid`, id))
}

// GetUserByUsername is case-insensitive when username is citext.
func GetUserByUsername(ctx context.Context, q Querier, username string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE username = $1::citext`,
		strings.TrimSpace(username)))
}

// ListUsers returns users in stable pages of at most 200 (default 50).
func ListUsers(ctx context.Context, q Querier, limit, offset int) ([]*User, error) {
	limit, offset = clampPage(limit, offset)
	rows, err := q.Query(ctx, `SELECT `+userColumns+` FROM users
		ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CountUsers reports the number of accounts; bootstrap uses it as the
// empty-install check.
func CountUsers(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
