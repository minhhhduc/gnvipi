package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The reliability suite drives every backend against an httptest server so the
// policies are exercised without touching live search engines.

type fakeTransport struct {
	status  int
	body    string
	headers map[string]string
	hits    atomic.Int32
	delay   time.Duration
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.hits.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	body := f.body
	status := f.status
	if status == 0 {
		status = 200
	}
	hdr := make(http.Header)
	for k, v := range f.headers {
		hdr.Set(k, v)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: hdr, Request: r}, nil
}

func testGateway(rt http.RoundTripper) *searchGateway {
	gw := newSearchGateway()
	for _, c := range gw.clients {
		c.httpClient.Transport = rt
		c.httpClient.Timeout = 2 * time.Second
	}
	return gw
}

func TestChallengePageIsReportedAsError(t *testing.T) {
	rt := &fakeTransport{status: 200, body: `<html><title>Sorry</title>our systems have detected unusual traffic</html>`}
	gw := testGateway(rt)
	ctx := deriveSearchContext("arxiv 2608.14210")
	err := errors.New("none")
	results, err2 := gw.backends[bingName].Search(context.Background(), gw.backends[bingName].Adapt(ctx))
	_ = results
	if err2 == nil {
		t.Fatalf("challenge page must surface as an error, got results")
	}
	_ = err
	if !errors.Is(err2, ErrChallenge) {
		t.Fatalf("error = %v, want ErrChallenge", err2)
	}
}

