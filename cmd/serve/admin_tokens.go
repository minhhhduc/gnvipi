package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
)

func registerAdminTokenRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/tokens", handleAdminListTokens(state))
	engine.POST("/admin/api/tokens", handleAdminCreateToken(state))
	engine.DELETE("/admin/api/tokens/:id", handleAdminRevokeToken(state))
	engine.POST("/admin/api/tokens/:id/reveal/request", handleAdminTokenRevealRequest(state))
	engine.POST("/admin/api/tokens/:id/reveal", handleAdminTokenReveal(state))
}

func adminTokenJSON(token *auth.APIToken) gin.H {
	return gin.H{
		"id":           token.ID,
		"user_id":      token.UserID,
		"kind":         token.Kind,
		"name":         token.Name,
		"token_prefix": token.TokenPrefix,
		"scope":        token.Scope,
		"expires_at":   token.ExpiresAt,
		"last_used_at": token.LastUsedAt,
		"revoked_at":   token.RevokedAt,
		"created_at":   token.CreatedAt,
		"can_reveal":   token.CanReveal,
	}
}

// adminTokenTarget resolves whose tokens a request addresses. Only a superuser may
// name another account; anyone else naming one is answered 404 so the id space
// stays unprobeable.
func adminTokenTarget(c *gin.Context, identity *adminIdentity) (string, bool) {
	target := strings.TrimSpace(c.Query("user_id"))
	if target == "" || sameUserID(target, identity.UserID) {
		return identity.UserID, true
	}
	if !identity.Role.AtLeast(auth.RoleSuperuser) {
		adminNotFoundResponse(c)
		return "", false
	}
	return target, true
}

func handleAdminListTokens(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset, ok := adminPage(c)
		if !ok {
			return
		}
		identity := adminIdentityOf(c)
		target, ok := adminTokenTarget(c, identity)
		if !ok {
			return
		}
		kind := auth.TokenKind(strings.TrimSpace(c.Query("kind")))
		if kind != "" && !kind.Valid() {
			adminFieldError(c, http.StatusBadRequest, "kind", "kind must be gateway or admin")
			return
		}
		// A user-role account never holds admin tokens: refuse the query instead
		// of returning an always-empty list that implies otherwise.
		if identity.Role == auth.RoleUser && kind == auth.TokenKindAdmin {
			adminAPIError(c, http.StatusForbidden, "forbidden", "user accounts hold gateway tokens only")
			return
		}
		tokens, err := auth.ListAPITokens(c.Request.Context(), state.pool, target, kind, limit, offset)
		if err != nil {
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "list_unavailable", "could not list tokens")
			return
		}
		out := make([]gin.H, 0, len(tokens))
		for _, token := range tokens {
			out = append(out, adminTokenJSON(token))
		}
		c.JSON(http.StatusOK, gin.H{"tokens": out, "limit": limit, "offset": offset})
	}
}

