package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIPResolverTrustedProxyChain(t *testing.T) {
	resolver, err := newClientIPResolver("10.0.0.0/8, 2001:db8:1::/48")
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}
	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		want       string
	}{
		{
			name:       "untrusted peer ignores spoofed headers",
			remoteAddr: "192.0.2.10:443",
			xff:        "198.51.100.20",
			xRealIP:    "198.51.100.21",
			want:       "192.0.2.10",
		},
		{
			name:       "trusted proxy uses forwarded client",
			remoteAddr: "10.0.0.2:443",
			xff:        "198.51.100.20",
			want:       "198.51.100.20",
		},
		{
			name:       "trusted chain walks right to left",
			remoteAddr: "10.0.0.2:443",
			xff:        "198.51.100.20, 10.0.0.1",
			want:       "198.51.100.20",
		},
		{
			name:       "untrusted forwarded hop stops spoofed prefix",
			remoteAddr: "10.0.0.2:443",
			xff:        "203.0.113.7, 198.51.100.20",
			want:       "198.51.100.20",
		},
		{
			name:       "trusted peer falls back to real ip",
			remoteAddr: "10.0.0.2:443",
			xRealIP:    "198.51.100.21",
			want:       "198.51.100.21",
		},
		{
			name:       "malformed forwarded chain falls back to peer",
			remoteAddr: "10.0.0.2:443",
			xff:        "198.51.100.20, not-an-ip",
			want:       "10.0.0.2",
		},
		{
			name:       "ipv6 trusted chain",
			remoteAddr: "[2001:db8:1::2]:443",
			xff:        "2001:db8:2::7, 2001:db8:1::1",
			want:       "2001:db8:2::7",
		},
		{
			name:       "zoned ipv6 peer keeps a lockout key",
			remoteAddr: "[fe80::1234%eth0]:443",
			want:       "fe80::1234",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test", http.NoBody)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				req.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if got := resolver.requestIP(req); got != tc.want {
				t.Fatalf("requestIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPResolverConfiguration(t *testing.T) {
	if _, err := newClientIPResolver("10.0.0.0/8,broken"); err == nil {
		t.Fatal("invalid trusted proxy accepted")
	}
	resolver, err := newClientIPResolver("192.0.2.10")
	if err != nil {
		t.Fatalf("parse trusted proxy address: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test", http.NoBody)
	req.RemoteAddr = "192.0.2.10:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	if got := resolver.requestIP(req); got != "198.51.100.20" {
		t.Fatalf("single trusted address requestIP() = %q", got)
	}
	mapped, err := newClientIPResolver("::ffff:10.0.0.0/104")
	if err != nil {
		t.Fatalf("parse mapped trusted proxy CIDR: %v", err)
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test", http.NoBody)
	req.RemoteAddr = "10.1.2.3:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	if got := mapped.requestIP(req); got != "198.51.100.20" {
		t.Fatalf("mapped trusted CIDR requestIP() = %q, want forwarded client", got)
	}
}

func TestDBAdminAuthTrustedProxyRecordsClientIP(t *testing.T) {
	h := newDBAdminHarness(t)
	resolver, err := newClientIPResolver("10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}
	h.state.clientIPs = resolver

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, adminLoginPath,
		strings.NewReader(`{"username":"owner","password":"wrong-password"}`))
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.20, 10.0.0.1")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	var storedIP string
	if err := h.pool.QueryRow(t.Context(), `SELECT host(ip) FROM login_attempts WHERE username = 'owner' ORDER BY id DESC LIMIT 1`).Scan(&storedIP); err != nil {
		t.Fatalf("read login attempt IP: %v", err)
	}
	if storedIP != "198.51.100.20" {
		t.Fatalf("stored login IP = %q, want forwarded client", storedIP)
	}
}
