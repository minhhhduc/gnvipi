package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTokenCacheHitMissAndKind(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	identity := TokenIdentity{UserID: "u1", Role: RoleUser}

	if _, ok := cache.Lookup("raw-token", TokenKindGateway); ok {
		t.Fatal("empty cache returned a hit")
	}
	cache.Store("raw-token", TokenKindGateway, identity)
	if got, ok := cache.Lookup("raw-token", TokenKindGateway); !ok || got.UserID != "u1" {
		t.Fatalf("lookup = %+v/%v, want the stored identity", got, ok)
	}
	if _, ok := cache.Lookup("raw-token", TokenKindAdmin); ok {
		t.Fatal("another token kind must not hit")
	}
	for key := range cache.entries {
		if strings.Contains(key, "raw-token") {
			t.Fatal("cache key contains the raw token")
		}
	}
}

func TestTokenCacheExpires(t *testing.T) {
	cache := NewTokenCache(10*time.Millisecond, 8)
	cache.Store("raw-token", TokenKindGateway, TokenIdentity{UserID: "u1"})
	time.Sleep(15 * time.Millisecond)
	if _, ok := cache.Lookup("raw-token", TokenKindGateway); ok {
		t.Fatal("expired entry still hits")
	}
	if len(cache.entries) != 0 {
		t.Fatalf("expired entry left %d entries", len(cache.entries))
	}
}

func TestTokenCacheEvictsBeyondMax(t *testing.T) {
	cache := NewTokenCache(time.Minute, 2)
	cache.Store("a", TokenKindGateway, TokenIdentity{UserID: "a"})
	cache.Store("b", TokenKindGateway, TokenIdentity{UserID: "b"})
	cache.Store("c", TokenKindGateway, TokenIdentity{UserID: "c"})
	if len(cache.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(cache.entries))
	}
	if _, ok := cache.Lookup("a", TokenKindGateway); ok {
		t.Fatal("oldest entry was not evicted")
	}
}

func TestTokenCachePurge(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	cache.Store("raw", TokenKindGateway, TokenIdentity{UserID: "u1"})
	cache.Purge("raw", TokenKindGateway)
	if _, ok := cache.Lookup("raw", TokenKindGateway); ok {
		t.Fatal("purged entry still hits")
	}
}

func TestTokenCachePurgeAll(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	cache.Store("a", TokenKindGateway, TokenIdentity{UserID: "a"})
	cache.Store("b", TokenKindGateway, TokenIdentity{UserID: "b"})
	cache.PurgeAll()
	if len(cache.entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(cache.entries))
	}
}
