package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// search_router.go — intent-driven backend selection, result merging, and
// contextual ranking.
//
// The router never sends one query to every backend. It derives intent, asks
// each selected backend to adapt that intent into its own syntax, runs the
// primary chain, and falls back only when a backend reports it is unhealthy.

// searchGateway holds the backends and their reliability state.
type searchGateway struct {
	mu          sync.RWMutex
	htmlPrimary string // "bing" or "duckduckgo": the user's configured engine
	backends    map[string]SearchBackend
	clients     map[string]*sharedSearchClient
	cache       *requestCache
	timeout     time.Duration
}

func newSearchGateway() *searchGateway {
	const gatewayUA = "Mozilla/5.0 (compatible; GatewaySearch/1.0)"
	const scholarlyUA = "gnvipi-gateway/1.0 (search gateway; mailto:admin@example.com)"
	clients := map[string]*sharedSearchClient{
		bingName:     newSharedClient(gatewayUA, 1.5, 2*time.Second, 3, 5),
		ddgName:      newSharedClient(gatewayUA, 1.2, 2*time.Second, 3, 5),
		arxivName:    newSharedClient(scholarlyUA, 0.5, 3*time.Second, 2, 4),
		crossrefName: newSharedClient(scholarlyUA, 0.9, 1500*time.Millisecond, 3, 5),
		openalexName: newSharedClient(scholarlyUA, 0.9, 1500*time.Millisecond, 3, 5),
		s2Name:       newSharedClient(gatewayUA, 0.4, 3*time.Second, 2, 4),
	}
	gw := &searchGateway{
		backends: map[string]SearchBackend{
			bingName:     &bingBackend{client: clients[bingName]},
			ddgName:      &ddgBackend{client: clients[ddgName]},
			arxivName:    &arxivBackend{client: clients[arxivName]},
			crossrefName: &crossrefBackend{client: clients[crossrefName]},
			openalexName: &openalexBackend{client: clients[openalexName]},
			s2Name:       &s2Backend{client: clients[s2Name]},
		},
		clients:     clients,
		cache:       newRequestCache(5 * time.Minute),
		htmlPrimary: bingName,
		timeout:     20 * time.Second,
	}
	return gw
}

// setHTMLEngine switches the general-search primary between Bing and DDG.
func (g *searchGateway) setHTMLEngine(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if name != bingName && name != ddgName {
		return
	}
	g.htmlPrimary = name
}

func (g *searchGateway) htmlEngine() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.htmlPrimary
}

// routeBackends returns the ordered backend chain for an intent.
//
// Identifier queries go straight to the metadata providers and only fall back
// to web search when those are all unavailable. Academic queries run the
// scholarly chain with web search as discovery. Legal queries use the HTML
// engine first (it surfaces primary sources well) with scholarly providers as
// a second source. General queries use the HTML engine and its mirror.
func (g *searchGateway) routeBackends(ctx SearchContext) []string {
	html, fallback := g.htmlEngine(), ddgName
	if html == ddgName {
		fallback = bingName
	}
	switch ctx.Type {
	case SearchIdentifier:
		chain := []string{arxivName, crossrefName, s2Name, openalexName, html}
		if ctx.DOI != "" {
			chain = []string{crossrefName, openalexName, s2Name, html}
		}
		return chain
	case SearchAcademic:
		return []string{arxivName, openalexName, crossrefName, s2Name, html}
	case SearchLegal:
		return []string{html, crossrefName, openalexName, fallback}
	default:
		return []string{html, fallback}
	}
}

// SearchTrace records one backend's outcome for diagnostics and live tests.
type SearchTrace struct {
	Backend  string
	Queries  []string
	Status   string
	Results  int
	Error    string
	CacheHit bool
	Fallback bool
}