func TestBlockedChallengeIsDistinguishedFromEmpty(t *testing.T) {
	rt := &fakeTransport{status: 403, body: "forbidden"}
	gw := testGateway(rt)
	results, err := gw.backends[bingName].Search(context.Background(), gw.backends[bingName].Adapt(deriveSearchContext("test")))
	if results != nil || err == nil {
		t.Fatalf("403 must be an error, not empty results: %v %v", results, err)
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	rt := &fakeTransport{status: 429, headers: map[string]string{"Retry-After": "0"}}
	gw := testGateway(rt)
	gw.clients[bingName].maxRetries = 1
	results, err := gw.backends[bingName].Search(context.Background(), gw.backends[bingName].Adapt(deriveSearchContext("test")))
	if err == nil {
		t.Fatalf("persistent 429 must surface as an error")
	}
	if hits := rt.hits.Load(); hits > 2 {
		t.Fatalf("attempt count = %d, want at most initial + 1 retry", hits)
	}
	_ = results
}

func TestFallbackWhenPrimaryFails(t *testing.T) {
	rt := &fakeTransport{status: 403, body: "forbidden"}
	gw := testGateway(rt)
	results, traces := gw.search(context.Background(), "CISG article 79 exemption case law", 10)
	for _, tr := range traces {
		if tr.Backend == bingName && tr.Status != "error" {
			t.Fatalf("primary should have failed: %+v", tr)
		}
	}
	if len(results) > 0 {
		t.Fatalf("all-backends-down search should return no results, got %d", len(results))
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	rt := &fakeTransport{status: 403, body: "forbidden"}
	gw := testGateway(rt)
	health := gw.clients[bingName].health
	for i := 0; i < 5; i++ {
		health.Record(ErrBlocked)
	}
	if health.Healthy() {
		t.Fatal("circuit should be open after repeated blocks")
	}
	health.Record(nil)
	if !health.Healthy() {
		t.Fatal("a success should close the circuit")
	}
}

func TestCacheAvoidsRepeatRequests(t *testing.T) {
	rt := &fakeTransport{status: 200, body: `<html><li class=b_algo><h2><a href="https://example.com/a">A</a></h2><cite>https://example.com/a</cite></li></html>`}
	gw := testGateway(rt)
	gw.search(context.Background(), "example query", 10)
	gw.search(context.Background(), "example query", 10)
	if hits := rt.hits.Load(); hits < 2 {
		t.Fatalf("expected at least two upstream requests, got %d", hits)
	}
	rt.hits.Store(0)
	gw.search(context.Background(), "example query", 10)
	if hits := rt.hits.Load(); hits > 0 {
		t.Fatalf("cached search must not hit upstream, got %d hits", hits)
	}
}

func TestArxivParseExtractsMetadata(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
<entry><id>http://arxiv.org/abs/2608.14210v1</id><title>Test Paper Title</title>
<summary>Abstract text.</summary><published>2026-08-14T11:39:17Z</published>
<author><name>Author One</name></author><author><name>Author Two</name></author>
<arxiv:doi xmlns:arxiv="http://arxiv.org/schemas/atom">10.1000/test</arxiv:doi></entry>
</feed>`
	results := parseArxivResults([]byte(body))
	if len(results) != 1 {
		t.Fatalf("parsed %d results", len(results))
	}
	r := results[0]
	if r.ArxivID != "2608.14210" || r.Title != "Test Paper Title" {
		t.Fatalf("id/title = %q %q", r.ArxivID, r.Title)
	}
	if len(r.Authors) != 2 || r.Authors[0] != "Author One" {
		t.Fatalf("authors = %v", r.Authors)
	}
	if r.PublishedAt == nil || r.PublishedAt.Year() != 2026 {
		t.Fatalf("published = %v", r.PublishedAt)
	}
	if r.URL != "https://arxiv.org/abs/2608.14210" {
		t.Fatalf("url = %q", r.URL)
	}
}

func TestCrossrefParseExtractsMetadata(t *testing.T) {
	body := `{"message":{"items":[{"DOI":"10.1007/x","title":["A Chapter"],"author":[{"given":"A","family":"B"}],"published":{"date-parts":[[2023,4,1]]},"abstract":"Summary.","URL":"https://example.com/x"}]}}`
	results := parseCrossrefResults([]byte(body), false)
	if len(results) != 1 {
		t.Fatalf("parsed %d results", len(results))
	}
	r := results[0]
	if r.DOI != "10.1007/x" || r.Title != "A Chapter" || r.Authors[0] != "A B" {
		t.Fatalf("result = %+v", r)
	}
	if r.PublishedAt == nil || r.PublishedAt.Month() != time.April {
		t.Fatalf("published = %v", r.PublishedAt)
	}
}

func TestRankingPromotesExactIdentifierAndDemotesNoise(t *testing.T) {
	ctx := deriveSearchContext("arxiv 2608.14210")
	results := []SearchResult{
		{Title: "Generic pagination", URL: "https://legalfundingjournal.com/page/106/"},
		{Title: "Paper", URL: "https://arxiv.org/abs/2608.14210"},
	}
	ranked := rankResults(ctx, results)
	if ranked[0].URL != "https://arxiv.org/abs/2608.14210" {
		t.Fatalf("exact arXiv match must rank first, got %q", ranked[0].URL)
	}
}

func TestLegalRankingPrefersOfficialSources(t *testing.T) {
	ctx := deriveSearchContext("CISG article 79 exemption case law")
	results := []SearchResult{
		{Title: "Some blog about Article 79", URL: "https://example.com/blog", Snippet: "Article 79"},
		{Title: "UNCITRAL digest Article 79 CISG", URL: "https://digitallibrary.un.org/record/123", Snippet: "Article 79"},
	}
	ranked := rankResults(ctx, results)
	if ranked[0].URL != "https://digitallibrary.un.org/record/123" {
		t.Fatalf("UN source must outrank a blog, got %q", ranked[0].URL)
	}
}

func TestBingParserReadsCiteDestination(t *testing.T) {
	body := `<html><ol id=b_results><li class=b_algo><h2><a href="https://www.bing.com/ck/a?p=abc">CISG Article 79</a></h2><p>Exemption case law.</p><cite>https://<strong>cisg-online.org</strong>/se/79</cite></li></ol></html>`
	results, err := parseBingResults([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("parsed %d results", len(results))
	}
	r := results[0]
	if r.URL != "https://cisg-online.org/se/79" {
		t.Fatalf("url = %q", r.URL)
	}
	if r.Title != "CISG Article 79" {
		t.Fatalf("title = %q", r.Title)
	}
	if r.Snippet != "Exemption case law." {
		t.Fatalf("snippet = %q", r.Snippet)
	}
}

func TestDDGParserUnwrapsRedirect(t *testing.T) {
	page, _ := url.Parse("https://html.duckduckgo.com/html/?q=x")
	body := `<html><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Funcitral.un.org%2Fen%2Ftexts%2Fsalegoods%2Fconventions%2Fcisg">CISG</a></html>`
	results := parseDDGResults([]byte(body), page)
	if len(results) != 1 || results[0].URL != "https://uncitral.un.org/en/texts/salegoods/conventions/cisg" {
		t.Fatalf("results = %+v", results)
	}
}

func TestDedupCanonicalizesURLs(t *testing.T) {
	results := dedupeURLs([]SearchResult{
		{Title: "a", URL: "https://www.example.com/path/?utm_source=x"},
		{Title: "b", URL: "https://example.com/path"},
		{Title: "c", URL: "https://example.com/path"},
	})
	if len(results) != 1 {
		t.Fatalf("deduped to %d", len(results))
	}
	if results[0].URL != "https://example.com/path" {
		t.Fatalf("url = %q", results[0].URL)
	}
}
