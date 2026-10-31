package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Crossref backend — DOI metadata and scholarly works via the public REST API.
// Crossref requests a descriptive User-Agent in its API guidelines; anonymous
// access is rate limited to roughly 1 request/second, which the shared limiter
// already enforces.

const crossrefName = "crossref"

type crossrefBackend struct {
	client *sharedSearchClient
}

func (c *crossrefBackend) Name() string { return crossrefName }

func (c *crossrefBackend) Healthy() bool { return c.client.health.Healthy() }

// Adapt maps intent onto Crossref's query fields.
//
//   - an exact DOI becomes a metadata lookup rather than a search
//   - phrases go to query.bibliographic, which matches titles and descriptions
//   - keywords are combined for a second, broader variant
func (c *crossrefBackend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: crossrefName, MaxResults: 8}
	switch {
	case ctx.DOI != "":
		plan.Identifier = ctx.DOI
		plan.Queries = []string{ctx.DOI}
	default:
		// Instrument and provision lead: they are the precise terms, so they
		// must never be truncated off the end of a long query.
		var terms []string
		if ctx.Instrument != "" {
			terms = append(terms, ctx.Instrument)
		}
		if ctx.Provision != "" {
			terms = append(terms, ctx.Provision)
		}
		terms = append(terms, ctx.Phrases...)
		for _, k := range ctx.Keywords {
			if len(terms) >= 8 {
				break
			}
			terms = append(terms, k)
		}
		if len(terms) == 0 {
			terms = []string{ctx.Original}
		}
		plan.Queries = []string{strings.Join(terms, " ")}
	}
	return plan
}

func (c *crossrefBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		var target string
		if plan.Identifier != "" {
			target = "https://api.crossref.org/works/" + url.PathEscape(plan.Identifier)
		} else {
			target = "https://api.crossref.org/works?query.bibliographic=" + url.QueryEscape(q) +
				"&rows=" + itoa(plan.MaxResults) +
				"&select=DOI,title,author,published,abstract,URL,type,container-title,is-referenced-by-count"
		}
		status, body, err := c.client.do(ctx, target, map[string]string{
			"User-Agent": "gnvipi-gateway/1.0 (search gateway; mailto:admin@example.com)",
		})
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		_ = status
		parsed := parseCrossrefResults(body, plan.Identifier != "")
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

type crossrefResponse struct {
	Message struct {
		Items []crossrefWork `json:"items"`
	} `json:"message"`
}

type crossrefWork struct {
	DOI            string   `json:"DOI"`
	Title          []string `json:"title"`
	Container      []string `json:"container-title"`
	URL            string   `json:"URL"`
	Type           string   `json:"type"`
	Abstract       string   `json:"abstract"`
	IsReferencedBy int      `json:"is-referenced-by-count"`
	Author         []struct {
		Given  string `json:"given"`
		Family string `json:"family"`
	} `json:"author"`
	Published struct {
		Parts [][]int `json:"date-parts"`
	} `json:"published"`
}

func parseCrossrefResults(body []byte, single bool) []SearchResult {
	if single {
		var work crossrefWork
		if err := json.Unmarshal(body, &work); err != nil || work.DOI == "" {
			return nil
		}
		return []SearchResult{crossrefToResult(work)}
	}
	var resp crossrefResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	results := make([]SearchResult, 0, len(resp.Message.Items))
	for _, w := range resp.Message.Items {
		results = append(results, crossrefToResult(w))
	}
	return results
}

func crossrefToResult(w crossrefWork) SearchResult {
	res := SearchResult{
		Backend: crossrefName,
		Title:   strings.Join(w.Title, " "),
		URL:     w.URL,
		DOI:     w.DOI,
	}
	for _, a := range w.Author {
		name := strings.TrimSpace(a.Given + " " + a.Family)
		if name != "" {
			res.Authors = append(res.Authors, name)
		}
	}
	if len(w.Published.Parts) > 0 && len(w.Published.Parts[0]) >= 3 {
		y, m, d := w.Published.Parts[0][0], time.Month(w.Published.Parts[0][1]), w.Published.Parts[0][2]
		t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		res.PublishedAt = &t
	}
	if w.Abstract != "" {
		res.Snippet = strings.TrimSpace(w.Abstract)
		if len(res.Snippet) > 300 {
			res.Snippet = res.Snippet[:300] + "…"
		}
	}
	return res
}

func itoa(n int) string {
	if n <= 0 {
		return "5"
	}
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
