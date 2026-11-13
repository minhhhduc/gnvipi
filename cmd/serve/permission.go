package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/auth"
)

// Admin API paths that stay reachable while must_change_password is set.
const (
	adminLoginPath    = "/admin/api/auth/login"
	adminLogoutPath   = "/admin/api/auth/logout"
	adminSessionPath  = "/admin/api/auth/session"
	adminPasswordPath = "/admin/api/auth/password"
)

// pepperEnv supplies the optional peppered-password secret. Rotating it
// invalidates every stored hash, so it is read once per process.
const pepperEnv = "GNVIPI_PASSWORD_PEPPER"

// authState is the database-backed identity configuration shared by every
// /admin/api handler. A nil *authState means file-backed mode: the admin token
// gate applies and no database-backed admin route is registered.
type authState struct {
	pool     *pgxpool.Pool
	sessions auth.SessionConfig
	login    auth.LoginConfig
	password auth.BootstrapConfig
	// loopbackBind records whether the admin listener is bound to a loopback
	// address. Plaintext cookies are only accepted on such a bind, matching the
	// file-token policy in adminAccess.
	loopbackBind bool
}

func newAuthState(pool *pgxpool.Pool) *authState {
	pepper := []byte(strings.TrimSpace(os.Getenv(pepperEnv)))
	return &authState{
		pool:     pool,
		sessions: auth.DefaultSessionConfig(),
		login: auth.LoginConfig{
			Params:  auth.DefaultParams(),
			Pepper:  pepper,
			Lockout: auth.DefaultLockoutConfig(),
		},
		password: auth.BootstrapConfig{Params: auth.DefaultParams(), Pepper: pepper},
	}
}

// ownership says how far an allowed role may reach: any subject, or only the
// authenticated identity itself.
type ownership uint8

const (
	ownAny ownership = iota
	ownSelf
)

type permissionSet map[auth.Role]ownership

func allowed(roles ...auth.Role) permissionSet {
	set := make(permissionSet, len(roles))
	for _, role := range roles {
		set[role] = ownAny
	}
	return set
}

// selfOrOwner lets a role reach itself and an owner reach everyone.
func selfOrOwner(roles ...auth.Role) permissionSet {
	set := make(permissionSet, len(roles)+1)
	set[auth.RoleOwner] = ownAny
	for _, role := range roles {
		set[role] = ownSelf
	}
	return set
}

// adminPermission is one declared route. The table below is the single source
// of truth for /admin/api authorization; a route without an entry fails closed.
type adminPermission struct {
	roles permissionSet
	// anonymous routes authenticate nobody (sign-in) and never run the CSRF,
	// role or scope checks that assume an identity.
	anonymous bool
	// subjectParam marks routes whose :id path parameter is a user id, so a
	// self-only role is confined to its own id and cross-user ids become 404.
	subjectParam bool
}

// tokenScope is the admin-token scope a route needs: read for safe methods,
// write for everything else.
func (p adminPermission) tokenScope(method string) auth.Scope {
	if method == http.MethodGet || method == http.MethodHead {
		return auth.ScopeRead
	}
	return auth.ScopeWrite
}

