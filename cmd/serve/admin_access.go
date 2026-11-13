package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
)

const adminSessionCookie = "gnvipi_admin_session"
const adminSessionLifetime = 12 * time.Hour

// adminAccess protects only /admin and /admin/*, including the login endpoint
// intercepted here. Install it before registering admin routes. With dbAuth the
// JSON API authenticates against the database instead, so this gate leaves
// /admin/api alone and keeps guarding the pages.
func adminAccess(addr, token string, dbAuth bool) gin.HandlerFunc {
	loopback := adminLoopbackBind(addr)
	tokenHash := sha256.Sum256([]byte(token))
	var sessionKey [32]byte
	if token != "" {
		// Restarting the gateway invalidates existing sessions without retaining
		// an unbounded session store or putting the admin token in a cookie.
		sessionKey = sha256.Sum256([]byte(rand.Text()))
	}
	return func(c *gin.Context) {
		r := c.Request
		path := r.URL.Path
		if path != "/admin" && !strings.HasPrefix(path, "/admin/") {
			c.Next()
			return
		}
		if dbAuth && adminAPIPath(path) {
			c.Next()
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !adminSameOrigin(r) {
			adminAccessError(c, http.StatusForbidden, "Admin mutations require the same origin; cross-site requests are not allowed.")
			return
		}
		if token == "" {
			if !loopback {
				adminAccessError(c, http.StatusForbidden, "Set -admin-token or GNVIPI_ADMIN_TOKEN to enable admin access on a non-loopback bind, or bind to localhost, 127.0.0.1, or [::1].")
				return
			}
			// A loopback listener alone does not prevent DNS rebinding. Local
			// open access also requires a loopback request authority.
			if !adminLoopbackRequest(r) {
				adminAccessError(c, http.StatusForbidden, "Open local admin access requires a localhost or loopback IP Host.")
				return
			}
			if path == "/admin/login" {
				c.Redirect(http.StatusSeeOther, "/admin")
				c.Abort()
				return
			}
			c.Next()
			return
		}
		// HTTP cookies are permitted only on an actual loopback bind/authority
		// so local token-protected UI remains usable without setting up TLS.
		// Remote browser sessions require TLS; forwarded headers are not trusted.
		allowPlainCookie := loopback && adminLoopbackRequest(r)
		if path == "/admin/login" {
			if !adminTLSRequest(r) && !allowPlainCookie {
				adminAccessError(c, http.StatusForbidden, "Browser admin login requires HTTPS outside loopback. Use HTTPS or the Authorization: Bearer / X-Admin-Token header for API access.")
				return
			}
			switch r.Method {
			case http.MethodGet:
				adminLoginPage(c, http.StatusOK, "Enter the admin token configured for this gateway.")
			case http.MethodPost:
				r.Body = http.MaxBytesReader(c.Writer, r.Body, 8<<10)
				if err := r.ParseForm(); err != nil || len(r.PostForm["token"]) != 1 {
					adminAccessError(c, http.StatusBadRequest, "Submit one admin token using the login form.")
					return
				}
				if !adminTokenMatches(r.PostForm.Get("token"), tokenHash) {
					if adminHTMLRequest(r) {
						adminLoginPage(c, http.StatusUnauthorized, "Invalid admin token.")
					} else {
						adminAccessError(c, http.StatusUnauthorized, "Invalid admin token.")
					}
					return
				}
				expires := time.Now().Add(adminSessionLifetime)
				payload := strconv.FormatInt(expires.Unix(), 10) + "." + rand.Text()
				http.SetCookie(c.Writer, &http.Cookie{
					Name: adminSessionCookie, Value: payload + "." + adminSessionSignature(payload, sessionKey),
					Path: "/admin", HttpOnly: true, Secure: adminTLSRequest(r),
					SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(adminSessionLifetime.Seconds()),
				})
				c.Redirect(http.StatusSeeOther, "/admin")
				c.Abort()
			default:
				c.Header("Allow", "GET, POST")
				adminAccessError(c, http.StatusMethodNotAllowed, "The admin login endpoint accepts GET and POST.")
			}
			return
		}
		authenticated, supplied := adminHeaderAuthentication(r, tokenHash)
		if !supplied && (adminTLSRequest(r) || allowPlainCookie) {
			authenticated = adminValidSession(r, sessionKey)
		}
		if !authenticated {
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) && adminHTMLRequest(r) {
				c.Redirect(http.StatusSeeOther, "/admin/login")
				c.Abort()
			} else {
				c.Header("WWW-Authenticate", `Bearer realm="gnvipi-admin"`)
				adminAccessError(c, http.StatusUnauthorized, "Admin authentication required. Sign in at /admin/login or supply Authorization: Bearer / X-Admin-Token.")
			}
			return
		}
		c.Next()
	}
}

func adminLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	return err == nil && adminLoopbackHost(host)
}

// portOnly reduces a listen address to ":port" for URLs and status lines, so
// both the loopback default (127.0.0.1:8080) and a wildcard bind (:8080) read
// as http://localhost:8080. The admin access policy is decided separately from
// adminLoopbackBind, using the real bind address.
func portOnly(addr string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return addr
	}
	return ":" + port
}

