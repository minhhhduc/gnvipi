package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const CSRFCookieName = "gw_csrf"

// CSRFHeaderName is the double-submit header required on cookie-authenticated
// writes.
const CSRFHeaderName = "X-CSRF-Token"

// NewCSRFToken returns 32 random bytes as base64url.
func NewCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidDoubleSubmit reports whether the header and cookie carry the same
// non-empty value, compared in constant time.
func ValidDoubleSubmit(cookie, header string) bool {
	if cookie == "" || header == "" || len(cookie) != len(header) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) == 1
}

// CheckOrigin accepts a cookie-authenticated write only when it carries an
// Origin equal to allowedOrigin or, absent Origin, a same-origin
// Sec-Fetch-Site. Requests with neither signal are rejected.
func CheckOrigin(r *http.Request, allowedOrigin string) bool {
	want, ok := normalizeOrigin(allowedOrigin)
	if !ok {
		return false
	}
	origins, hasOrigin := r.Header["Origin"]
	if hasOrigin {
		if len(origins) != 1 {
			return false
		}
		got, ok := normalizeOrigin(origins[0])
		return ok && got == want
	}
	sites, hasSite := r.Header["Sec-Fetch-Site"]
	if !hasSite || len(sites) != 1 {
		return false
	}
	return strings.EqualFold(sites[0], "same-origin")
}

func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		strings.HasSuffix(u.Host, ":") {
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

// CSRFCookie builds the readable gw_csrf double-submit cookie.
func CSRFCookie(token string, secure bool, lifetime time.Duration) *http.Cookie {
	maxAge := int(lifetime.Seconds())
	if maxAge < 0 {
		maxAge = -1
	}
	return &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	}
}
