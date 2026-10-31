package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/html"
)

// maxBytesReader caps a request body; io.LimitReader is not a Reader-on-error,
// so a body larger than the cap is reported as an error rather than truncated.
func maxBytesReader(r io.Reader, n int64) io.Reader { return io.LimitReader(r, n) }

var searchClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if !publicSearchHost(req.URL) {
		return fmt.Errorf("search redirect must use a public HTTPS host")
	}
	if len(via) >= 5 {
		return fmt.Errorf("too many search redirects")
	}
	return nil
}}

// searchGatewayInstance serves the MCP tool. It is created at startup; tests
// swap it to point at httptest servers.
var searchGatewayInstance = newSearchGateway()

func publicSearchHost(u *url.URL) bool {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	return true
}

type searchResult struct{ Title, URL string }

func searchWeb(query string) ([]searchResult, error) {
	results, err := searchWebRanked(query)
	if err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, len(results))
	for _, r := range results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL})
	}
	return out, nil
}

// searchWebRanked runs the adaptive pipeline and returns normalized results.
// The configured search URL still selects the general-web engine (Bing or
// DuckDuckGo); intent selects the academic and legal backends.
func searchWebRanked(query string) ([]SearchResult, error) {
	template := prefs.searchTemplate()
	if !validSearchURL(template) {
		return nil, fmt.Errorf("configure an HTTPS search URL on the config page first")
	}
	gw := searchGatewayInstance
	switch {
	case strings.Contains(template, "bing.com"):
		gw.setHTMLEngine(bingName)
	case strings.Contains(template, "duckduckgo.com"):
		gw.setHTMLEngine(ddgName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, traces := gw.rankedSearch(ctx, query)
	if len(results) == 0 {
		for _, t := range traces {
			if t.Status == "error" && t.Backend != "router" {
				return nil, fmt.Errorf("search backend %s: %s", t.Backend, t.Error)
			}
		}
	}
	return results, nil
}

func parseSearchResults(doc *html.Node, page *url.URL) []searchResult {
	results := make([]searchResult, 0, 8)
	seen := map[string]bool{}
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if len(results) >= 8 {
			return
		}
		// Scholar places paper titles in h3 links; its other links include login,
		// cached HTML and side PDF controls that are not separate results.
		if n.Type == html.ElementNode && n.Data == "a" && (page.Hostname() != "scholar.google.com" || n.Parent != nil && n.Parent.Data == "h3") {
			var href string
			for _, a := range n.Attr {
				if a.Key == "href" {
					href = a.Val
					break
				}
			}
			link, err := page.Parse(href)
			if err == nil && strings.TrimPrefix(link.Hostname(), "www.") == strings.TrimPrefix(page.Hostname(), "www.") {
				redirect := link.Query().Get("uddg")
				if redirect == "" {
					redirect = link.Query().Get("q")
				}
				if redirect != "" {
					link, _ = url.Parse(redirect)
				}
			}
			if link != nil && (link.Scheme == "http" || link.Scheme == "https") && strings.TrimPrefix(link.Hostname(), "www.") != strings.TrimPrefix(page.Hostname(), "www.") && !seen[link.String()] {
				title := strings.Join(strings.Fields(nodeText(n)), " ")
				if len(title) >= 8 {
					results = append(results, searchResult{Title: title, URL: link.String()})
					seen[link.String()] = true
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	return results
}

func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(nodeText(child))
		b.WriteByte(' ')
	}
	return b.String()
}

// searchMCP exposes the configured search page as a stateless Streamable HTTP MCP tool.
func searchMCP(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	if origin := c.Request.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != c.Request.Host {
			c.Status(http.StatusForbidden)
			return
		}
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(maxBytesReader(c.Request.Body, 64<<10)).Decode(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON-RPC request"})
		return
	}
	if len(request.ID) == 0 {
		c.Status(http.StatusAccepted)
		return
	}
	var result any
	switch request.Method {
	case "initialize":
		result = gin.H{"protocolVersion": "2025-06-18", "capabilities": gin.H{"tools": gin.H{}}, "serverInfo": gin.H{"name": "gateway-search", "version": "1.0"}}
	case "ping":
		result = gin.H{}
	case "tools/list":
		result = gin.H{"tools": []any{gin.H{"name": "search_web", "description": "Search the public web using the search engine configured in Gateway admin. Returns page titles and URLs.", "inputSchema": gin.H{"type": "object", "properties": gin.H{"query": gin.H{"type": "string"}}, "required": []string{"query"}}}}}
	case "tools/call":
		var args struct {
			Name      string `json:"name"`
			Arguments struct {
				Query string `json:"query"`
			} `json:"arguments"`
		}
		if json.Unmarshal(request.Params, &args) != nil || args.Name != "search_web" || strings.TrimSpace(args.Arguments.Query) == "" {
			c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": request.ID, "error": gin.H{"code": -32602, "message": "search_web requires a query"}})
			return
		}
		items, err := searchWebRanked(args.Arguments.Query)
		text := "No search results found."
		if err != nil {
			text = err.Error()
		} else if len(items) > 0 {
			var b strings.Builder
			for _, item := range items {
				if item.Snippet != "" {
					fmt.Fprintf(&b, "%s\n%s\n%s\n\n", item.Title, item.URL, item.Snippet)
				} else {
					fmt.Fprintf(&b, "%s\n%s\n\n", item.Title, item.URL)
				}
			}
			text = b.String()
		}
		result = gin.H{"content": []any{gin.H{"type": "text", "text": text}}, "isError": err != nil}
	default:
		c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": request.ID, "error": gin.H{"code": -32601, "message": "method not found"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": request.ID, "result": result})
}
