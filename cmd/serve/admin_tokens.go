package main

import (
	"errors"
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
	}
}

// adminTokenTarget resolves whose tokens a request addresses. Only an owner may
// name another account; anyone else naming one is answered 404 so the id space
// stays unprobeable.
func adminTokenTarget(c *gin.Context, identity *adminIdentity) (string, bool) {
	target := strings.TrimSpace(c.Query("user_id"))
	if target == "" || target == identity.UserID {
		return identity.UserID, true
	}
	if identity.Role != auth.RoleOwner {
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
		target := identity.UserID
		if raw := strings.TrimSpace(req.UserID); raw != "" && raw != identity.UserID {
			if identity.Role != auth.RoleOwner {
				adminNotFoundResponse(c)
				return
			}
			target = raw
		}
		kind := auth.TokenKindGateway
		if raw := strings.TrimSpace(req.Kind); raw != "" {
			kind = auth.TokenKind(raw)
		}
		if !kind.Valid() {
			adminFieldError(c, http.StatusBadRequest, "kind", "kind must be gateway or admin")
			return
		}
		if identity.Role == auth.RoleUser && kind != auth.TokenKindGateway {
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

		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "create_unavailable", "could not create the token")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
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
			adminAPIError(c, http.StatusBadRequest, "invalid_token", err.Error())
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "token.create", "token", token.ID, map[string]any{
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
		if identity.Role != auth.RoleOwner && token.UserID != identity.UserID {
			adminNotFoundResponse(c)
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke the token")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := auth.RevokeAPIToken(ctx, tx, token.ID); err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "revoke_unavailable", "could not revoke the token")
			return
		}
		if err := writeAdminAudit(ctx, tx, identity, "token.revoke", "token", token.ID, map[string]any{
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
		c.Status(http.StatusNoContent)
	}
}