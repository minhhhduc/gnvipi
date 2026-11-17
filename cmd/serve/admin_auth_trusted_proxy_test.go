package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyHTTPSAdminTransport(t *testing.T) {
	trusted, err := newClientIPResolver("127.0.0.1/32")
	if err != nil {
		t.Fatalf("newClientIPResolver: %v", err)
	}

	makeProxyReq := func(remoteAddr, proto, host, origin string) *http.Request {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/admin/api/auth/login", http.NoBody)
		req.RemoteAddr = remoteAddr
		req.Host = host
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return req
	}

	// Spoofed X-Forwarded-Proto from an untrusted peer must be rejected.
	untrustedReq := makeProxyReq("203.0.113.9:12345", "https", "gnvipi.example.com", "https://gnvipi.example.com")
	if adminTLSRequestWithProxies(untrustedReq, trusted) {
		t.Fatal("expected untrusted peer X-Forwarded-Proto=https to be rejected")
	}
	state := &authState{loopbackBind: true, clientIPs: trusted}
	if state.cookieTransportAllowed(untrustedReq) {
		t.Fatal("expected cookieTransportAllowed=false for untrusted external host")
	}
	if adminSameOriginWithProxies(untrustedReq, trusted) {
		t.Fatal("expected adminSameOriginWithProxies=false for untrusted peer")
	}

	// Trusted proxy forwarding X-Forwarded-Proto=https is accepted for cookies and origin checks.
	trustedReq := makeProxyReq("127.0.0.1:43210", "https", "gnvipi.example.com", "https://gnvipi.example.com")
	if !adminTLSRequestWithProxies(trustedReq, trusted) {
		t.Fatal("expected trusted peer X-Forwarded-Proto=https to be accepted")
	}
	if !state.cookieTransportAllowed(trustedReq) {
		t.Fatal("expected cookieTransportAllowed=true behind trusted HTTPS proxy")
	}
	if !adminSameOriginWithProxies(trustedReq, trusted) {
		t.Fatal("expected adminSameOriginWithProxies=true behind trusted HTTPS proxy")
	}
	origin, ok := adminRequestOriginWithProxies(trustedReq, trusted)
	if !ok || origin != "https://gnvipi.example.com:443" {
		t.Fatalf("adminRequestOriginWithProxies = (%q, %v), want (%q, true)", origin, ok, "https://gnvipi.example.com:443")
	}
}
