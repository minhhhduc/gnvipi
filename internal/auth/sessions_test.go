package auth

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestNewSessionToken(t *testing.T) {
	a, err := NewSessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	b, err := NewSessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if a == b {
		t.Fatal("session tokens repeat")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token is not 32 base64url bytes: %q (%v)", a, err)
	}
	if len(HashToken(a)) != 32 {
		t.Fatal("session hash must be SHA-256")
	}
	if string(HashToken(a)) == a {
		t.Fatal("hash equals plaintext")
	}
}

func TestSessionCookie(t *testing.T) {
	c := SessionCookie("token-value", true, DefaultSessionConfig())
	if c.Name != SessionCookieName || c.Path != "/admin" || !c.HttpOnly ||
		!c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected cookie: %+v", c)
	}
	if c.MaxAge != int(DefaultSessionConfig().AbsoluteTimeout.Seconds()) {
		t.Fatalf("max age %d", c.MaxAge)
	}
	plain := SessionCookie("token-value", false, DefaultSessionConfig())
	if plain.Secure {
		t.Fatal("non-TLS cookie must not set Secure")
	}
	clear := ClearSessionCookie(true)
	if clear.MaxAge != -1 || clear.Value != "" {
		t.Fatalf("unexpected clear cookie: %+v", clear)
	}
}