// search executes the full pipeline: intent, routing, per-backend adaptation,
// caching, execution, normalization, dedup, and ranking.
func (g *searchGateway) search(ctx context.Context, query string, maxResults int) ([]SearchResult, []SearchTrace) {
	intent := deriveSearchContext(query)
	chain := g.routeBackends(intent)
	var traces []SearchTrace

	var merged []SearchResult
	var primaryErr error
	for i, name := range chain {
		backend, ok := g.backends[name]
		if !ok || !backend.Healthy() {
			traces = append(traces, SearchTrace{Backend: name, Status: "skipped"})
			continue
		}
		plan := backend.Adapt(intent)
		key := name + "|" + strings.Join(plan.Queries, "||")
		if cached, hit := g.cache.Get(key); hit {
			traces = append(traces, SearchTrace{
				Backend: name, Queries: plan.Queries, Status: "cache_hit",
				Results: len(cached), CacheHit: true, Fallback: i > 0,
			})
			merged = append(merged, cached...)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, g.timeout)
		results, err := backend.Search(cctx, plan)
		cancel()
		if err != nil {
			traces = append(traces, SearchTrace{
				Backend: name, Queries: plan.Queries, Status: "error",
				Error: err.Error(), Fallback: i > 0,
			})
			if i == 0 {
				primaryErr = err
			}
			continue
		}
		g.cache.Set(key, results)
		traces = append(traces, SearchTrace{
			Backend: name, Queries: plan.Queries, Status: "ok",
			Results: len(results), Fallback: i > 0,
		})
		merged = append(merged, results...)
	}

	merged = dedupeURLs(merged)
	ranked := rankResults(intent, merged)
	if maxResults > 0 && len(ranked) > maxResults {
		ranked = ranked[:maxResults]
	}
	if primaryErr != nil && len(ranked) == 0 {
		// ponytail: the router must surface a reason, not an empty result set,
		// when every backend failed — callers depend on distinguishing that.
		traces = append(traces, SearchTrace{Backend: "router", Status: "all_failed", Error: primaryErr.Error()})
	}
	return ranked, traces
}

// rankedSearch is the entry point used by the MCP handler.
func (g *searchGateway) rankedSearch(ctx context.Context, query string) ([]SearchResult, []SearchTrace) {
	return g.search(ctx, query, 10)
}

// rankResults scores results for the intent. Signals are weighted per type:
// legal and academic queries reward authority and identifier matches; general
// queries trust the engine's ordering.
func rankResults(ctx SearchContext, results []SearchResult) []SearchResult {
	if len(results) <= 1 {
		return results
	}
	scored := make([]scoredResult, 0, len(results))
	for i, r := range results {
		scored = append(scored, scoredResult{result: r, index: i, score: relevanceScore(ctx, r, i)})
	}
	// Stable sort keeps backend order among equal scores.
	for i := 1; i < len(scored); i++ {
		j := i
		for j > 0 && scored[j].score > scored[j-1].score {
			scored[j], scored[j-1] = scored[j-1], scored[j]
			j--
		}
	}
	out := make([]SearchResult, len(scored))
	for i, s := range scored {
		out[i] = s.result
	}
	return out
}

type scoredResult struct {
	result SearchResult
	index  int
	score  float64
}

func relevanceScore(ctx SearchContext, r SearchResult, position int) float64 {
	// Position is the engine's own ranking; it starts high and decays gently so
	// authority can still promote a lower-ranked primary source.
	score := 1.0 / float64(position+1)
	switch ctx.Type {
	case SearchIdentifier:
		score += identifierBonus(ctx, r) * 3.0
	case SearchLegal:
		score += authorityBonus(r.URL) * 2.0
		score += provisionMatch(ctx, r) * 1.5
	case SearchAcademic:
		score += academicBonus(r) * 1.5
		if r.Abstractish() {
			score += 0.05
		}
	}
	score -= noisePenalty(r, ctx)
	return score
}

// identifierBonus rewards results that actually contain the requested
// identifier, so a pagination page that merely ranks for it cannot win.
func identifierBonus(ctx SearchContext, r SearchResult) float64 {
	bonus := 0.0
	if ctx.ArxivID != "" {
		if r.ArxivID == ctx.ArxivID || strings.Contains(r.URL, "/abs/"+ctx.ArxivID) {
			bonus += 1.0
		} else if strings.Contains(r.URL, ctx.ArxivID) || strings.Contains(r.Title, ctx.ArxivID) {
			bonus += 0.3
		} else {
			bonus -= 0.4
		}
	}
	if ctx.DOI != "" {
		if strings.EqualFold(r.DOI, ctx.DOI) {
			bonus += 1.0
		} else if strings.Contains(strings.ToLower(r.URL), strings.ToLower(ctx.DOI)) {
			bonus += 0.3
		}
	}
	return bonus
}

