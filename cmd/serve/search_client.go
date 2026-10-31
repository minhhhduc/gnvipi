package main

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// reliability.go — the transport layer shared by every search backend.
//
// Design notes:
//   - Rate limiting is a per-backend token bucket so one slow engine can never
//     starve another.
//   - Backoff respects Retry-After and adds jitter, so a fleet of gateways
//     retrying a 429 does not synchronize into a second burst.
//   - The circuit breaker is per backend, not global: Bing being down must not
//     disable arXiv.
//   - Blocking is detected, never silently reported as "0 results".

// backendHealth tracks failures per backend and opens a circuit after a run of
// them, so the router stops sending traffic to an engine that is challenging us.
type backendHealth struct {
	mu             sync.Mutex
	failures       int
	openedAt       time.Time
	cooldown       time.Duration
	failuresToOpen int
}

func newBackendHealth(cooldown time.Duration, failuresToOpen int) *backendHealth {
	return &backendHealth{cooldown: cooldown, failuresToOpen: failuresToOpen}
}

func (h *backendHealth) Healthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.openedAt.IsZero() {
		return true
	}
	if time.Since(h.openedAt) < h.cooldown {
		return false
	}
	// Cooldown elapsed: allow one probe request through, but stay open until it
	// succeeds. A blocked engine should not be re-flooded on recovery.
	h.failures = h.failuresToOpen - 1
	h.openedAt = time.Now()
	return true
}

func (h *backendHealth) Record(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.failures = 0
		h.openedAt = time.Time{}
		return
	}
	if !errors.Is(err, ErrRateLimited) && !errors.Is(err, ErrBlocked) &&
		!errors.Is(err, ErrChallenge) && !errors.Is(err, ErrUpstream) {
		return // transient network blips do not trip the breaker
	}
	h.failures++
	if h.failures >= h.failuresToOpen && h.openedAt.IsZero() {
		h.openedAt = time.Now()
	}
}

// rateLimiter is a minimal token bucket: refill rate is one token per interval,
// bursts up to capacity are allowed.
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	cap      float64
	interval time.Duration
	last     time.Time
}

func newRateLimiter(capacity float64, interval time.Duration) *rateLimiter {
	return &rateLimiter{tokens: capacity, cap: capacity, interval: interval, last: time.Now()}
}

func (l *rateLimiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(l.last)
		if elapsed > 0 {
			l.tokens += float64(elapsed) / float64(l.interval)
			if l.tokens > l.cap {
				l.tokens = l.cap
			}
			l.last = now
		}
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		deficit := 1 - l.tokens
		wait := time.Duration(float64(l.interval) * deficit)
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// requestCache memoizes successful backend responses for a bounded TTL. Calls
// with the same key share a single in-flight request, so parallel search
// components hitting the same query do not multiply upstream load.
type requestCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	results []SearchResult
	expires time.Time
}

func newRequestCache(ttl time.Duration) *requestCache {
	return &requestCache{items: make(map[string]cacheEntry), ttl: ttl}
}

// newSharedClient builds a backend transport with its own limiter and breaker.
// Limiter capacity and interval set the request ceiling, failuresToOpen sets
// how many back-to-back blocks trip the circuit.
func newSharedClient(userAgent string, capacity float64, interval time.Duration, maxRetries, failuresToOpen int) *sharedSearchClient {
	return &sharedSearchClient{
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if !publicSearchHost(req.URL) {
					return errSearchRedirect
				}
				if len(via) >= 5 {
					return errSearchRedirect
				}
				return nil
			},
		},
		limiter:    newRateLimiter(capacity, interval),
		health:     newBackendHealth(60*time.Second, failuresToOpen),
		maxRetries: maxRetries,
		userAgent:  userAgent,
	}
}

var errSearchRedirect = errors.New("search redirect must use a public HTTPS host")

// setTestTransport replaces every backend's HTTP transport. Tests use this to
// point all backends at an httptest server; production never calls it.
func (g *searchGateway) setTestTransport(rt http.RoundTripper) {
	for _, c := range g.clients {
		c.httpClient.Transport = rt
	}
}

// ponytail: the cache is unbounded in size; search volumes are low and entries
// expire fast. Replace with an LRU if a busy gateway ever shows growth here.
func (c *requestCache) Get(key string) ([]SearchResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.results, true
}

func (c *requestCache) Set(key string, results []SearchResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) > 512 {
		for k, item := range c.items {
			if time.Now().After(item.expires) {
				delete(c.items, k)
			}
		}
	}
	c.items[key] = cacheEntry{results: results, expires: time.Now().Add(c.ttl)}
}

// sharedSearchClient wraps *http.Client with the reliability policies every
// backend needs: per-backend rate limit, bounded backoff honoring Retry-After,
// and circuit-breaker bookkeeping. Backends supply the limiter and health so
// the policy is tuned per engine rather than globally.
type sharedSearchClient struct {
	httpClient *http.Client
	limiter    *rateLimiter
	health     *backendHealth
	cache      *requestCache
	maxRetries int
	userAgent  string
}

func (s *sharedSearchClient) do(ctx context.Context, target string, extraHeaders map[string]string) (int, []byte, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return 0, nil, err
	}
	if !s.health.Healthy() {
		return 0, nil, ErrBlocked
	}
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ { // ponytail: bounded to 3 attempts, no retry storms
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("User-Agent", s.userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			lastErr = ErrUnreachable
			if attempt < s.maxRetries {
				s.backoff(ctx, attempt, "")
				continue
			}
			break
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = ErrUnreachable
			break
		}
		if cerr := classifyBackendError(resp.StatusCode, body); cerr != nil {
			lastErr = cerr
			if attempt < s.maxRetries && (cerr == ErrRateLimited || cerr == ErrUpstream) {
				s.backoff(ctx, attempt, resp.Header.Get("Retry-After"))
				continue
			}
			break
		}
		s.health.Record(nil)
		return resp.StatusCode, body, nil
	}
	s.health.Record(lastErr)
	if lastErr == nil {
		lastErr = ErrUpstream
	}
	return 0, nil, lastErr
}

// backoff waits a bounded, jittered interval honoring a server Retry-After.
func (s *sharedSearchClient) backoff(ctx context.Context, attempt int, retryAfter string) {
	wait := time.Duration(0)
	if d := parseRetryAfter(retryAfter); d > 0 {
		wait = d
	} else {
		// 500ms, 1s, 2s ... capped, plus up to 25% jitter.
		base := time.Duration(500) << attempt * time.Millisecond
		if base > 4*time.Second {
			base = 4 * time.Second
		}
		wait = base + time.Duration(rand.Int64N(int64(base/4)))
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if d, err := time.ParseDuration(raw + "s"); err == nil {
		return d
	}
	if t, err := http.ParseTime(raw); err == nil {
		return time.Until(t)
	}
	return 0
}
