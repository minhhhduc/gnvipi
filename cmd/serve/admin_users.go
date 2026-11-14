package main

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/minhhhduc/gnvipi/internal/auth"
)

func registerAdminUserRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/users", handleAdminListUsers(state))
	engine.POST("/admin/api/users", handleAdminCreateUser(state))
	engine.GET("/admin/api/users/:id", handleAdminGetUser(state))
	engine.PUT("/admin/api/users/:id", handleAdminUpdateUser(state))
	engine.DELETE("/admin/api/users/:id", handleAdminDeleteUser(state))
	engine.POST("/admin/api/users/:id/password", handleAdminSetUserPassword(state))
	engine.POST("/admin/api/users/:id/revoke", handleAdminRevokeUser(state))
}

// adminUserJSON is the only user representation on the admin API. The password
// hash is deliberately absent: it never leaves the auth package.
func adminUserJSON(u *auth.User) gin.H {
	return gin.H{
		"id":                   u.ID,
		"username":             u.Username,
		"email":                u.Email,
		"display_name":         u.DisplayName,
		"role":                 u.Role,
		"status":               u.Status,
		"must_change_password": u.MustChangePassword,
		"failed_attempts":      u.FailedAttempts,
		"locked_until":         u.LockedUntil,
		"last_login_at":        u.LastLoginAt,
		"created_at":           u.CreatedAt,
		"updated_at":           u.UpdatedAt,
		"created_by":           u.CreatedBy,
	}
}

func handleAdminListUsers(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset, ok := adminPage(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()
		users, err := auth.ListUsers(ctx, state.pool, limit, offset)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "list_unavailable", "could not list users")
			return
		}
		total, err := auth.CountUsers(ctx, state.pool)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "list_unavailable", "could not list users")
			return
		}
		out := make([]gin.H, 0, len(users))
		for _, u := range users {
			out = append(out, adminUserJSON(u))
		}
		c.JSON(http.StatusOK, gin.H{"users": out, "total": total, "limit": limit, "offset": offset})
	}
}

