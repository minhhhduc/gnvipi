package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/html"
)

type searchRoundTrip func(*http.Request) (*http.Response, error)

func (f searchRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestScholarSearchSkipsNavigation(t *testing.T) {
	markup := `<html><a href="https://accounts.google.com/Login">Đăng nhập</a>
	<div class="gs_r"><a href="https://aclanthology.org/2025.acl-long.683.pdf">[PDF] aclanthology.org</a>
	<h3 class="gs_rt"><a href="https://aclanthology.org/2025.acl-long.683/">LexCLiPR: Cross-lingual paragraph retrieval from legal judgments</a></h3>
	<a href="https://scholar.googleusercontent.com/scholar?q=cache:abc">Xem dạng HTML</a></div></html>`
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	page, err := url.Parse("https://scholar.google.com/scholar?q=HUDOC")
	if err != nil {
		t.Fatal(err)
	}
	results := parseSearchResults(doc, page)
	if len(results) != 1 || results[0].Title != "LexCLiPR: Cross-lingual paragraph retrieval from legal judgments" || results[0].URL != "https://aclanthology.org/2025.acl-long.683/" {
		t.Fatalf("Scholar results: %#v", results)
	}
}

func TestDuckDuckGoRedirectUsesDestination(t *testing.T) {
	page, err := url.Parse("https://www.duckduckgo.com/html/?q=Hamburg")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(`<a href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Funcitral.un.org%2Fen%2Ftexts%2Ftransportgoods%2Fconventions%2Fhamburg_rules">United Nations Convention on the Carriage of Goods by Sea</a>`))
	if err != nil {
		t.Fatal(err)
	}
	results := parseSearchResults(doc, page)
	if len(results) != 1 || results[0].URL != "https://uncitral.un.org/en/texts/transportgoods/conventions/hamburg_rules" {
		t.Fatalf("DuckDuckGo results: %#v", results)
	}
}

func TestSearchConfigPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	p := &modelPrefs{path: path}
	searchURL := "https://scholar.google.com/scholar?q={query}"
	if err := p.setSearchURL(searchURL); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if got := loaded.searchTemplate(); got != searchURL {
		t.Fatalf("loaded search URL = %q", got)
	}
}

func TestSearchHTTPJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v2/search", searchHTTP)

	oldPrefs := prefs
	defer func() { prefs = oldPrefs }()
	prefs = &modelPrefs{}

	for _, body := range []string{"not json", `{"query":"  "}`} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2/search", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, w.Code)
		}
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2/search", strings.NewReader(`{"query":"colab"}`)))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unconfigured search status = %d, want 502", w.Code)
	}

	prefs = &modelPrefs{searchURL: "https://www.bing.com/search?q={query}"}
	oldClient := searchClient
	defer func() { searchClient = oldClient }()
	searchClient = &http.Client{Transport: searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `<html><li class=b_algo><h2><a href="https://example.com/guide">Example guide</a></h2></li></html>`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	gw := searchGatewayInstance
	defer gw.setTestTransport(nil)
	gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `<html><li class=b_algo><h2><a href="https://example.com/guide">Example guide</a></h2></li></html>`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	}))

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2/search", strings.NewReader(`{"query":"colab mcp"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Results []struct{ Title, URL string } `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) == 0 || got.Results[0].URL != "https://example.com/guide" {
		t.Fatalf("results = %+v", got.Results)
	}
}

func TestGatewaySearchMCP(t *testing.T) {
	if got := normalizeSearchURL("https://www.google.com"); got != "https://www.google.com/search?q={query}" {
		t.Fatalf("Google URL = %q", got)
	}
	if got := normalizeSearchURL("https://scholar.google.com"); got != "https://scholar.google.com/scholar?q={query}" {
		t.Fatalf("Google Scholar URL = %q", got)
	}
	if validSearchURL("https://localhost/search?q={query}") {
		t.Fatal("accepted a local search endpoint")
	}
	oldPrefs, oldClient := prefs, searchClient
	defer func() { prefs, searchClient = oldPrefs, oldClient }()
	prefs = &modelPrefs{searchURL: "https://www.bing.com/search?q={query}"}
	searchClient = &http.Client{Transport: searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.Query().Get("q"); got != "colab mcp" {
			t.Errorf("query = %q", got)
		}
		body := `<html><li class=b_algo><h2><a href="https://example.com/guide">Example MCP guide</a></h2><cite>https://example.com/guide</cite></li></html>`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	gw := searchGatewayInstance
	defer gw.setTestTransport(nil)
	gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Host == "www.bing.com" || r.Host == "html.duckduckgo.com" {
			body := `<html><li class=b_algo><h2><a href="https://example.com/guide">Example MCP guide</a></h2><cite>https://example.com/guide</cite></li></html>`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"results":[]}`)), Header: make(http.Header), Request: r}, nil
	}))
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_web","arguments":{"query":"colab mcp"}}}`))
	searchMCP(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "https://example.com/guide") {
		t.Fatalf("MCP search response: %d %s", w.Code, w.Body.String())
	}
}

