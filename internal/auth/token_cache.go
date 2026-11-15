package auth

import (
	"container/list"
	"sync"
	"time"
)

// TokenCacheTTL bounds how long a validated token identity may be reused. A
// gateway token authenticates every /v1 request, often long streams, so the
// window is sized to keep DB validation off the hot path; revoke handlers call
// PurgeAll so revocation still applies immediately.
const TokenCacheTTL = 5 * time.Minute

// tokenCacheEntry is one cached identity. Entries are keyed by the token hash,
// never the raw token.
type tokenCacheEntry struct {
	key      string
	identity TokenIdentity
	expires  time.Time
}

// TokenCache is a bounded TTL cache of validated token identities. It is safe
// for concurrent use. Failed validations are never stored.
type TokenCache struct {
	ttl time.Duration
	max int

	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // most recently used at the front
}

// NewTokenCache builds a cache holding at most max identities for ttl.
func NewTokenCache(ttl time.Duration, max int) *TokenCache {
	if ttl <= 0 {
		ttl = TokenCacheTTL
	}
	if max <= 0 {
		max = 4096
	}
	return &TokenCache{ttl: ttl, max: max, entries: make(map[string]*list.Element), order: list.New()}
}

func cacheKey(raw string, kind TokenKind) string {
	// Raw tokens are never used as keys.
	return string(HashToken(raw)) + "\x00" + string(kind)
}

// Lookup returns a cached identity when one is still fresh.
func (c *TokenCache) Lookup(raw string, kind TokenKind) (TokenIdentity, bool) {
	if c == nil || raw == "" {
		return TokenIdentity{}, false
	}
	key := cacheKey(raw, kind)
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return TokenIdentity{}, false
	}
	entry := element.Value.(*tokenCacheEntry)
	if !time.Now().Before(entry.expires) {
		c.order.Remove(element)
		delete(c.entries, key)
		return TokenIdentity{}, false
	}
	c.order.MoveToFront(element)
	return entry.identity, true
}

// Store caches a validated identity for at most the configured TTL.
func (c *TokenCache) Store(raw string, kind TokenKind, identity TokenIdentity) {
	if c == nil || raw == "" {
		return
	}
	key := cacheKey(raw, kind)
	entry := &tokenCacheEntry{key: key, identity: identity, expires: time.Now().Add(c.ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value = entry
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(entry)
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*tokenCacheEntry).key)
	}
}

// PurgeAll drops every cached identity. Revocation is rare and not on the hot
// path, so flushing the whole cache is cheaper than tracking token ids.
func (c *TokenCache) PurgeAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element)
	c.order.Init()
}

// Purge drops one cached identity, for callers that revoke a token and want the
// change to apply immediately.
func (c *TokenCache) Purge(raw string, kind TokenKind) {
	if c == nil || raw == "" {
		return
	}
	key := cacheKey(raw, kind)
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.order.Remove(element)
		delete(c.entries, key)
	}
}