func provisionMatch(ctx SearchContext, r SearchResult) float64 {
	if ctx.Provision == "" {
		return 0
	}
	blob := strings.ToLower(r.Title + " " + r.Snippet + " " + r.URL)
	needle := strings.ToLower(ctx.Provision)
	if strings.Contains(blob, needle) {
		return 1.0
	}
	if strings.Contains(blob, strings.ReplaceAll(needle, " ", "")) {
		return 0.6
	}
	return 0
}

// authorityBonus promotes primary and official sources for legal queries. The
// table is small by design: authority is a ranking signal, not a whitelist,
// and unknown domains score zero rather than negative.
func authorityBonus(rawURL string) float64 {
	u, host := splitHost(rawURL)
	if host == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(host, ".un.org") || host == "un.org":
		return 1.0
	case strings.HasSuffix(host, ".europa.eu"):
		return 0.9
	case strings.HasSuffix(host, ".gov") || strings.HasSuffix(host, ".gov.uk") || strings.HasSuffix(host, ".gov.au"):
		return 0.8
	case strings.HasSuffix(host, ".edu") || strings.HasSuffix(host, ".ac.uk") || strings.HasSuffix(host, ".edu.au"):
		return 0.7
	case strings.HasSuffix(host, "uncitral.un.org") || host == "cisg-online.org" || host == "cisg.law.pace.edu":
		return 1.0
	case host == "iccwbo.org" || host == "unidroit.org" || host == "wto.org" || host == "wipo.int" || host == "imo.org":
		return 0.9
	}
	_ = u
	return 0
}

// canonicalAuthorityDomains lists the primary domains for a legal instrument,
// used only to build Bing's site-restricted query variant.
func canonicalAuthorityDomains(instrument string) []string {
	return legalAuthorityDomains(instrument)
}

func academicBonus(r SearchResult) float64 {
	bonus := 0.0
	if r.DOI != "" {
		bonus += 0.2
	}
	if r.ArxivID != "" {
		bonus += 0.2
	}
	if len(r.Authors) > 0 {
		bonus += 0.1
	}
	if r.PublishedAt != nil {
		bonus += 0.1
	}
	return bonus
}

// noisePenalty demotes pages whose shape signals low value: pagination, tag and
// category archives, and thin aggregators. It is a penalty on shape, not a
// domain blacklist.
func noisePenalty(r SearchResult, ctx SearchContext) float64 {
	u, host := splitHost(r.URL)
	if host == "" {
		return 0.5
	}
	path := u.Path()
	penalty := 0.0
	for _, pattern := range []string{"/page/", "/tag/", "/category/", "/tags/", "/author/", "/archive/"} {
		if strings.Contains(path, pattern) {
			penalty += 0.6
			break
		}
	}
	if strings.HasSuffix(path, "/pdf") && r.Title == "" {
		penalty += 0.1
	}
	// A short title with no snippet is usually a thin aggregator listing.
	if len(r.Title) < 12 && r.Snippet == "" {
		penalty += 0.2
	}
	// Identifier queries: a result that never mentions the identifier is noise.
	if ctx.ArxivID != "" && !strings.Contains(r.URL, ctx.ArxivID) && !strings.Contains(r.Title, ctx.ArxivID) {
		penalty += 0.5
	}
	return penalty
}

func splitHost(rawURL string) (parsedURL, string) {
	if rawURL == "" {
		return parsedURL{}, ""
	}
	parsed, err := parseURL(rawURL)
	if err != nil {
		return parsedURL{}, ""
	}
	return parsed, hostOf(parsed)
}

// Abstractish reports whether this result carries a real abstract rather than a
// bare title, so academic ranking can prefer complete records.
func (r SearchResult) Abstractish() bool {
	return len(r.Snippet) > 80
}

var _ = log.Printf