func TestConfiguredSearchTemplatesAreUsed(t *testing.T) {
	oldPrefs, oldGW := prefs, searchGatewayInstance
	defer func() {
		prefs = oldPrefs
		searchGatewayInstance = oldGW
	}()

	cases := []struct {
		name      string
		template  string
		wantHost  string
		wantParam string
		wantVal   string
		htmlBody  string
		wantURL   string
	}{
		{
			name:     "google",
			template: "https://www.google.com/search?q={query}",
			wantHost: "www.google.com",
			htmlBody: `<html><a href="https://docs.example.org/google-hit">Google Hit Title</a></html>`,
			wantURL:  "https://docs.example.org/google-hit",
		},
		{
			name:     "scholar",
			template: "https://scholar.google.com/scholar?q={query}",
			wantHost: "scholar.google.com",
			htmlBody: `<html><h3><a href="https://aclanthology.org/paper1">Scholar Paper Title</a></h3></html>`,
			wantURL:  "https://aclanthology.org/paper1",
		},
		{
			name:      "custom with extra query param",
			template:  "https://custom.example.org/search?q={query}&lang=vi",
			wantHost:  "custom.example.org",
			wantParam: "lang",
			wantVal:   "vi",
			htmlBody:  `<html><a href="https://target.example.org/vi-doc">Custom Search Result</a></html>`,
			wantURL:   "https://target.example.org/vi-doc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newSearchGateway()
			// Start with DDG selected to ensure custom/Google/Scholar overrides prior selection.
			gw.setHTMLEngine(ddgName)
			searchGatewayInstance = gw

			var seenHosts []string
			var seenParam string
			gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
				seenHosts = append(seenHosts, r.URL.Host)
				if r.URL.Host == tc.wantHost {
					if tc.wantParam != "" {
						seenParam = r.URL.Query().Get(tc.wantParam)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(tc.htmlBody)),
						Header:     make(http.Header),
						Request:    r,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`<html></html>`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}))

			prefs = &modelPrefs{searchURL: tc.template}
			results, err := searchWebRanked("test query")
			if err != nil {
				t.Fatalf("searchWebRanked error: %v", err)
			}
			var matchedHost bool
			for _, h := range seenHosts {
				if h == tc.wantHost {
					matchedHost = true
					break
				}
			}
			if !matchedHost {
				t.Fatalf("expected request to host %q, saw %v", tc.wantHost, seenHosts)
			}
			if tc.wantParam != "" && seenParam != tc.wantVal {
				t.Fatalf("query param %s = %q, want %q", tc.wantParam, seenParam, tc.wantVal)
			}
			var foundResult bool
			for _, r := range results {
				if r.URL == tc.wantURL {
					foundResult = true
					break
				}
			}
			if !foundResult {
				t.Fatalf("expected result URL %q in %+v", tc.wantURL, results)
			}
		})
	}
}