// adminRoutePermissions declares every /admin/api route. Keys are
// "<METHOD> <gin route pattern>", exactly what gin reports through FullPath, so
// the trailing ":id"/"*id" forms must match the registration strings.
var adminRoutePermissions = map[string]adminPermission{
	// Sign-in surface. The design spec names these /admin/api/me and
	// /admin/api/me/password; the admin API keeps one auth prefix instead, with
	// the same semantics (self-only identity and password change).
	"POST " + adminLoginPath:    {anonymous: true},
	"POST " + adminLogoutPath:   {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},
	"GET " + adminSessionPath:   {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},
	"POST " + adminPasswordPath: {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},

	// Users: operators manage accounts, everyone may read themselves.
	"GET /admin/api/users":               {roles: allowed(auth.RoleOwner)},
	"POST /admin/api/users":              {roles: allowed(auth.RoleOwner)},
	"GET /admin/api/users/:id":           {roles: selfOrOwner(auth.RoleAdmin, auth.RoleUser), subjectParam: true},
	"PUT /admin/api/users/:id":           {roles: allowed(auth.RoleOwner), subjectParam: true},
	"DELETE /admin/api/users/:id":        {roles: allowed(auth.RoleOwner), subjectParam: true},
	"POST /admin/api/users/:id/password": {roles: selfOrOwner(auth.RoleAdmin, auth.RoleUser), subjectParam: true},
	"POST /admin/api/users/:id/revoke":   {roles: selfOrOwner(auth.RoleAdmin, auth.RoleUser), subjectParam: true},

	// Personal API tokens. Kind limits are enforced by the handlers: a user
	// only ever holds gateway tokens, owners and admins hold both.
	"GET /admin/api/tokens":        {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},
	"POST /admin/api/tokens":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},
	"DELETE /admin/api/tokens/:id": {roles: allowed(auth.RoleOwner, auth.RoleAdmin, auth.RoleUser)},

	// Audit trail is owner-only: it discloses every subject id.
	"GET /admin/api/audit": {roles: allowed(auth.RoleOwner)},

	// Legacy configuration surface. Stats attribution per user arrives with the
	// key-aware routing work, so it stays operator-only rather than leaking
	// every caller's usage to a user-role account.
	"GET /admin/api/providers":           {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/providers":          {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/providers/test":     {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/providers/:id":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"PUT /admin/api/providers/:id":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"DELETE /admin/api/providers/:id":    {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/providers/:id/test": {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/stats":               {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/models":              {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/models":             {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"DELETE /admin/api/models/*id":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/models/*id":         {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/config/automode":     {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/config/automode":    {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/config/cycle":        {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/config/cycle":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/config/search":       {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/config/search":      {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"GET /admin/api/chromes":             {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
	"POST /admin/api/chromes":            {roles: allowed(auth.RoleOwner, auth.RoleAdmin)},
}

const adminIdentityKey = "gnvipi.admin.identity"

// adminIdentity is the resolved caller. Session and API-token callers share it;
// viaToken marks the ones exempt from CSRF and subject to token scope.
type adminIdentity struct {
	UserID             string
	Username           string
	Role               auth.Role
	MustChangePassword bool
	ViaToken           bool
	TokenID            string
	Scope              auth.Scope
	SessionToken       string
}

// adminAuthFailure is a complete error response. Callers turn it into JSON so
// every rejection on the admin API keeps the {error, code} envelope.
type adminAuthFailure struct {
	status  int
	code    string
	message string
}

func adminAPIPath(path string) bool {
	return path == "/admin/api" || strings.HasPrefix(path, "/admin/api/")
}

// adminAPIAuthorization guards every /admin/api route with the declared
// permission. It is installed on the engine rather than per route so a route
// registered without a declaration is rejected instead of silently inheriting
// the admin-token gate.
func adminAPIAuthorization(state *authState) gin.HandlerFunc {
	if state == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if !adminAPIPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		c.Header("Cache-Control", "no-store")

		perm, declared := adminRoutePermissions[c.Request.Method+" "+c.FullPath()]
		if !declared {
			adminUndeclaredRoute(c)
			return
		}

		identity, failure := state.identify(c)
		if failure != nil && !perm.anonymous {
			adminFailureResponse(c, failure)
			return
		}
		if !perm.anonymous {
			if identity == nil {
				adminFailureResponse(c, &adminAuthFailure{
					status:  http.StatusUnauthorized,
					code:    "authentication_required",
					message: "sign in to the admin console",
				})
				return
			}
			if identity.MustChangePassword && !adminPasswordChangeAllowed(c, identity) {
				adminAPIError(c, http.StatusForbidden, "password_change_required", "change the admin password before using the console")
				return
			}
			if !identity.ViaToken {
				if failure := verifyAdminCookieWrite(c); failure != nil {
					adminFailureResponse(c, failure)
					return
				}
			}
			if identity.ViaToken {
				if err := auth.RequireScope(identity.Scope, perm.tokenScope(c.Request.Method)); err != nil {
					adminAPIError(c, http.StatusForbidden, "insufficient_scope", "this admin token does not permit the action")
					return
				}
			}
			reach, ok := perm.roles[identity.Role]
			if !ok {
				adminAPIError(c, http.StatusForbidden, "forbidden", "this action is not permitted for your role")
				return
			}
			if reach == ownSelf && perm.subjectParam && c.Param("id") != identity.UserID {
				adminNotFoundResponse(c)
				return
			}
		}
		if identity != nil {
			c.Set(adminIdentityKey, identity)
		}
		c.Next()
	}
}

// cookieTransportAllowed mirrors the file-token policy: a browser session is
// issued and accepted over TLS, or over plaintext only on a loopback bind whose
// request authority is also loopback. Bearer tokens are unaffected.
func (s *authState) cookieTransportAllowed(r *http.Request) bool {
	return adminTLSRequest(r) || (s.loopbackBind && adminLoopbackRequest(r))
}

// identify resolves the caller from the session cookie, then from an admin API
// token. A stale or invalid session is not an error on its own: the request is
// still allowed to authenticate with a bearer token.
func (s *authState) identify(c *gin.Context) (*adminIdentity, *adminAuthFailure) {
	ctx := c.Request.Context()
	now := time.Now()

	// An explicit Authorization header selects bearer authentication. Never
	// silently fall back to a cookie when the caller supplied malformed or
	// invalid bearer credentials, and do not make bearer requests satisfy CSRF
	// merely because a browser also attached a session cookie.
	raw, present, wellFormed := bearerCredential(c.Request)
	if present {
		if !wellFormed {
			return nil, &adminAuthFailure{
				status:  http.StatusUnauthorized,
				code:    "authentication_required",
				message: "sign in to the admin console",
			}
		}
		identity, err := auth.ValidateAPIToken(ctx, s.pool, raw, auth.TokenKindAdmin, now)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidToken) {
				return nil, &adminAuthFailure{
					status:  http.StatusUnauthorized,
					code:    "invalid_credentials",
					message: "invalid or expired admin token",
				}
			}
			return nil, &adminAuthFailure{
				status:  http.StatusInternalServerError,
				code:    "token_unavailable",
				message: "could not validate the admin token",
			}
		}
		return &adminIdentity{
			UserID:             identity.UserID,
			Username:           identity.Username,
			Role:               identity.Role,
			MustChangePassword: identity.MustChangePassword,
			ViaToken:           true,
			TokenID:            identity.TokenID,
			Scope:              identity.Scope,
		}, nil
	}

	if raw, err := c.Cookie(auth.SessionCookieName); err == nil && raw != "" {
		if !s.cookieTransportAllowed(c.Request) {
			return nil, &adminAuthFailure{
				status:  http.StatusForbidden,
				code:    "https_required",
				message: "admin sessions require HTTPS outside loopback",
			}
		}
		session, err := auth.ValidateSession(ctx, s.pool, raw, now, s.sessions)
		switch {
		case err == nil:
			return &adminIdentity{
				UserID:             session.UserID,
				Username:           session.Username,
				Role:               session.Role,
				MustChangePassword: session.MustChangePassword,
				SessionToken:       raw,
			}, nil
		case errors.Is(err, auth.ErrInvalidSession):
			// Fall through to the generic authentication-required response.
		default:
			return nil, &adminAuthFailure{
				status:  http.StatusInternalServerError,
				code:    "session_unavailable",
				message: "could not validate the admin session",
			}
		}
	}

	return nil, &adminAuthFailure{
		status:  http.StatusUnauthorized,
		code:    "authentication_required",
		message: "sign in to the admin console",
	}
}

// verifyAdminCookieWrite enforces the double-submit token and the same-origin
// rule for state-changing requests authenticated by the session cookie.
func verifyAdminCookieWrite(c *gin.Context) *adminAuthFailure {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	cookie, _ := c.Cookie(auth.CSRFCookieName) //nolint:errcheck // a missing cookie is simply an empty CSRF token
	if !auth.ValidDoubleSubmit(cookie, c.GetHeader(auth.CSRFHeaderName)) {
		return &adminAuthFailure{
			status:  http.StatusForbidden,
			code:    "csrf_failed",
			message: "invalid or missing CSRF token",
		}
	}
	origin, ok := adminRequestOrigin(c.Request)
	if !ok || !auth.CheckOrigin(c.Request, origin) {
		return &adminAuthFailure{
			status:  http.StatusForbidden,
			code:    "cross_origin",
			message: "cross-origin admin request rejected",
		}
	}
	return nil
}

func adminPasswordChangeAllowed(c *gin.Context, identity *adminIdentity) bool {
	if c.Request.Method == http.MethodGet && c.Request.URL.Path == adminSessionPath {
		return true
	}
	if c.Request.Method != http.MethodPost {
		return false
	}
	if c.Request.URL.Path == adminPasswordPath || c.Request.URL.Path == adminLogoutPath {
		return true
	}
	return c.FullPath() == "/admin/api/users/:id/password" && c.Param("id") == identity.UserID
}

func bearerCredential(r *http.Request) (raw string, present, wellFormed bool) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", false, false
	}
	if len(values) != 1 {
		return "", true, false
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", true, false
	}
	return fields[1], true, true
}

// adminRequestIP is the transport peer address. Forwarded headers stay unused:
// the gateway has no trusted proxy list, and a spoofable lockout key is worse
// than no key.
func adminRequestIP(r *http.Request) string {
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

// adminRequestOrigin rebuilds this listener's own origin so the CSRF origin
// check compares the request against the same scheme and host it arrived on.
func adminRequestOrigin(r *http.Request) (string, bool) {
	scheme := "http"
	if adminTLSRequest(r) {
		scheme = "https"
	}
	return adminOrigin(scheme + "://" + r.Host)
}

func adminIdentityOf(c *gin.Context) *adminIdentity {
	value, ok := c.Get(adminIdentityKey)
	if !ok {
		return nil
	}
	identity, _ := value.(*adminIdentity)
	return identity
}

func adminAPIError(c *gin.Context, status int, code, message string) {
	body := gin.H{"error": message}
	if code != "" {
		body["code"] = code
	}
	c.AbortWithStatusJSON(status, body)
}

func adminFailureResponse(c *gin.Context, failure *adminAuthFailure) {
	adminAPIError(c, failure.status, failure.code, failure.message)
}

// adminNotFoundResponse hides cross-subject access: the caller learns nothing
// about whether the id exists when it may not reach it.
func adminNotFoundResponse(c *gin.Context) {
	adminAPIError(c, http.StatusNotFound, "not_found", "not found")
}

// adminUndeclaredRoute fails closed. Tests see 500 so a forgotten declaration
// is a loud failure; a deployed binary answers 403 rather than exposing a route
// that skipped authorization.
func adminUndeclaredRoute(c *gin.Context) {
	if gin.Mode() == gin.TestMode {
		adminAPIError(c, http.StatusInternalServerError, "undeclared_route", "admin route has no permission declaration")
		return
	}
	adminAPIError(c, http.StatusForbidden, "forbidden", "admin route is not available")
}

// adminFieldError keeps field-level validation in the established envelope.
func adminFieldError(c *gin.Context, status int, field, problem string) {
	c.JSON(status, gin.H{
		"error":  problem,
		"field":  field,
		"fields": map[string]string{field: problem},
	})
}

// adminPage parses limit/offset list paging (default 50, maximum 200). It
// answers the request itself and returns ok=false on invalid input.
func adminPage(c *gin.Context) (limit, offset int, ok bool) {
	limit = 50
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			adminFieldError(c, http.StatusBadRequest, "limit", "limit must be a positive integer")
			return 0, 0, false
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			adminFieldError(c, http.StatusBadRequest, "offset", "offset must be zero or a positive integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// writeAdminAudit records one mutation. It accepts the pool or the caller's
// transaction so account and token writes stay auditable.
// adminAuditActor names the human behind a request: the token as the actor type
// when the call authenticated with one, the user otherwise.
func adminAuditActor(identity *adminIdentity) auth.AuditEntry {
	entry := auth.AuditEntry{ActorType: "user", ActorID: identity.UserID}
	if identity.ViaToken {
		entry.ActorType, entry.ActorID = "token", identity.TokenID
	}
	return entry
}

func writeAdminAudit(ctx context.Context, q auth.Querier, identity *adminIdentity, action, targetType, targetID, userID string, details map[string]any) error {
	entry := adminAuditActor(identity)
	entry.Action, entry.TargetType, entry.TargetID = action, targetType, targetID
	entry.UserID, entry.Details = userID, details
	return auth.WriteAudit(ctx, q, entry)
}
