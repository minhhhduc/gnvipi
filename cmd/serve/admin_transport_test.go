package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminAccessEmptyConnectionStateIsPlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, addr, target string
		want               int
	}{
		{"local form login", "127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusSeeOther},
		{"remote cookie login still needs TLS", ":8080", "http://gateway.example:8080", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := adminAccessTestEngine(tc.addr, "transport-token")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.target+"/admin/login", strings.NewReader(url.Values{"token": {"transport-token"}}.Encode()))
			// Go 1.26 net.Conn wrappers may expose an empty ConnectionState on HTTP.
			req.TLS = &tls.ConnectionState{}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", tc.target)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, req)
			if out.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", out.Code, tc.want, out.Body.String())
			}
			if tc.want == http.StatusSeeOther {
				cookies := out.Result().Cookies()
				if len(cookies) != 1 || cookies[0].Secure {
					t.Fatal("plaintext loopback login did not issue a usable cookie")
				}
			}
		})
	}
}