func handleAdminCreateUser(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Username    string `json:"username"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			Password    string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with username, role and password")
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		req.Email = strings.TrimSpace(req.Email)
		req.DisplayName = strings.TrimSpace(req.DisplayName)
		if req.Username == "" {
			adminFieldError(c, http.StatusBadRequest, "username", "username is required")
			return
		}
		if n := utf8.RuneCountInString(req.Username); n < 3 || n > 64 {
			adminFieldError(c, http.StatusBadRequest, "username", "username must be 3-64 characters")
			return
		}
		if utf8.RuneCountInString(req.DisplayName) > 200 {
			adminFieldError(c, http.StatusBadRequest, "display_name", "display_name must be at most 200 characters")
			return
		}
		// The one-time bootstrap token creates the first owner through the same
		// advisory-locked path as the CLI, so a concurrent bootstrap cannot
		// create two owners.
		if identity := adminIdentityOf(c); identity != nil && identity.Bootstrap {
			role := auth.Role(strings.TrimSpace(req.Role))
			if role != "" && role != auth.RoleOwner {
				adminFieldError(c, http.StatusBadRequest, "role", "the bootstrap token may only create the first owner")
				return
			}
			if err := auth.ValidatePassword(req.Password, req.Username, req.Email); err != nil {
				adminFieldError(c, http.StatusBadRequest, "password", err.Error())
				return
			}
			user, err := auth.CreateFirstOwner(c.Request.Context(), state.pool, auth.CreateOwnerInput{
				Username:    req.Username,
				Email:       req.Email,
				DisplayName: req.DisplayName,
				Password:    req.Password,
			}, state.password)
			switch {
			case errors.Is(err, auth.ErrUsersExist):
				adminAPIError(c, http.StatusConflict, "users_exist", "an owner already exists; the bootstrap token is ignored")
			case err != nil:
				adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the owner")
			default:
				c.JSON(http.StatusCreated, adminUserJSON(user))
			}
			return
		}
		role := auth.RoleUser
		if raw := strings.TrimSpace(req.Role); raw != "" {
			role = auth.Role(raw)
		}
		if !role.Valid() {
			adminFieldError(c, http.StatusBadRequest, "role", "role must be owner, admin or user")
			return
		}
		if err := auth.ValidatePassword(req.Password, req.Username, req.Email); err != nil {
			adminFieldError(c, http.StatusBadRequest, "password", err.Error())
			return
		}
		hash, err := auth.HashPassword(req.Password, state.password.Params, state.password.Pepper)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "hash_unavailable", "could not create the user")
			return
		}
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the user")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		user, err := auth.CreateUser(ctx, tx, auth.CreateUserInput{
			Username:           req.Username,
			Email:              req.Email,
			DisplayName:        req.DisplayName,
			Role:               role,
			PasswordHash:       hash,
			MustChangePassword: true,
			CreatedBy:          identity.UserID,
		})
		switch {
		case errors.Is(err, auth.ErrUsernameTaken):
			adminFieldError(c, http.StatusConflict, "username", "username is already taken")
			return
		case errors.Is(err, auth.ErrEmailTaken):
			adminFieldError(c, http.StatusConflict, "email", "email is already taken")
			return
		case err != nil:
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the user")
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "user.create", "user", user.ID, user.ID, map[string]any{
			"username": user.Username,
			"role":     string(user.Role),
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the user creation")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the user")
			return
		}
		c.JSON(http.StatusCreated, adminUserJSON(user))
	}
}

func handleAdminGetUser(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := auth.GetUserByID(c.Request.Context(), state.pool, c.Param("id"))
		if err != nil {
			adminUserLookupFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, adminUserJSON(user))
	}
}

func handleAdminUpdateUser(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			DisplayName        *string `json:"display_name"`
			Email              *string `json:"email"`
			Role               *string `json:"role"`
			Status             *string `json:"status"`
			MustChangePassword *bool   `json:"must_change_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with the fields to update")
			return
		}
		var patch auth.UpdateUserInput
		if req.DisplayName != nil {
			value := strings.TrimSpace(*req.DisplayName)
			patch.DisplayName = &value
		}
		if req.Email != nil {
			value := strings.TrimSpace(*req.Email)
			patch.Email = &value
		}
		if req.Role != nil {
			role := auth.Role(strings.TrimSpace(*req.Role))
			if !role.Valid() {
				adminFieldError(c, http.StatusBadRequest, "role", "role must be owner, admin or user")
				return
			}
			patch.Role = &role
		}
		if req.Status != nil {
			status := auth.Status(strings.TrimSpace(*req.Status))
			if !status.Valid() {
				adminFieldError(c, http.StatusBadRequest, "status", "status must be active or disabled")
				return
			}
			patch.Status = &status
		}
		patch.MustChangePassword = req.MustChangePassword

		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "update_unavailable", "could not update the user")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		user, err := auth.UpdateUserTx(ctx, tx, c.Param("id"), patch)
		switch {
		case errors.Is(err, auth.ErrUserNotFound), isInvalidUUID(err):
			adminNotFoundResponse(c)
			return
		case errors.Is(err, auth.ErrLastOwner):
			adminFieldError(c, http.StatusConflict, "role", "the last active owner cannot be demoted or disabled")
			return
		case errors.Is(err, auth.ErrInvalidRole):
			adminFieldError(c, http.StatusBadRequest, "role", "role must be owner, admin or user")
			return
		case errors.Is(err, auth.ErrInvalidStatus):
			adminFieldError(c, http.StatusBadRequest, "status", "status must be active or disabled")
			return
		case errors.Is(err, auth.ErrUsernameTaken):
			adminFieldError(c, http.StatusConflict, "username", "username is already taken")
			return
		case errors.Is(err, auth.ErrEmailTaken):
			adminFieldError(c, http.StatusConflict, "email", "email is already taken")
			return
		case err != nil:
			adminAPIError(c, http.StatusInternalServerError, "update_unavailable", "could not update the user")
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "user.update", "user", user.ID, user.ID, map[string]any{
			"username": user.Username,
			"role":     string(user.Role),
			"status":   string(user.Status),
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the user update")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "update_unavailable", "could not update the user")
			return
		}
		c.JSON(http.StatusOK, adminUserJSON(user))
	}
}

