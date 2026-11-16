package auth

import (
	"strings"
	"sync"
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

func TestTokenCacheStoreIfGenRejectsStalePurgeAll(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	_, gen, ok := cache.LookupWithGen("raw", TokenKindGateway)
	if ok {
		t.Fatal("empty cache returned a hit")
	}
	cache.PurgeAll()
	if cache.StoreIfGen("raw", TokenKindGateway, TokenIdentity{UserID: "u1"}, gen) {
		t.Fatal("StoreIfGen stored stale identity after PurgeAll")
	}
	if _, ok := cache.Lookup("raw", TokenKindGateway); ok {
		t.Fatal("stale identity became cached after PurgeAll")
	}

	_, freshGen, ok := cache.LookupWithGen("raw", TokenKindGateway)
	if ok {
		t.Fatal("unexpected hit after rejected StoreIfGen")
	}
	if !cache.StoreIfGen("raw", TokenKindGateway, TokenIdentity{UserID: "u1"}, freshGen) {
		t.Fatal("StoreIfGen rejected fresh generation")
	}
	if got, ok := cache.Lookup("raw", TokenKindGateway); !ok || got.UserID != "u1" {
		t.Fatalf("lookup = %+v/%v, want u1/true", got, ok)
	}
}

func TestTokenCacheStoreIfGenRejectsStalePurge(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	_, gen, ok := cache.LookupWithGen("raw", TokenKindGateway)
	if ok {
		t.Fatal("empty cache returned a hit")
	}
	cache.Purge("raw", TokenKindGateway)
	if cache.StoreIfGen("raw", TokenKindGateway, TokenIdentity{UserID: "u1"}, gen) {
		t.Fatal("StoreIfGen stored stale identity after Purge")
	}
	if _, ok := cache.Lookup("raw", TokenKindGateway); ok {
		t.Fatal("stale identity became cached after Purge")
	}
}

func TestTokenCacheStoreIfGenConcurrentRevokeRace(t *testing.T) {
	cache := NewTokenCache(time.Minute, 8)
	missed := make(chan struct{})
	purged := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, gen, ok := cache.LookupWithGen("revoked-token", TokenKindGateway)
		if ok {
			t.Error("expected initial cache miss")
			return
		}
		close(missed)
		<-purged
		if cache.StoreIfGen("revoked-token", TokenKindGateway, TokenIdentity{UserID: "u1"}, gen) {
			t.Error("in-flight validation stored identity after PurgeAll")
		}
	}()

	<-missed
	cache.PurgeAll()
	close(purged)
	wg.Wait()

	if _, ok := cache.Lookup("revoked-token", TokenKindGateway); ok {
		t.Fatal("revoked token was re-cached by in-flight validation")
	}
}
