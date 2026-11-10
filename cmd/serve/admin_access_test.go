package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// defaultAddr must stay in sync with the -addr flag in run() (cmd/serve/main.go).
// run() parses the global FlagSet and only executes under main(), so the default
// is mirrored here rather than read from a live flag.
const defaultAddr = "127.0.0.1:8080"

func adminAccessTestEngine(addr, token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAccess(addr, token))
	engine.Any("/admin", func(c *gin.Context) { c.String(http.StatusOK, "admin") })
	engine.Any("/admin/providers", func(c *gin.Context) { c.String(http.StatusOK, "changed") })
	engine.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "public") })
	engine.GET("/administrator", func(c *gin.Context) { c.String(http.StatusOK, "public") })
	return engine
}

func adminAccessRequest(t *testing.T, engine *gin.Engine, method, target string, body io.Reader, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = http.NoBody
	}
	req := httptest.NewRequestWithContext(t.Context(), method, target, body)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	return response
}

// A wildcard or LAN bind must not become open because the caller is local.
func TestAdminAccessBindProtection(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want int
	}{
		{"127.0.0.1:8080", http.StatusOK},
		{" 127.0.0.1:8080 ", http.StatusOK},
		{"127.0.0.2:8080", http.StatusOK},
		{"[::1]:8080", http.StatusOK},
		{"localhost:8080", http.StatusOK},
		{"LOCALHOST:8080", http.StatusOK},
		{"[::ffff:127.0.0.1]:8080", http.StatusOK},
		{":8080", http.StatusForbidden},
		{"0.0.0.0:8080", http.StatusForbidden},
		{"[::]:8080", http.StatusForbidden},
		{"192.168.1.10:8080", http.StatusForbidden},
		{"gateway.example:8080", http.StatusForbidden},
		{"", http.StatusForbidden},
		{"not-an-address", http.StatusForbidden},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			response := adminAccessRequest(t, adminAccessTestEngine(tc.addr, ""), http.MethodGet, "http://localhost:8080/admin", nil, nil)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d", response.Code, tc.want)
			}
			if tc.want == http.StatusForbidden && !strings.Contains(response.Body.String(), "GNVIPI_ADMIN_TOKEN") {
				t.Fatalf("missing configuration guidance: %s", response.Body.String())
			}
		})
	}
	engine := adminAccessTestEngine("127.0.0.1:8080", "")
	response := adminAccessRequest(t, engine, http.MethodPost, "http://attacker.example:8080/admin/providers", nil, map[string]string{"Origin": "http://attacker.example:8080"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("loopback access accepted a non-loopback Host: %d", response.Code)
	}
}

