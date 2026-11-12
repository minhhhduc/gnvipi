package auth

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// BootstrapConfig controls hashing during owner creation and recovery.
type BootstrapConfig struct {
	Params Params
	Pepper []byte
}

func (c BootstrapConfig) normalized() BootstrapConfig {
	if c.Params.Memory == 0 {
		c.Params = DefaultParams()
	}
	return c
}

// CreateOwnerInput is the first-owner bootstrap request. The password is
// supplied by the caller (for example an interactive CLI prompt); there is no
// default password.
type CreateOwnerInput struct {
	Username    string
	Email       string
	DisplayName string
	Password    string
}

// CreateFirstOwner creates the first owner only when users is empty. The
// advisory lock makes concurrent bootstraps serialize; the loser sees the
// committed user and returns ErrUsersExist. The user and audit row commit in
// one transaction.
func CreateFirstOwner(ctx context.Context, db DB, in CreateOwnerInput, cfg BootstrapConfig) (*User, error) {
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

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort cleanup/best-effort parse

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
		Role:         RoleOwner,
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
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// ResetPassword sets a temporary password, forces a change on next login,
// revokes every session and every API token, and audits the action. The new
// password is never returned or written to audit.
func ResetPassword(ctx context.Context, db DB, userID, newPassword string, cfg BootstrapConfig) error {
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
	if err := WriteAudit(ctx, tx, AuditEntry{
		ActorType:  "system",
		Action:     "user.password_reset",
		TargetType: "user",
		TargetID:   userID,
		UserID:     userID,
		Details: map[string]any{
			"revoked_sessions": sessions,
			"revoked_tokens":   tokens,
			"must_change":      true,
		},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ChangePassword verifies the current password, stores a new hash, clears
// must_change_password, revokes all other sessions and audits the change. The
// current session (currentSessionToken) stays valid. Passwords are never
// audited.
func ChangePassword(ctx context.Context, db DB, userID, currentSessionToken, oldPassword, newPassword string, cfg BootstrapConfig) error {
	cfg = cfg.normalized()
	u, err := GetUserByID(ctx, db, userID)
	if err != nil {
		return err
	}
	ok, _, err := VerifyPassword(oldPassword, u.PasswordHash, cfg.Params, cfg.Pepper)
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
		WHERE id = $1::uuid`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	revoked, err := RevokeUserSessionsExcept(ctx, tx, userID, currentSessionToken)
	if err != nil {
		return err
	}
	if err := WriteAudit(ctx, tx, AuditEntry{
		ActorType:  "user",
		ActorID:    userID,
		Action:     "user.password_change",
		TargetType: "user",
		TargetID:   userID,
		UserID:     userID,
		Details:    map[string]any{"revoked_sessions": revoked},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
