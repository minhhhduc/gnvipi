package auth

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAPIToken(t *testing.T) {
	for _, tc := range []struct {
		kind   TokenKind
		marker string
		prefix int
	}{
		{TokenKindGateway, "gw_", 3 + 6},
		{TokenKindAdmin, "adm_", 4 + 6},
	} {
		raw, prefix, err := generateAPIToken(tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if !strings.HasPrefix(raw, tc.marker) || !strings.HasPrefix(prefix, tc.marker) {
			t.Fatalf("%s token missing marker: %q %q", tc.kind, raw, prefix)
		}
		if len(prefix) != tc.prefix || len(prefix) < 4 || len(prefix) > 16 {
			t.Fatalf("%s prefix length %d", tc.kind, len(prefix))
		}
		if len(HashToken(raw)) != 32 {
			t.Fatal("token hash must be SHA-256")
		}
		if strings.Contains(raw, prefix) == false {
			t.Fatal("display prefix must be a prefix of the token")
		}
	}
}

func TestResolveTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	gatewayDefault, err := resolveTokenExpiry(TokenKindGateway, "", TokenExpiry{}, now)
	if err != nil || !gatewayDefault.Equal(now.Add(GatewayTokenDefaultExpiry)) {
		t.Fatalf("gateway default: %v %v", gatewayDefault, err)
	}
	adminDefault, err := resolveTokenExpiry(TokenKindAdmin, ScopeRead, TokenExpiry{}, now)
	if err != nil || !adminDefault.Equal(now.Add(AdminTokenDefaultExpiry)) {
		t.Fatalf("admin default: %v %v", adminDefault, err)
	}
	if _, err := resolveTokenExpiry(TokenKindAdmin, ScopeRead, TokenExpiry{NoExpiry: true}, now); err != ErrTokenNoExpiryNotAllowed {
		t.Fatalf("admin no-expiry: %v", err)
	}
	if _, err := resolveTokenExpiry(TokenKindAdmin, "", TokenExpiry{}, now); err != ErrInvalidTokenScope {
		t.Fatalf("admin without scope: %v", err)
	}
	if _, err := resolveTokenExpiry(TokenKindGateway, ScopeRead, TokenExpiry{}, now); err != ErrInvalidTokenScope {
		t.Fatalf("gateway with scope: %v", err)
	}
	if at, err := resolveTokenExpiry(TokenKindGateway, "", TokenExpiry{NoExpiry: true}, now); err != nil || at != nil {
		t.Fatalf("gateway no-expiry: %v %v", at, err)
	}
	tooLate := now.Add(GatewayTokenDefaultExpiry + time.Hour)
	if _, err := resolveTokenExpiry(TokenKindGateway, "", TokenExpiry{At: &tooLate}, now); err != ErrInvalidTokenExpiry {
		t.Fatalf("gateway over 365d: %v", err)
	}
	past := now.Add(-time.Hour)
	if _, err := resolveTokenExpiry(TokenKindAdmin, ScopeRead, TokenExpiry{At: &past}, now); err != ErrInvalidTokenExpiry {
		t.Fatalf("past expiry: %v", err)
	}
}

func TestScopeHelpers(t *testing.T) {
	if !ScopeAllows(ScopeWrite, ScopeWrite) || !ScopeAllows(ScopeWrite, ScopeRead) ||
		!ScopeAllows(ScopeRead, ScopeRead) || ScopeAllows(ScopeRead, ScopeWrite) {
		t.Fatal("scope matrix wrong")
	}
	if err := RequireScope(ScopeRead, ScopeWrite); err != ErrInsufficientScope {
		t.Fatalf("RequireScope: %v", err)
	}
}

func TestAccountLockDuration(t *testing.T) {
	cfg := DefaultLockoutConfig()
	for failures, want := range map[int]time.Duration{
		4: 0, 5: time.Minute, 6: 2 * time.Minute, 7: 4 * time.Minute,
		8: 8 * time.Minute, 9: 15 * time.Minute, 20: 15 * time.Minute,
	} {
		if got := cfg.AccountLockDuration(failures); got != want {
			t.Errorf("%d failures: got %v, want %v", failures, got, want)
		}
	}
}

func TestClampPage(t *testing.T) {
	if l, o := clampPage(0, -1); l != 50 || o != 0 {
		t.Fatalf("defaults: %d %d", l, o)
	}
	if l, _ := clampPage(1000, 0); l != 200 {
		t.Fatalf("max: %d", l)
	}
	if l, _ := clampPage(25, 5); l != 25 {
		t.Fatalf("explicit: %d", l)
	}
}
