package main

import (
	"context"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// DuckDuckGo backend — fallback HTML engine. Its html endpoint answers the
// gateway's honest User-Agent, so it works as a second source when Bing is
// blocked, but it returns fewer results and weaker snippets.

const ddgName = "duckduckgo"

type ddgBackend struct {
	client *sharedSearchClient
}

func (d *ddgBackend) Name() string { return ddgName }

func (d *ddgBackend) Healthy() bool { return d.client.health.Healthy() }

// Adapt mirrors Bing's syntax minus the site-restriction variant: DDG applies
// OR-groups inconsistently and drops results, so only the broad query is sent.
func (d *ddgBackend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: ddgName, MaxResults: 8}
	var parts []string
	if ctx.Instrument != "" {
		parts = append(parts, `"`+ctx.Instrument+`"`)
	}
	if ctx.Provision != "" {
		parts = append(parts, `"`+ctx.Provision+`"`)
	}
	for _, p := range ctx.Phrases {
		if !strings.EqualFold(p, ctx.Instrument) && !strings.EqualFold(p, ctx.Provision) {
			parts = append(parts, `"`+p+`"`)
		}
	}
	keywords := ctx.Keywords
	if len(keywords) > 6 {
		keywords = keywords[:6]
	}
	parts = append(parts, keywords...)
	q := strings.Join(parts, " ")
	if q == "" {
		q = ctx.Original
	}
	plan.Queries = []string{q}
	return plan
}

func (d *ddgBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		target := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q)
		_, body, err := d.client.do(ctx, target, nil)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		page, _ := url.Parse(target)
		parsed := parseDDGResults(body, page)
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

// parseDDGResults reads DDG's result__a anchors and unwraps the l/?uddg=
// redirect to the destination URL.
func parseDDGResults(body []byte, page *url.URL) []SearchResult {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	results := make([]SearchResult, 0, 8)
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if len(results) >= 8 {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" && hasClass(n, "result__a") {
			results = append(results, parseDDGAnchor(n, page))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)
	return results
}

func parseDDGAnchor(a *html.Node, page *url.URL) SearchResult {
	res := SearchResult{Backend: ddgName, Title: strings.TrimSpace(nodeText(a))}
	for _, attr := range a.Attr {
		if attr.Key == "href" {
			link, err := page.Parse(attr.Val)
			if err == nil {
				if redirect := link.Query().Get("uddg"); redirect != "" {
					if u, err := url.Parse(redirect); err == nil {
						link = u
					}
				}
				if link.Scheme == "http" || link.Scheme == "https" {
					res.URL = link.String()
				}
			}
			break
		}
	}
	return res
}
