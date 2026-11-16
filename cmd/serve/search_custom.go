package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

const customName = "custom"

// customBackend executes general web searches against a user-configured HTTPS
// search URL template (e.g. Google, Google Scholar, or a custom endpoint).
type customBackend struct {
	client *sharedSearchClient
	mu     sync.RWMutex
	tmpl   string
}

func (c *customBackend) Name() string { return customName }

func (c *customBackend) Healthy() bool { return c.client.health.Healthy() }

func (c *customBackend) setTemplate(tmpl string) {
	c.mu.Lock()
	c.tmpl = tmpl
	c.mu.Unlock()
}

func (c *customBackend) template() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tmpl
}

func (c *customBackend) Adapt(ctx *SearchContext) QueryPlan {
	plan := QueryPlan{Backend: customName, MaxResults: 10}
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

func (c *customBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	tmpl := c.template()
	if !validSearchURL(tmpl) {
		return nil, fmt.Errorf("configure an HTTPS search URL on the config page first")
	}
	var results []SearchResult
	for _, q := range plan.Queries {
		target := expandSearchTemplate(tmpl, q)
		body, err := c.client.do(ctx, target, nil)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		page, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		doc, err := html.Parse(bytes.NewReader(body))
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		for _, item := range parseSearchResults(doc, page) {
			results = append(results, SearchResult{
				Title:   item.Title,
				URL:     item.URL,
				Backend: customName,
			})
		}
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

func expandSearchTemplate(tmpl, query string) string {
	return strings.ReplaceAll(tmpl, "{query}", url.QueryEscape(query))
}