// Removing either Origin or Fetch Metadata checks allows browser mutations.
func TestAdminAccessMutationOrigins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		headers map[string]string
		want    int
	}{
		{"headerless API", "http://localhost:8080/admin/providers", nil, http.StatusOK},
		{"same origin", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"default port", "http://localhost/admin/providers", map[string]string{"Origin": "http://LOCALHOST:80"}, http.StatusOK},
		{"IPv6", "http://[::1]:8080/admin/providers", map[string]string{"Origin": "http://[::1]:8080"}, http.StatusOK},
		{"different host", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"different port", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8081"}, http.StatusForbidden},
		{"different scheme", "http://localhost:8080/admin/providers", map[string]string{"Origin": "https://localhost:8080"}, http.StatusForbidden},
		{"opaque origin", "http://localhost:8080/admin/providers", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"malformed origin", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://["}, http.StatusForbidden},
		{"empty explicit port", "http://localhost/admin/providers", map[string]string{"Origin": "http://localhost:"}, http.StatusForbidden},
		{"empty origin", "http://localhost:8080/admin/providers", map[string]string{"Origin": ""}, http.StatusForbidden},
		{"origin path", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080/admin"}, http.StatusForbidden},
		{"origin userinfo", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://user@localhost:8080"}, http.StatusForbidden},
		{"origin query", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080?"}, http.StatusForbidden},
		{"origin fragment", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080#fragment"}, http.StatusForbidden},
		{"origin list", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080 http://evil.example"}, http.StatusForbidden},
		{"cross-site metadata", "http://localhost:8080/admin/providers", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site is not same-origin", "http://localhost:8080/admin/providers", map[string]string{"Origin": "http://localhost:8080", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"unknown metadata", "http://localhost:8080/admin/providers", map[string]string{"Sec-Fetch-Site": "invalid"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := adminAccessTestEngine("localhost:8080", "")
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				response := adminAccessRequest(t, engine, method, tc.target, nil, tc.headers)
				if response.Code != tc.want {
					t.Fatalf("%s status = %d, want %d", method, response.Code, tc.want)
				}
			}
		})
	}
	engine := adminAccessTestEngine("localhost:8080", "")
	for _, name := range []string{"Origin", "Sec-Fetch-Site"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080/admin/providers", http.NoBody)
		req.Header.Add(name, map[string]string{"Origin": "http://localhost:8080", "Sec-Fetch-Site": "same-origin"}[name])
		req.Header.Add(name, map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}[name])
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden {
			t.Fatalf("duplicate %s accepted: %d", name, response.Code)
		}
	}
}

func TestAdminAccessTokens(t *testing.T) {
	const token = "test-admin-secret"
	for _, addr := range []string{"localhost:8080", ":8080"} {
		engine := adminAccessTestEngine(addr, token)
		for _, tc := range []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"missing", nil, http.StatusUnauthorized},
			{"Bearer", map[string]string{"Authorization": "Bearer " + token}, http.StatusOK},
			{"Bearer case insensitive", map[string]string{"Authorization": "bearer " + token}, http.StatusOK},
			{"admin header", map[string]string{"X-Admin-Token": token}, http.StatusOK},
			{"wrong Bearer", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
			{"wrong header", map[string]string{"X-Admin-Token": "wrong"}, http.StatusUnauthorized},
			{"Basic is not Bearer", map[string]string{"Authorization": "Basic " + token}, http.StatusUnauthorized},
			{"Bearer extra fields", map[string]string{"Authorization": "Bearer " + token + " extra"}, http.StatusUnauthorized},
			{"conflicting headers", map[string]string{"Authorization": "Bearer wrong", "X-Admin-Token": token}, http.StatusUnauthorized},
			{"token cannot bypass origin", map[string]string{"Authorization": "Bearer " + token, "Origin": "http://evil.example"}, http.StatusForbidden},
		} {
			t.Run(addr+"/"+tc.name, func(t *testing.T) {
				response := adminAccessRequest(t, engine, http.MethodPost, "http://localhost:8080/admin/providers", nil, tc.headers)
				if response.Code != tc.want {
					t.Fatalf("status = %d, want %d", response.Code, tc.want)
				}
				if strings.Contains(response.Body.String(), token) {
					t.Fatal("response leaked the token")
				}
			})
		}
		response := adminAccessRequest(t, engine, http.MethodGet, "http://localhost:8080/admin?token="+token, nil, nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("query token granted access: %d", response.Code)
		}
		response = adminAccessRequest(t, engine, http.MethodGet, "http://localhost:8080/admin", nil, map[string]string{"Accept": "text/html", "Sec-Fetch-Mode": "navigate"})
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
			t.Fatalf("browser was not directed to login: %d %q", response.Code, response.Header().Get("Location"))
		}
	}
}

// Exercise intercepted login without registering /admin/login, then replay the
// issued cookie through the protected surface rather than testing private helpers.
func TestAdminAccessLoginSession(t *testing.T) {
	const token = "test-cookie-secret"
	for _, tc := range []struct {
		name   string
		addr   string
		origin string
		secure bool
	}{
		{"HTTPS remote", ":8443", "https://gateway.example:8443", true},
		{"HTTP loopback", "127.0.0.1:8080", "http://127.0.0.1:8080", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := adminAccessTestEngine(tc.addr, token)
			response := adminAccessRequest(t, engine, http.MethodGet, tc.origin+"/admin/login", nil, map[string]string{"Accept": "text/html"})
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `action="/admin/login"`) || !strings.Contains(response.Body.String(), `type="password"`) || strings.Contains(response.Body.String(), token) {
				t.Fatalf("unusable or leaking login form: %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("login response allows caching")
			}
			headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "text/html", "Origin": tc.origin, "Sec-Fetch-Site": "same-origin"}
			response = adminAccessRequest(t, engine, http.MethodPost, tc.origin+"/admin/login", strings.NewReader(url.Values{"token": {"wrong"}}.Encode()), headers)
			if response.Code != http.StatusUnauthorized || len(response.Result().Cookies()) != 0 {
				t.Fatalf("invalid login granted a session: %d", response.Code)
			}
			response = adminAccessRequest(t, engine, http.MethodPost, tc.origin+"/admin/login", strings.NewReader(url.Values{"token": {token}}.Encode()), headers)
			cookies := response.Result().Cookies()
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin" || len(cookies) != 1 {
				t.Fatalf("login failed: %d %q, cookies=%d", response.Code, response.Header().Get("Location"), len(cookies))
			}
			cookie := cookies[0]
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure != tc.secure || cookie.Path != "/admin" || cookie.Domain != "" || cookie.MaxAge <= 0 || cookie.Expires.IsZero() || strings.Contains(cookie.Value, token) {
				t.Fatal("session cookie is insecure, unbounded, or contains the admin token")
			}
			response = adminAccessRequest(t, engine, http.MethodGet, tc.origin+"/admin", nil, nil, cookie)
			if response.Code != http.StatusOK {
				t.Fatalf("session cannot open admin: %d", response.Code)
			}
			if tc.secure {
				response = adminAccessRequest(t, engine, http.MethodGet, strings.Replace(tc.origin, "https://", "http://", 1)+"/admin", nil, nil, cookie)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("remote cookie authenticated over plaintext HTTP: %d", response.Code)
				}
			}
			response = adminAccessRequest(t, engine, http.MethodPost, tc.origin+"/admin/providers", nil, map[string]string{"Origin": tc.origin, "Sec-Fetch-Site": "same-origin"}, cookie)
			if response.Code != http.StatusOK {
				t.Fatalf("session cannot mutate admin: %d", response.Code)
			}
			response = adminAccessRequest(t, engine, http.MethodPost, tc.origin+"/admin/providers", nil, map[string]string{"Origin": "https://evil.example"}, cookie)
			if response.Code != http.StatusForbidden {
				t.Fatalf("session bypassed CSRF: %d", response.Code)
			}
			response = adminAccessRequest(t, engine, http.MethodGet, tc.origin+"/admin", nil, map[string]string{"Authorization": "Bearer wrong"}, cookie)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("invalid explicit credentials fell back to cookie: %d", response.Code)
			}
			response = adminAccessRequest(t, adminAccessTestEngine(tc.addr, token), http.MethodGet, tc.origin+"/admin", nil, nil, cookie)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("session survived server key rotation: %d", response.Code)
			}
			tampered := *cookie
			tampered.Value = "0" + cookie.Value
			response = adminAccessRequest(t, engine, http.MethodGet, tc.origin+"/admin", nil, nil, &tampered)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("tampered session accepted: %d", response.Code)
			}
		})
	}
}

