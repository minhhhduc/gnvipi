package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// BootstrapConfig controls hashing during superuser creation and recovery.
type BootstrapConfig struct {
	Params            Params
	Pepper            []byte
	PreviousPepper    []byte
	PreviousPepperSet bool
}

func (c BootstrapConfig) normalized() BootstrapConfig {
	if c.Params.Memory == 0 {
		c.Params = DefaultParams()
	}
	return c
}

// CreateSuperuserInput is the first-superuser bootstrap request. The password is
// supplied by the caller (for example an interactive CLI prompt); there is no
// default password.
type CreateSuperuserInput struct {
	Username    string
	Email       string
	DisplayName string
	Password    string
}

// CreateFirstSuperuser creates the first superuser only when users is empty. The
// advisory lock makes concurrent bootstraps serialize; the loser sees the
// committed user and returns ErrUsersExist. The user and audit row commit in
// one transaction.
func CreateFirstSuperuser(ctx context.Context, db DB, in CreateSuperuserInput, cfg BootstrapConfig) (*User, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup/best-effort parse

	u, err := CreateFirstSuperuserTx(ctx, tx, in, cfg)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// CreateFirstSuperuserTx performs the bootstrap inside a caller-owned transaction
// and leaves the commit to the caller, so related rows (for example a gateway
// token) can be written atomically with the superuser. The advisory lock is
// transaction-scoped, so tx must be a real transaction: a connection pool also
// satisfies Querier but would release the lock as soon as the statement ends,
// letting two concurrent bootstraps create two superusers.
func CreateFirstSuperuserTx(ctx context.Context, tx pgx.Tx, in CreateSuperuserInput, cfg BootstrapConfig) (*User, error) {
	cfg = cfg.normalized()
	username := strings.TrimSpace(in.Username)
	if n := utf8.RuneCountInString(username); n < 3 || n > 64 {
		return nil, fmt.Errorf("auth: username must be 3-64 characters")
	}
	if err := ValidatePassword(in.Password, username, in.Email); err != nil {
		return nil, err
	}
	hash, err := HashPassword(in.Password, cfg.Params, cfg.Pepper)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('gnvipi:bootstrap'))`); err != nil {
		return nil, err
	}
	n, err := CountUsers(ctx, tx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrUsersExist
	}
	u, err := CreateUser(ctx, tx, CreateUserInput{
		Username:     username,
		Email:        in.Email,
		DisplayName:  in.DisplayName,
		Role:         RoleSuperuser,
		PasswordHash: hash,
	})
	if err != nil {
		return nil, err
	}
	if err := WriteAudit(ctx, tx, AuditEntry{
		ActorType:  "system",
		Action:     "user.bootstrap",
		TargetType: "user",
		TargetID:   u.ID,
		UserID:     u.ID,
		Details:    map[string]any{"username": u.Username, "role": string(u.Role)},
	}); err != nil {
		return nil, err
	}
	return u, nil
}

// ResetPassword sets a temporary password, forces a change on next login,
// revokes every session and every API token, and audits the action as the
// system. The new password is never returned or written to audit.
func ResetPassword(ctx context.Context, db DB, userID, newPassword string, cfg BootstrapConfig) error {
	return resetPassword(ctx, db, userID, newPassword, cfg, AuditEntry{ActorType: "system"})
}

// ResetPasswordBy performs the same reset on behalf of an authenticated actor,
// so the audit row names who did it instead of the system. actor contributes
// only ActorType and ActorID; the action, target and details are fixed here.
func ResetPasswordBy(ctx context.Context, db DB, userID, newPassword string, cfg BootstrapConfig, actor AuditEntry) error {
	actor.Action = "user.password_reset"
	actor.TargetType = "user"
	actor.TargetID = userID
	actor.UserID = userID
	actor.Details = nil
	return resetPassword(ctx, db, userID, newPassword, cfg, actor)
}

func resetPassword(ctx context.Context, db DB, userID, newPassword string, cfg BootstrapConfig, entry AuditEntry) error {
	cfg = cfg.normalized()
	u, err := GetUserByID(ctx, db, userID)
	if err != nil {
		return err
	}
	if err := ValidatePassword(newPassword, u.Username, u.Email); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword, cfg.Params, cfg.Pepper)
	if err != nil {
		return err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup/best-effort parse

	tag, err := tx.Exec(ctx, `
		UPDATE users SET password_hash = $2, password_updated_at = now(),
		                 must_change_password = true, failed_attempts = 0,
		                 locked_until = NULL, updated_at = now()
		WHERE id = $1::uuid`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	sessions, err := RevokeAllSessions(ctx, tx, userID)
	if err != nil {
		return err
	}
	tokens, err := RevokeUserAPITokens(ctx, tx, userID, "")
	if err != nil {
		return err
	}
	entry.Action = "user.password_reset"
	entry.TargetType = "user"
	entry.TargetID = userID
	entry.UserID = userID
	entry.Details = map[string]any{
		"revoked_sessions": sessions,
		"revoked_tokens":   tokens,
		"must_change":      true,
	}
	if err := WriteAudit(ctx, tx, entry); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ChangePassword verifies the current password, stores a new hash, clears
// must_change_password, revokes all other sessions and audits the change. The
// current session (currentSessionToken) stays valid. Passwords are never
// audited. The password update is a compare-and-swap so a concurrent reset or
// password change cannot be overwritten after this call verified a stale hash.
func ChangePassword(ctx context.Context, db DB, userID, currentSessionToken, oldPassword, newPassword string, cfg BootstrapConfig, actor AuditEntry) error {
	cfg = cfg.normalized()
	u, err := GetUserByID(ctx, db, userID)
	if err != nil {
		return err
	}
	ok, _, err := verifyPasswordWithPrevious(oldPassword, u.PasswordHash, cfg.Params, cfg.Pepper, cfg.PreviousPepper, cfg.PreviousPepperSet)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if err := ValidatePassword(newPassword, u.Username, u.Email); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword, cfg.Params, cfg.Pepper)
	if err != nil {
		return err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup/best-effort parse

	tag, err := tx.Exec(ctx, `
		UPDATE users SET password_hash = $2, password_updated_at = now(),
		                 must_change_password = false, failed_attempts = 0,
		                 locked_until = NULL, updated_at = now()
		WHERE id = $1::uuid AND password_hash = $3`, userID, hash, u.PasswordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidCredentials
	}
	revoked, err := RevokeUserSessionsExcept(ctx, tx, userID, currentSessionToken)
	if err != nil {
		return err
	}
	actor.Action = "user.password_change"
	actor.TargetType = "user"
	actor.TargetID = userID
	actor.UserID = userID
	actor.Details = map[string]any{"revoked_sessions": revoked}
	if err := WriteAudit(ctx, tx, actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RootUsername is the reserved name of the built-in root account. It is created
// by every database-mode start and the database refuses to delete it.
const RootUsername = "gnvipi"

// RootInput is the built-in root account request.
type RootInput struct {
	Username           string
	Email              string
	Password           string
	MustChangePassword bool
}

// EnsureRoot creates the built-in root account when it is missing and returns
// the row with a created flag. An existing account is left untouched apart from
// a role promotion to root, so a password the operator already set survives
// every restart. The advisory lock serializes concurrent starts; the seeded row
// is audited as the system.
//
// The password is not checked against the login policy: the documented default
// is a first-sign-in credential and MustChangePassword forces a real password
// before the console answers anything else.
func EnsureRoot(ctx context.Context, db DB, in RootInput, cfg BootstrapConfig) (*User, bool, error) {
	cfg = cfg.normalized()
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return nil, false, fmt.Errorf("auth: root username must not be empty")
	}
	if in.Password == "" {
		return nil, false, fmt.Errorf("auth: root password must not be empty")
	}
	hash, err := HashPassword(in.Password, cfg.Params, cfg.Pepper)
	if err != nil {
		return nil, false, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup after an aborted write

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('gnvipi:root'))`); err != nil {
		return nil, false, err
	}
	email := strings.TrimSpace(in.Email)
	u, err := GetUserByUsername(ctx, tx, username)
	switch {
	case errors.Is(err, ErrUserNotFound):
		u, err = CreateUser(ctx, tx, CreateUserInput{
			Username:           username,
			Email:              email,
			DisplayName:        "Root",
			Role:               RoleRoot,
			PasswordHash:       hash,
			MustChangePassword: in.MustChangePassword,
		})
		if err != nil {
			return nil, false, err
		}
		if err := writeRootAudit(ctx, tx, "user.root.create", u); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return u, true, nil
	case err != nil:
		return nil, false, err
	case u.Role == RoleRoot:
		if u.Email == "" && email != "" {
			u, err = UpdateUser(ctx, tx, u.ID, UpdateUserInput{Email: &email})
			if err != nil {
				return nil, false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, false, err
			}
		}
		return u, false, nil
	}

	// The reserved name is held by an ordinary account: promote it, keeping the
	// password its operator already knows.
	root := RoleRoot
	u, err = UpdateUserTx(ctx, tx, u.ID, UpdateUserInput{Role: &root})
	if err != nil {
		return nil, false, err
	}
	if err := writeRootAudit(ctx, tx, "user.root.promote", u); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return u, false, nil
}

func writeRootAudit(ctx context.Context, tx Querier, action string, u *User) error {
	return WriteAudit(ctx, tx, AuditEntry{
		ActorType:  "system",
		Action:     action,
		TargetType: "user",
		TargetID:   u.ID,
		UserID:     u.ID,
		Details:    map[string]any{"username": u.Username, "role": string(u.Role)},
	})
}