func TestConfiguredSearchRejectsAuthorityPlaceholdersAndPrivateHosts(t *testing.T) {
	rejected := []string{
		"https://{query}/search",
		"https://{query}.example.com/search",
		"https://sub.{query}/search",
		"https://{query}@example.com/search?q={query}",
		"https://example.com:{query}/search",
		"http://example.com/search?q={query}",
		"https://localhost/search?q={query}",
		"https://127.0.0.1/search?q={query}",
	}
	for _, raw := range rejected {
		if validSearchURL(raw) {
			t.Fatalf("validSearchURL(%q) = true, want false", raw)
		}
	}
	accepted := []string{
		"https://example.com/search/{query}",
		"https://example.com:8443/search?q={query}",
	}
	for _, raw := range accepted {
		if !validSearchURL(raw) {
			t.Fatalf("validSearchURL(%q) = false, want true", raw)
		}
	}

	gw := newSearchGateway()
	var roundTripCalled bool
	gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		roundTripCalled = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`<html></html>`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	}))

	for _, name := range []string{customName, bingName, ddgName} {
		for _, tmpl := range []string{"https://{query}/search", "https://127.0.0.1/search?q={query}", "http://example.com/search?q={query}"} {
			roundTripCalled = false
			b := gw.backends[name]
			plan := QueryPlan{Backend: name, Queries: []string{"localhost"}, MaxResults: 5}
			if _, err := b.Search(withSearchTemplate(context.Background(), tmpl), plan); err == nil {
				t.Fatalf("%s.Search(%q) succeeded, want error", name, tmpl)
			}
			if roundTripCalled {
				t.Fatalf("%s.Search(%q) reached RoundTrip", name, tmpl)
			}
		}
	}
}

type mutatingTemplateBackend struct {
	SearchBackend
	onSearch func()
}

func (m *mutatingTemplateBackend) template() string {
	if tb, ok := m.SearchBackend.(interface{ template() string }); ok {
		return tb.template()
	}
	return ""
}

func (m *mutatingTemplateBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	if m.onSearch != nil {
		m.onSearch()
	}
	return m.SearchBackend.Search(ctx, plan)
}

func TestConfiguredSearchImmutableTemplateSnapshot(t *testing.T) {
	gw := newSearchGateway()
	var seenHosts []string
	gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		seenHosts = append(seenHosts, r.URL.Host)
		var body string
		switch r.URL.Host {
		case "first.example.org":
			body = `<html><a href="https://target-first.example.org/hit">First Result</a></html>`
		case "second.example.org":
			body = `<html><a href="https://target-second.example.org/hit">Second Result</a></html>`
		default:
			body = `<html></html>`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	}))

	custom := gw.backends[customName].(*customBackend)
	custom.setTemplate("https://first.example.org/search?q={query}")
	gw.setHTMLEngine(customName)
	gw.backends[customName] = &mutatingTemplateBackend{
		SearchBackend: custom,
		onSearch: func() {
			// Simulate a concurrent config update between cache-key construction and Search execution.
			custom.setTemplate("https://second.example.org/search?q={query}")
			gw.setSearchTemplate("https://second.example.org/search?q={query}")
		},
	}

	results, traces := gw.search(context.Background(), "snapshot query")
	if len(results) == 0 || results[0].URL != "https://target-first.example.org/hit" {
		t.Fatalf("first search results = %+v, traces = %+v", results, traces)
	}
	if len(seenHosts) == 0 || seenHosts[0] != "first.example.org" {
		t.Fatalf("expected initial request to first.example.org, saw %v", seenHosts)
	}
	for _, h := range seenHosts {
		if h == "second.example.org" {
			t.Fatalf("unexpected request to mutated host second.example.org during snapshotted search: %v", seenHosts)
		}
	}

	// Subsequent search with the new template must miss the first template's cache entry.
	gw.backends[customName] = custom
	seenHosts = nil
	results2, traces2 := gw.search(context.Background(), "snapshot query")
	if len(results2) == 0 || results2[0].URL != "https://target-second.example.org/hit" {
		t.Fatalf("second search results = %+v, traces = %+v", results2, traces2)
	}
	if len(seenHosts) == 0 || seenHosts[0] != "second.example.org" {
		t.Fatalf("expected second search to hit second.example.org, saw %v", seenHosts)
	}

	// Re-running with the first template snapshot must hit the first template's cached entry.
	seenHosts = nil
	results3, traces3 := gw.rankedSearch(withSearchTemplate(context.Background(), "https://first.example.org/search?q={query}"), "snapshot query")
	if len(results3) == 0 || results3[0].URL != "https://target-first.example.org/hit" {
		t.Fatalf("third search results = %+v, traces = %+v", results3, traces3)
	}
	if len(traces3) == 0 || !traces3[0].CacheHit {
		t.Fatalf("expected cache hit for first template, traces = %+v", traces3)
	}
	if len(seenHosts) != 0 {
		t.Fatalf("expected no network requests on cache hit, saw %v", seenHosts)
	}
}
