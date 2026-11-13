package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
)

// maxUserAgentBytes bounds the stored user agent, which is attacker-controlled.
const maxUserAgentBytes = 512

func registerAdminAuthRoutes(engine *gin.Engine, state *authState) {
	engine.POST(adminLoginPath, handleAdminLogin(state))
	engine.POST(adminLogoutPath, handleAdminLogout(state))
	engine.GET(adminSessionPath, handleAdminSession(state))
	engine.POST(adminPasswordPath, handleAdminChangePassword(state))
}

func handleAdminLogin(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Sign-in is not cookie-authenticated, but a cross-site form post would
		// still plant a session in the victim's browser. Requests that carry an
		// Origin/Sec-Fetch-Site must be same-origin; headerless clients are
		// non-browser callers.
		if !adminSameOrigin(c.Request) {
			adminAPIError(c, http.StatusForbidden, "cross_origin", "cross-origin sign-in rejected")
			return
		}
		// A password in a plaintext request is worse than a bearer token: it is
		// reusable and it mints a long-lived session. Apply the same transport
		// rule the file-token login has always enforced.
		if !state.cookieTransportAllowed(c.Request) {
			adminAPIError(c, http.StatusForbidden, "https_required", "sign-in requires HTTPS outside loopback")
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with username and password")
			return
		}
		username := strings.TrimSpace(req.Username)
		if username == "" || req.Password == "" {
			adminFieldError(c, http.StatusBadRequest, "username", "username and password are required")
			return
		}
		// Invalid account names still execute the normal dummy-password and
		// lockout path, but never persist an attacker-controlled megabyte-scale
		// value. "-" cannot name a user because usernames require 3-64 runes.
		if n := utf8.RuneCountInString(username); n < 3 || n > 64 {
			username = "-"
		}

		ctx := c.Request.Context()
		now := time.Now()
		user, needsRehash, err := auth.Login(ctx, state.pool, username, req.Password, state.clientIPs.requestIP(c.Request), now, state.login)
		switch {
		case errors.Is(err, auth.ErrAccountLocked), errors.Is(err, auth.ErrIPLocked):
			adminAPIError(c, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-in attempts; try again later")
			return
		case errors.Is(err, auth.ErrInvalidCredentials):
			adminAPIError(c, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
			return
		case err != nil:
			adminAPIError(c, http.StatusInternalServerError, "login_unavailable", "sign-in is temporarily unavailable")
			return
		}

		// The verified password is in hand, so a hash written under stale Argon2
		// parameters or a previous pepper is upgraded now. The compare-and-swap
		// prevents a concurrent password reset from being overwritten.
		if needsRehash {
			adminRehashPassword(ctx, state, user.ID, req.Password, user.PasswordHash)
		}
		csrf, err := auth.NewCSRFToken()
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not create the admin session")
			return
		}

		// Rotate the previous session and create its replacement atomically. A
		// failed revocation must never leave a stolen superseded token active.
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not create the admin session")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
		if previous, cookieErr := c.Cookie(auth.SessionCookieName); cookieErr == nil && previous != "" {
			if err := auth.RevokeSession(ctx, tx, previous); err != nil {
				adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not rotate the admin session")
				return
			}
		}
		raw, _, err := auth.CreateSession(ctx, tx, auth.CreateSessionInput{
			UserID:    user.ID,
			IP:        state.clientIPs.requestIP(c.Request),
			UserAgent: adminUserAgent(c.Request),
			Now:       now,
		}, state.sessions)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not create the admin session")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not create the admin session")
			return
		}

		secure := adminTLSRequest(c.Request)
		http.SetCookie(c.Writer, auth.SessionCookie(raw, secure, state.sessions))
		http.SetCookie(c.Writer, auth.CSRFCookie(csrf, secure, state.sessions.AbsoluteTimeout))
		c.JSON(http.StatusOK, gin.H{"user": adminUserJSON(user), "csrf_token": csrf})
	}
}

func handleAdminLogout(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		if raw, err := c.Cookie(auth.SessionCookieName); err == nil && raw != "" {
			if err := auth.RevokeSession(c.Request.Context(), state.pool, raw); err != nil {
				adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not revoke the admin session")
				return
			}
		}
		adminClearCredentials(c)
		c.Status(http.StatusNoContent)
	}
}

func handleAdminSession(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		user, err := auth.GetUserByID(c.Request.Context(), state.pool, identity.UserID)
		if err != nil {
			// The account disappeared between validation and this read: the
			// session is no longer usable, so stop pretending it is.
			adminClearCredentials(c)
			adminAPIError(c, http.StatusUnauthorized, "authentication_required", "sign in to the admin console")
			return
		}
		csrf, err := auth.NewCSRFToken()
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "session_unavailable", "could not refresh the admin session")
			return
		}
		http.SetCookie(c.Writer, auth.CSRFCookie(csrf, adminTLSRequest(c.Request), state.sessions.AbsoluteTimeout))
		c.JSON(http.StatusOK, gin.H{"user": adminUserJSON(user), "csrf_token": csrf})
	}
}

func handleAdminChangePassword(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with current_password and new_password")
			return
		}
		if req.CurrentPassword == "" {
			adminFieldError(c, http.StatusBadRequest, "current_password", "current password is required")
			return
		}
		err := auth.ChangePassword(c.Request.Context(), state.pool, identity.UserID,
			identity.SessionToken, req.CurrentPassword, req.NewPassword, state.password, adminAuditActor(identity))
		switch {
		case err == nil:
			c.Status(http.StatusNoContent)
		case errors.Is(err, auth.ErrInvalidCredentials):
			adminFieldError(c, http.StatusBadRequest, "current_password", "current password is incorrect")
		case adminPasswordPolicyError(err):
			adminFieldError(c, http.StatusBadRequest, "new_password", err.Error())
		case errors.Is(err, auth.ErrUserNotFound):
			adminNotFoundResponse(c)
		default:
			adminAPIError(c, http.StatusInternalServerError, "password_unavailable", "could not change the password")
		}
	}
}

func adminPasswordPolicyError(err error) bool {
	return errors.Is(err, auth.ErrPasswordTooShort) || errors.Is(err, auth.ErrPasswordTooLong) ||
		errors.Is(err, auth.ErrPasswordCommon) || errors.Is(err, auth.ErrPasswordContainsIdentity)
}

// adminClearCredentials expires the session and CSRF cookies in one response.
func adminClearCredentials(c *gin.Context) {
	secure := adminTLSRequest(c.Request)
	http.SetCookie(c.Writer, auth.ClearSessionCookie(secure))
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     auth.CSRFCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func adminUserAgent(r *http.Request) string {
	agent := r.UserAgent()
	if len(agent) > maxUserAgentBytes {
		// The cut can land inside a multi-byte rune, and PostgreSQL rejects
		// invalid UTF-8 in a text column.
		agent = strings.ToValidUTF8(agent[:maxUserAgentBytes], "")
	}
	return agent
}

// adminRehashPassword rewrites the stored hash when the login reported it as
// stale. Failures are ignored: the caller is already authenticated and a stale
// hash still verifies.
func adminRehashPassword(ctx context.Context, state *authState, userID, password, expectedHash string) {
	hash, err := auth.HashPassword(password, state.login.Params, state.login.Pepper)
	if err != nil {
		return
	}
	//nolint:errcheck // best-effort upgrade; a concurrent password change wins
	_, _ = state.pool.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1::uuid AND password_hash = $3`, userID, hash, expectedHash)
}
