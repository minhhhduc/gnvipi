package main

import "testing"

// TestArxivIdentifierPrefersDirectLookup checks an arXiv ID routes to arXiv
// first and becomes an id: lookup rather than a web search.
func TestArxivIdentifierPrefersDirectLookup(t *testing.T) {
	ctx := deriveSearchContext("arxiv 2608.14210")
	if ctx.Type != SearchIdentifier {
		t.Fatalf("type = %q, want identifier", ctx.Type)
	}
	if ctx.ArxivID != "2608.14210" {
		t.Fatalf("arxiv id = %q", ctx.ArxivID)
	}
	gw := newSearchGateway()
	chain := gw.routeBackends(ctx)
	if chain[0] != arxivName {
		t.Fatalf("chain = %v, want arXiv first", chain)
	}
	plan := gw.backends[arxivName].Adapt(ctx)
	if len(plan.Queries) != 1 || plan.Queries[0] != "id:2608.14210" {
		t.Fatalf("arXiv plan = %v", plan.Queries)
	}
	bing := gw.backends[bingName].Adapt(ctx)
	for _, q := range bing.Queries {
		if !containsString(q, "2608.14210") {
			t.Fatalf("Bing must include the identifier, got %q", q)
		}
	}
}

// TestLegalQueryAdaptationDiffers checks a legal query is detected, quoted for
// Bing, given an authoritative-domain variant, and adapted differently for
// academic providers.
func TestLegalQueryAdaptationDiffers(t *testing.T) {
	ctx := deriveSearchContext("CISG article 79 exemption case law")
	if ctx.Type != SearchLegal {
		t.Fatalf("type = %q, want legal", ctx.Type)
	}
	if ctx.Instrument != "CISG" {
		t.Fatalf("instrument = %q", ctx.Instrument)
	}
	if ctx.Provision != "Article 79" {
		t.Fatalf("provision = %q", ctx.Provision)
	}
	gw := newSearchGateway()
	chain := gw.routeBackends(ctx)
	if chain[0] != bingName {
		t.Fatalf("legal chain = %v, want HTML engine first", chain)
	}
	bing := gw.backends[bingName].Adapt(ctx)
	if len(bing.Queries) < 2 {
		t.Fatalf("Bing legal plan = %v, want broad + authoritative variants", bing.Queries)
	}
	if !containsString(bing.Queries[0], `"CISG"`) || !containsString(bing.Queries[0], `"Article 79"`) {
		t.Fatalf("Bing broad query = %q", bing.Queries[0])
	}
	if !containsString(bing.Queries[1], "site:uncitral.un.org") {
		t.Fatalf("Bing authoritative query = %q", bing.Queries[1])
	}
	academic := gw.backends[arxivName].Adapt(ctx).Queries
	if len(academic) == 0 || containsString(academic[0], "site:") {
		t.Fatalf("arXiv plan must not carry Bing site: syntax: %v", academic)
	}
	if !containsString(academic[0], "Article 79") {
		t.Fatalf("arXiv plan must target the provision: %v", academic)
	}
	crossref := gw.backends[crossrefName].Adapt(ctx).Queries
	if !containsString(crossref[0], "Article 79") {
		t.Fatalf("Crossref plan = %v", crossref)
	}
}

// TestUNCLOSLegalRouting checks a treaty/provision query selects the official-
// source strategy for the right instrument.
func TestUNCLOSLegalRouting(t *testing.T) {
	ctx := deriveSearchContext("UNCLOS article 76 continental shelf")
	if ctx.Type != SearchLegal {
		t.Fatalf("type = %q, want legal", ctx.Type)
	}
	if ctx.Instrument != "UNCLOS" || ctx.Provision != "Article 76" {
		t.Fatalf("instrument=%q provision=%q", ctx.Instrument, ctx.Provision)
	}
	gw := newSearchGateway()
	bing := gw.backends[bingName].Adapt(ctx)
	if !containsString(bing.Queries[0], `"UNCLOS"`) || !containsString(bing.Queries[0], `"Article 76"`) {
		t.Fatalf("Bing broad query = %q", bing.Queries[0])
	}
	if !containsString(bing.Queries[1], "site:un.org") || containsString(bing.Queries[1], "site:cisg-online.org") {
		t.Fatalf("Bing UNCLOS authoritative query = %q", bing.Queries[1])
	}
}

// TestAcademicQueryAdaptation checks an academic query routes to scholarly
// providers and adapts phrases for paper discovery.
func TestAcademicQueryAdaptation(t *testing.T) {
	ctx := deriveSearchContext("document level legal machine translation")
	if ctx.Type != SearchAcademic {
		t.Fatalf("type = %q, want academic", ctx.Type)
	}
	gw := newSearchGateway()
	chain := gw.routeBackends(ctx)
	if chain[0] != arxivName {
		t.Fatalf("academic chain = %v, want arXiv first", chain)
	}
	arxiv := gw.backends[arxivName].Adapt(ctx)
	if len(arxiv.Queries) < 2 {
		t.Fatalf("arXiv plan = %v, want exact + loose variants", arxiv.Queries)
	}
	if !containsString(arxiv.Queries[0], `"document-level"`) && !containsString(arxiv.Queries[0], "document") {
		t.Fatalf("arXiv exact plan = %v", arxiv.Queries)
	}
	crossref := gw.backends[crossrefName].Adapt(ctx).Queries
	if !containsString(crossref[0], "document-level") && !containsString(crossref[0], "document level") {
		t.Fatalf("Crossref plan = %v", crossref)
	}
	// A general query must not route to scholarly providers.
	general := deriveSearchContext("weather in Hanoi")
	if general.Type != SearchGeneral {
		t.Fatalf("general query misclassified as %q", general.Type)
	}
	genChain := gw.routeBackends(general)
	if genChain[0] != bingName && genChain[0] != ddgName {
		t.Fatalf("general chain = %v", genChain)
	}
	for _, name := range genChain {
		if name == arxivName || name == crossrefName {
			t.Fatalf("general chain must exclude scholarly backends: %v", genChain)
		}
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