func TestAdminAccessLoginBoundaries(t *testing.T) {
	const token = "login-boundary-secret"
	engine := adminAccessTestEngine(":8080", token)
	response := adminAccessRequest(t, engine, http.MethodPost, "http://gateway.example:8080/admin/login", strings.NewReader("token="+token), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://gateway.example:8080"})
	if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
		t.Fatalf("remote plaintext login issued a cookie: %d", response.Code)
	}
	engine = adminAccessTestEngine("localhost:8080", token)
	for _, headers := range []map[string]string{
		{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://evil.example"},
		{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://localhost:8080", "Sec-Fetch-Site": "cross-site"},
		{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null"},
	} {
		response = adminAccessRequest(t, engine, http.MethodPost, "http://localhost:8080/admin/login", strings.NewReader("token="+token), headers)
		if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
			t.Fatalf("cross-site login issued a cookie: %d", response.Code)
		}
	}
	for _, body := range []string{"", "token=wrong&token=" + token, "token=" + strings.Repeat("a", 9000)} {
		response = adminAccessRequest(t, engine, http.MethodPost, "http://localhost:8080/admin/login", strings.NewReader(body), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://localhost:8080"})
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid login form accepted: %d", response.Code)
		}
		if len(response.Result().Cookies()) != 0 {
			t.Fatal("invalid form issued a cookie")
		}
	}
	for _, path := range []string{"/v1/models", "/administrator"} {
		response = adminAccessRequest(t, adminAccessTestEngine(":8080", ""), http.MethodGet, "http://localhost:8080"+path, nil, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("non-admin path %s was blocked: %d", path, response.Code)
		}
	}
}

// The signature may be valid while the session is expired; rejecting only
// tampered cookies would let captured sessions authenticate indefinitely.
func TestAdminAccessSessionExpiration(t *testing.T) {
	key := [32]byte{1}
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{"1.session-nonce", false},
		{"4102444800.session-nonce", true},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://gateway.example/admin", http.NoBody)
		req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: tc.payload + "." + adminSessionSignature(tc.payload, key)})
		if got := adminValidSession(req, key); got != tc.want {
			t.Fatalf("session %q valid = %v, want %v", tc.payload, got, tc.want)
		}
	}
}

// A brand-new checkout runs `go run ./cmd/serve` with no flags and opens the
// admin console in a browser. That path must not land on the 403 for a
// non-loopback bind, so the default address has to be loopback.
func TestDefaultAddrKeepsAdminOpen(t *testing.T) {
	if !adminLoopbackBind(defaultAddr) {
		t.Fatalf("default -addr %q is not loopback; /admin would answer 403 out of the box", defaultAddr)
	}
	engine := adminAccessTestEngine(defaultAddr, "")
	response := adminAccessRequest(t, engine, http.MethodGet, "http://localhost"+portOnly(defaultAddr)+"/admin", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("default config blocked /admin: %d %s", response.Code, response.Body.String())
	}
}

// The status line and Claude Code's model cache shorten the listen address to
// ":port"; widening a loopback default to 127.0.0.1 must not break either.
func TestPortOnly(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"127.0.0.1:8080", ":8080"},
		{":8080", ":8080"},
		{" [::1]:8080 ", ":8080"},
		{"localhost:8080", ":8080"},
		{"0.0.0.0:9090", ":9090"},
		{"", ""},
	} {
		if got := portOnly(tc.addr); got != tc.want {
			t.Fatalf("portOnly(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}
