package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// SearchType is the intent class derived from the raw query. It drives backend
// selection and per-backend query adaptation; it never causes an LLM call.
type SearchType string

const (
	SearchGeneral    SearchType = "general"
	SearchAcademic   SearchType = "academic"
	SearchLegal      SearchType = "legal"
	SearchIdentifier SearchType = "identifier" // DOI / arXiv ID / PMID
)

// SearchContext is the derived intent. Heuristics only: no network, no model.
type SearchContext struct {
	Original   string
	Type       SearchType
	Keywords   []string
	Phrases    []string // multi-word terms worth quoting
	Instrument string   // CISG, UNCLOS, ...
	Provision  string   // Article 79, Section 2, ...
	ArxivID    string
	DOI        string
	Academic   bool
	Legal      bool
}

// SearchResult is the normalized cross-backend result model.
type SearchResult struct {
	Title       string
	URL         string
	Snippet     string
	Backend     string
	Authors     []string
	PublishedAt *time.Time
	DOI         string
	ArxivID     string
}

// QueryPlan is one backend's adapted view of the request.
type QueryPlan struct {
	Backend    string
	Queries    []string
	MaxResults int
	Identifier string // non-empty => direct lookup, skip query adaptation
}

// SearchBackend is the per-engine abstraction: each backend owns its query
// adaptation, request construction, response parsing, and failure policy.
type SearchBackend interface {
	Name() string
	// Adapt transforms the derived intent into this backend's query syntax.
	Adapt(ctx SearchContext) QueryPlan
	// Search executes the plan. A nil slice with nil error means a legitimate
	// zero-result page; a non-nil error means the backend is unhealthy and the
	// router should try the next backend.
	Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error)
	// Healthy reports whether the backend should be tried at all.
	Healthy() bool
}

// Backend errors. The router distinguishes "blocked, try another backend" from
// "search ran and found nothing" so a challenge page is never reported as
// "no results found".
var (
	ErrRateLimited = errors.New("backend rate limited")
	ErrBlocked     = errors.New("backend blocked the request")
	ErrChallenge   = errors.New("backend returned a challenge page")
	ErrUpstream    = errors.New("backend upstream error")
	ErrUnreachable = errors.New("backend unreachable")
	ErrParser      = errors.New("backend returned an unparseable response")
)

// classifyBackendError maps an HTTP status or page signature onto a backend
// error. Returning nil means the response is a normal page the parser owns.
func classifyBackendError(status int, body []byte) error {
	switch {
	case status == 429:
		return ErrRateLimited
	case status == 403:
		return ErrBlocked
	case status >= 500:
		return ErrUpstream
	case status == 200 && looksLikeChallenge(body):
		return ErrChallenge
	}
	return nil
}

// looksLikeChallenge detects robot-verification pages without following them.
func looksLikeChallenge(body []byte) bool {
	low := strings.ToLower(string(body))
	for _, marker := range []string{
		"unusual traffic",
		"/httpservice/retry/enablejs",
		"enable javascript",
		"our systems have detected",
		"robot verification",
		"please complete the security check",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// dedupeURLs collapses results pointing at the same normalized URL, keeping
// the first (highest-ranked) occurrence and preserving order.
func dedupeURLs(results []SearchResult) []SearchResult {
	seen := make(map[string]bool, len(results))
	out := results[:0]
	for _, r := range results {
		key := normalizeResultURL(r.URL)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		r.URL = key
		out = append(out, r)
	}
	return out
}

// normalizeResultURL canonicalizes a result URL for comparison and display.
func normalizeResultURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	u.RawFragment = ""
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	u.Host = host
	if u.Path == "/" {
		u.Path = ""
	} else if len(u.Path) > 1 {
		// ponytail: trailing-slash folding is a display choice, not full RFC
		// normalization; a redirect that adds "/" must not escape dedup.
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
	q := u.Query()
	q.Del("utm_source")
	q.Del("utm_medium")
	q.Del("utm_campaign")
	if len(q) == 0 {
		u.RawQuery = ""
	} else {
		u.RawQuery = q.Encode()
	}
	return u.String()
}