func handleAdminDeleteUser(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "delete_unavailable", "could not delete the user")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		target, err := auth.GetUserByID(ctx, tx, c.Param("id"))
		if err != nil {
			adminUserLookupFailure(c, err)
			return
		}
		if err := auth.DeleteUser(ctx, tx, target.ID); err != nil {
			switch {
			case errors.Is(err, auth.ErrUserNotFound), isInvalidUUID(err):
				adminNotFoundResponse(c)
			case errors.Is(err, auth.ErrLastOwner):
				adminFieldError(c, http.StatusConflict, "id", "the last active owner cannot be deleted")
			default:
				adminAPIError(c, http.StatusInternalServerError, "delete_unavailable", "could not delete the user")
			}
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "user.delete", "user", target.ID, target.ID, map[string]any{
			"username": target.Username,
			"role":     string(target.Role),
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the user deletion")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "delete_unavailable", "could not delete the user")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// handleAdminSetUserPassword serves both paths: changing your own password
// needs the current secret, while an owner resetting somebody else's password
// issues a temporary one that must be changed at next sign-in.
func handleAdminSetUserPassword(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		targetID := c.Param("id")
		var req struct {
			CurrentPassword string `json:"current_password"`
			Password        string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with password")
			return
		}
		ctx := c.Request.Context()
		if targetID == identity.UserID {
			if req.CurrentPassword == "" {
				adminFieldError(c, http.StatusBadRequest, "current_password", "current password is required")
				return
			}
			err := auth.ChangePassword(ctx, state.pool, targetID, identity.SessionToken, req.CurrentPassword, req.Password, state.password, adminAuditActor(identity))
			switch {
			case err == nil:
				c.Status(http.StatusNoContent)
			case errors.Is(err, auth.ErrInvalidCredentials):
				adminFieldError(c, http.StatusBadRequest, "current_password", "current password is incorrect")
			case adminPasswordPolicyError(err):
				adminFieldError(c, http.StatusBadRequest, "password", err.Error())
			default:
				adminAPIError(c, http.StatusInternalServerError, "password_unavailable", "could not change the password")
			}
			return
		}
		// Owner reset: anonymous password checks are impossible, so validate the
		// new secret against the target account's identity fields first.
		target, err := auth.GetUserByID(ctx, state.pool, targetID)
		if err != nil {
			adminUserLookupFailure(c, err)
			return
		}
		if err := auth.ValidatePassword(req.Password, target.Username, target.Email); err != nil {
			adminFieldError(c, http.StatusBadRequest, "password", err.Error())
			return
		}
		if err := auth.ResetPasswordBy(ctx, state.pool, target.ID, req.Password, state.password, adminAuditActor(identity)); err != nil {
			if errors.Is(err, auth.ErrUserNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "password_unavailable", "could not reset the password")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func handleAdminRevokeUser(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		targetID := c.Param("id")
		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke credentials")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		target, err := auth.GetUserByID(ctx, tx, targetID)
		if err != nil {
			adminUserLookupFailure(c, err)
			return
		}
		sessions, err := auth.RevokeAllSessions(ctx, tx, target.ID)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke credentials")
			return
		}
		tokens, err := auth.RevokeUserAPITokens(ctx, tx, target.ID, "")
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke credentials")
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "user.revoke", "user", target.ID, target.ID, map[string]any{
			"username":         target.Username,
			"revoked_sessions": sessions,
			"revoked_tokens":   tokens,
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the credential revocation")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke credentials")
			return
		}
		if target.ID == identity.UserID {
			adminClearCredentials(c)
		}
		c.JSON(http.StatusOK, gin.H{"revoked_sessions": sessions, "revoked_tokens": tokens})
	}
}

// adminUserLookupFailure maps a failed user lookup. A malformed id and a
// missing account answer identically so ids cannot be probed.
func adminUserLookupFailure(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrUserNotFound) || isInvalidUUID(err) {
		adminNotFoundResponse(c)
		return
	}
	adminAPIError(c, http.StatusInternalServerError, "user_unavailable", "could not read the user")
}

// isInvalidUUID reports a PostgreSQL text->uuid cast failure, which is how a
// malformed path id reaches the driver.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