func handleAdminCreateToken(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Scope     string `json:"scope"`
			UserID    string `json:"user_id"`
			ExpiresAt string `json:"expires_at"`
			NoExpiry  bool   `json:"no_expiry"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with name and kind")
			return
		}
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		target := identity.UserID
		if raw := strings.TrimSpace(req.UserID); raw != "" && !sameUserID(raw, identity.UserID) {
			if !identity.Role.AtLeast(auth.RoleSuperuser) {
				adminNotFoundResponse(c)
				return
			}
			target = raw
		}
		// The target decides which kinds a token may carry, not the caller: an
		// superuser naming a user account must not mint an admin token for it.
		targetUser, err := auth.GetUserByID(ctx, state.pool, target)
		if err != nil {
			adminUserLookupFailure(c, err)
			return
		}
		kind := auth.TokenKindGateway
		if raw := strings.TrimSpace(req.Kind); raw != "" {
			kind = auth.TokenKind(raw)
		}
		if !kind.Valid() {
			adminFieldError(c, http.StatusBadRequest, "kind", "kind must be gateway or admin")
			return
		}
		if targetUser.Role == auth.RoleUser && kind != auth.TokenKindGateway {
			adminAPIError(c, http.StatusForbidden, "forbidden", "user accounts hold gateway tokens only")
			return
		}
		var scope auth.Scope
		if raw := strings.TrimSpace(req.Scope); raw != "" {
			scope = auth.Scope(raw)
			if !scope.Valid() {
				adminFieldError(c, http.StatusBadRequest, "scope", "scope must be read or write")
				return
			}
		}
		if kind == auth.TokenKindAdmin && scope == "" {
			adminFieldError(c, http.StatusBadRequest, "scope", "admin tokens require a read or write scope")
			return
		}
		expiry := auth.TokenExpiry{NoExpiry: req.NoExpiry}
		if raw := strings.TrimSpace(req.ExpiresAt); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				adminFieldError(c, http.StatusBadRequest, "expires_at", "expires_at must be an RFC 3339 timestamp")
				return
			}
			expiry.At = &at
		}

		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the token")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		raw, token, err := auth.CreateAPIToken(ctx, tx, auth.CreateAPITokenInput{
			UserID: target,
			Kind:   kind,
			Name:   strings.TrimSpace(req.Name),
			Scope:  scope,
			Expiry: expiry,
			Now:    time.Now(),
		})
		switch {
		case errors.Is(err, auth.ErrInvalidTokenKind):
			adminFieldError(c, http.StatusBadRequest, "kind", "kind must be gateway or admin")
			return
		case errors.Is(err, auth.ErrInvalidTokenScope):
			adminFieldError(c, http.StatusBadRequest, "scope", "admin tokens require a read or write scope")
			return
		case errors.Is(err, auth.ErrInvalidTokenExpiry):
			adminFieldError(c, http.StatusBadRequest, "expires_at", "expires_at must be in the future and within the allowed window")
			return
		case errors.Is(err, auth.ErrTokenNoExpiryNotAllowed):
			adminFieldError(c, http.StatusBadRequest, "expires_at", "admin tokens require an expiry")
			return
		case err != nil:
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the token")
			return
		}
		if state.keyring != nil {
			enc, nonce, kver, encErr := state.keyring.Encrypt([]byte(raw), token.ID, token.UserID, "api_token")
			if encErr != nil {
				adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not encrypt the token")
				return
			}
			if setErr := auth.SetAPITokenCipher(ctx, tx, token.ID, enc, nonce, kver); setErr != nil {
				adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not store the token")
				return
			}
			token.CanReveal = true
		}
		if err := writeAdminAudit(ctx, tx, identity, "token.create", "token", token.ID, token.UserID, map[string]any{
			"user_id": token.UserID,
			"kind":    string(token.Kind),
			"scope":   string(token.Scope),
			"name":    token.Name,
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the token creation")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the token")
			return
		}
		body := adminTokenJSON(token)
		// The plaintext token is shown exactly once; only its hash is stored.
		body["token"] = raw
		c.JSON(http.StatusCreated, body)
	}
}

func handleAdminRevokeToken(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		token, err := auth.GetAPIToken(ctx, state.pool, c.Param("id"))
		if err != nil {
			if errors.Is(err, auth.ErrNotFound) || isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "token_unavailable", "could not read the token")
			return
		}
		if !identity.Role.AtLeast(auth.RoleSuperuser) && !sameUserID(token.UserID, identity.UserID) {
			adminNotFoundResponse(c)
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke the token")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		if err := auth.RevokeAPIToken(ctx, tx, token.ID); err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke the token")
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "token.revoke", "token", token.ID, token.UserID, map[string]any{
			"user_id": token.UserID,
			"kind":    string(token.Kind),
			"name":    token.Name,
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the token revocation")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke the token")
			return
		}
		// Revocation must not wait for the TTL window.
		gatewayTokenCache.PurgeAll()
		c.Status(http.StatusNoContent)
	}
}

func loadRevealableAPIToken(c *gin.Context, state *authState, identity *adminIdentity) (*auth.APITokenCipher, bool) {
	ctx := c.Request.Context()
	record, err := auth.GetAPITokenCipher(ctx, state.pool, c.Param("id"))
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) || isInvalidUUID(err) {
			adminNotFoundResponse(c)
			return nil, false
		}
		adminAPIError(c, http.StatusInternalServerError, "token_unavailable", "could not read the token")
		return nil, false
	}
	if !identity.Role.AtLeast(auth.RoleSuperuser) && !sameUserID(record.UserID, identity.UserID) {
		adminNotFoundResponse(c)
		return nil, false
	}
	if record.Revoked {
		adminFieldError(c, http.StatusBadRequest, "token", "revoked tokens cannot be revealed")
		return nil, false
	}
	if len(record.Enc) == 0 || len(record.Nonce) == 0 || record.KVer <= 0 || state.keyring == nil {
		adminFieldError(c, http.StatusUnprocessableEntity, "token", "token was created without reveal encryption")
		return nil, false
	}
	return record, true
}

func handleAdminTokenRevealRequest(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		record, ok := loadRevealableAPIToken(c, state, identity)
		if !ok {
			return
		}
		if state.mailer == nil || !state.mailer.Enabled() {
			adminAPIError(c, http.StatusServiceUnavailable, "smtp_unavailable", "SMTP is not configured on this server")
			return
		}
		if state.revealOTP == nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "OTP store unavailable")
			return
		}
		ctx := c.Request.Context()
		recipient, err := auth.GetUserByID(ctx, state.pool, record.UserID)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "lookup token owner failed")
			return
		}
		if strings.TrimSpace(recipient.Email) == "" {
			adminFieldError(c, http.StatusBadRequest, "email", "token owner has no email configured")
			return
		}
		code, err := state.revealOTP.Issue(identity.UserID, "token:"+record.ID, time.Now())
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "generate OTP failed")
			return
		}
		subject := fmt.Sprintf("Verification code to reveal API key %q", record.Name)
		body := fmt.Sprintf("Your one-time verification code to reveal API key %q is: %s\n\nThis code expires in 5 minutes.", record.Name, code)
		if err := state.mailer.Send(ctx, recipient.Email, subject, body); err != nil {
			adminAPIError(c, http.StatusBadGateway, "email_failed", "could not send verification email")
			return
		}
		if err := writeAdminAudit(ctx, state.pool, identity, "token.reveal_otp_sent", "token", record.ID, record.UserID, map[string]any{
			"name": record.Name,
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the reveal request")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"sent":               true,
			"to":                 maskEmail(recipient.Email),
			"expires_in_seconds": int(revealOTPTTL.Seconds()),
		})
	}
}

func handleAdminTokenReveal(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		record, ok := loadRevealableAPIToken(c, state, identity)
		if !ok {
			return
		}
		var req struct {
			OTP string `json:"otp"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with otp")
			return
		}
		if state.revealOTP == nil || !state.revealOTP.Verify(identity.UserID, "token:"+record.ID, req.OTP, time.Now()) {
			adminAPIError(c, http.StatusForbidden, "invalid_otp", "invalid or expired verification code")
			return
		}
		plaintext, err := state.keyring.Decrypt(record.Enc, record.Nonce, record.ID, record.UserID, "api_token", record.KVer)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "credential_unavailable", "stored token material cannot be decrypted")
			return
		}
		if err := writeAdminAudit(c.Request.Context(), state.pool, identity, "token.reveal", "token", record.ID, record.UserID, map[string]any{
			"name": record.Name,
		}); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "audit_unavailable", "could not record the token reveal")
			return
		}
		c.JSON(http.StatusOK, gin.H{"token": string(plaintext)})
	}
}