// logAdminSurface states where the admin console listens on startup. Without a
// token the console is loopback-only, which is the default; a wildcard bind
// without a token stays closed, so say so instead of letting the first browser
// request discover it.
func logAdminSurface(addr, token string) {
	scope := "loopback only"
	switch {
	case token != "":
		scope = "token protected"
	case adminLoopbackBind(addr):
	default:
		scope = "disabled (non-loopback bind needs -admin-token)"
	}
	log.Printf("admin console: http://localhost%s/admin (%s)", portOnly(addr), scope)
	if !adminLoopbackBind(addr) && token == "" {
		log.Printf("warning: -addr %s listens on every interface and -admin-token is empty; /admin requests are rejected with 403", addr)
	}
}

func adminLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func adminLoopbackRequest(r *http.Request) bool {
	u, err := url.Parse("http://" + r.Host)
	return err == nil && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && adminLoopbackHost(u.Hostname())
}

// A wrapped plaintext connection can expose an empty ConnectionState on Go 1.26.
func adminTLSRequest(r *http.Request) bool {
	return r.TLS != nil && r.TLS.HandshakeComplete
}

// Compare full origins, including scheme and effective port. Origin is a
// serialized origin, not a URL with credentials, paths, queries, or fragments.
func adminOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || u.Path != "" || strings.HasSuffix(u.Host, ":") || strings.ContainsAny(raw, "?#") {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if strings.Contains(host, ":") {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), true
}

func adminSameOrigin(r *http.Request) bool {
	if sites, present := r.Header["Sec-Fetch-Site"]; present && (len(sites) != 1 || sites[0] != "same-origin") {
		return false
	}
	scheme := "http"
	if adminTLSRequest(r) {
		scheme = "https"
	}
	target, validTarget := adminOrigin(scheme + "://" + r.Host)
	if !validTarget {
		return false
	}

	origins, hasOrigin := r.Header["Origin"]
	if hasOrigin {
		if len(origins) != 1 {
			return false
		}
		origin, valid := adminOrigin(origins[0])
		if !valid || origin != target {
			return false
		}
	}

	referers, hasReferer := r.Header["Referer"]
	if hasReferer {
		if len(referers) != 1 {
			return false
		}
		refURL, err := url.Parse(referers[0])
		if err != nil || refURL.Scheme == "" || refURL.Host == "" {
			return false
		}
		refOrigin, valid := adminOrigin(refURL.Scheme + "://" + refURL.Host)
		if !valid || refOrigin != target {
			return false
		}
	}

	return true
}

func adminTokenMatches(token string, expected [32]byte) bool {
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
}

func adminHeaderAuthentication(r *http.Request, expected [32]byte) (valid, supplied bool) {
	valid = true
	if values, present := r.Header["Authorization"]; present {
		supplied = true
		if len(values) != 1 {
			return false, true
		}
		fields := strings.Fields(values[0])
		valid = len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && adminTokenMatches(fields[1], expected)
	}
	if values, present := r.Header["X-Admin-Token"]; present {
		supplied = true
		valid = valid && len(values) == 1 && adminTokenMatches(values[0], expected)
	}
	return valid && supplied, supplied
}

func adminSessionSignature(payload string, key [32]byte) string {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func adminValidSession(r *http.Request, key [32]byte) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || len(cookie.Value) > 256 {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || parts[1] == "" {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expires <= time.Now().Unix() {
		return false
	}
	payload := parts[0] + "." + parts[1]
	return hmac.Equal([]byte(parts[2]), []byte(adminSessionSignature(payload, key)))
}

func adminHTMLRequest(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html") || r.Header.Get("Sec-Fetch-Mode") == "navigate" || r.Header.Get("Sec-Fetch-Dest") == "document"
}

func adminAccessError(c *gin.Context, status int, message string) {
	c.Abort()
	if adminHTMLRequest(c.Request) {
		c.Header("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		c.Data(status, "text/html; charset=utf-8", []byte("<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>Admin access</title><h1>Admin access</h1><p>"+html.EscapeString(message)+"</p></html>"))
		return
	}
	if err := gwerr.Write(c.Writer, gwerr.ProtocolOpenAIChat, &gwerr.Error{Status: status, Kind: gwerr.KindFromHTTPStatus(status), Message: message}); err != nil {
		log.Printf("write admin access error: %v", err)
	}
}

func adminLoginPage(c *gin.Context, status int, message string) {
	c.Header("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	c.Data(status, "text/html; charset=utf-8", []byte(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Gateway admin login</title></head>
<body><main><h1>Gateway admin login</h1><p>%s</p>
<form method="post" action="/admin/login">
<label for="token">Admin token</label>
<input id="token" name="token" type="password" autocomplete="current-password" required autofocus>
<button type="submit">Sign in</button>
</form></main></body></html>`, html.EscapeString(message))))
	c.Abort()
}
