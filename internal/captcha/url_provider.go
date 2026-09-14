package captcha

import (
	"context"
	"sync"
)

// URLProvider supplies the playground (or harness) URL whose hCaptcha widget
// mints the next token. Multiple implementations let the captcha subsystem
// run against a fixed URL (legacy default), a Round-Robin rotating URL list,
// or a ChampionState-driven URL (nvpi-style auto-fallback across the registry).
//
// Implementations must be safe for concurrent use — BrowserGroup calls
// PlaygroundURL on the request path.
type URLProvider interface {
	// PlaygroundURL returns the URL the Browser should navigate to before
	// extracting a token. Returned string is used as a CDP Navigate target.
	PlaygroundURL(ctx context.Context) string
}

// StaticURLProvider returns a fixed URL every call.
type StaticURLProvider struct{ URL string }

// PlaygroundURL is part of the URLProvider interface.
func (s StaticURLProvider) PlaygroundURL(context.Context) string { return s.URL }

// defaultPlaygroundURL is the fallback when no champion or rotation is selected.
const defaultPlaygroundURL = "https://build.nvidia.com/deepseek-ai/deepseek-v4-flash-0731/playground"

// NewDefaultURLProvider returns the legacy single-URL provider.
func NewDefaultURLProvider() URLProvider { return StaticURLProvider{URL: defaultPlaygroundURL} }

// RotatingURLProvider distributes token extraction across multiple candidate
// playground pages in round-robin order, preventing worker contention and
// avoiding rate limits or single-URL bottlenecks.
type RotatingURLProvider struct {
	mu   sync.Mutex
	urls []string
	idx  int
}

// NewRotatingURLProvider creates a URLProvider that rotates through urls.
func NewRotatingURLProvider(urls []string) URLProvider {
	valid := make([]string, 0, len(urls))
	for _, u := range urls {
		if u != "" {
			valid = append(valid, u)
		}
	}
	if len(valid) == 0 {
		return NewDefaultURLProvider()
	}
	return &RotatingURLProvider{urls: valid}
}

// PlaygroundURL returns the next URL in the rotation.
func (r *RotatingURLProvider) PlaygroundURL(context.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.urls) == 0 {
		return defaultPlaygroundURL
	}
	u := r.urls[r.idx%len(r.urls)]
	r.idx++
	return u
}

