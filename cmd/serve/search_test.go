package main

import (
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
	page, _ := url.Parse("https://scholar.google.com/scholar?q=HUDOC")
	results := parseSearchResults(doc, page)
	if len(results) != 1 || results[0].Title != "LexCLiPR: Cross-lingual paragraph retrieval from legal judgments" || results[0].URL != "https://aclanthology.org/2025.acl-long.683/" {
		t.Fatalf("Scholar results: %#v", results)
	}
}

func TestDuckDuckGoRedirectUsesDestination(t *testing.T) {
	page, _ := url.Parse("https://www.duckduckgo.com/html/?q=Hamburg")
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
	url := "https://scholar.google.com/scholar?q={query}"
	if err := p.setSearchURL(url); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	loaded.load(path)
	if got := loaded.searchTemplate(); got != url {
		t.Fatalf("loaded search URL = %q", got)
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
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	gw := searchGatewayInstance
	defer gw.setTestTransport(nil)
	gw.setTestTransport(searchRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Host == "www.bing.com" || r.Host == "html.duckduckgo.com" {
			body := `<html><li class=b_algo><h2><a href="https://example.com/guide">Example MCP guide</a></h2><cite>https://example.com/guide</cite></li></html>`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[]}`)), Header: make(http.Header), Request: r}, nil
	}))
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_web","arguments":{"query":"colab mcp"}}}`))
	searchMCP(c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://example.com/guide") {
		t.Fatalf("MCP search response: %d %s", w.Code, w.Body.String())
	}
}
