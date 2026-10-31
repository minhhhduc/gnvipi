package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Semantic Scholar backend — paper metadata with abstracts and citation counts.
// Anonymous access is permitted but sharply rate limited; the circuit breaker
// opens quickly so an S2 throttle degrades to the next provider rather than
// failing the whole search.

const s2Name = "semantic_scholar"

type s2Backend struct {
	client *sharedSearchClient
}

func (s *s2Backend) Name() string { return s2Name }

func (s *s2Backend) Healthy() bool { return s.client.health.Healthy() }

// Adapt maps intent onto Semantic Scholar's API.
//
//   - an arXiv ID or DOI resolves through /paper/{id}, which is exact
//   - everything else uses /paper/search with the phrase terms
func (s *s2Backend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: s2Name, MaxResults: 8}
	switch {
	case ctx.ArxivID != "":
		plan.Identifier = "arXiv:" + ctx.ArxivID
		plan.Queries = []string{"arXiv:" + ctx.ArxivID}
	case ctx.DOI != "":
		plan.Identifier = "DOI:" + ctx.DOI
		plan.Queries = []string{"DOI:" + ctx.DOI}
	default:
		terms := append([]string{}, ctx.Phrases...)
		terms = append(terms, ctx.Keywords...)
		if ctx.Instrument != "" {
			terms = append(terms, ctx.Instrument)
		}
		if ctx.Provision != "" {
			terms = append(terms, ctx.Provision)
		}
		if len(terms) == 0 {
			terms = []string{ctx.Original}
		}
		plan.Queries = []string{strings.Join(terms[:min(6, len(terms))], " ")}
	}
	return plan
}

func (s *s2Backend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		var target string
		if plan.Identifier != "" {
			target = "https://api.semanticscholar.org/graph/v1/paper/" +
				url.PathEscape(plan.Identifier) +
				"?fields=title,authors,abstract,externalIds,year,citationCount,url,openAccessPdf"
		} else {
			target = "https://api.semanticscholar.org/graph/v1/paper/search?query=" +
				url.QueryEscape(q) + "&limit=" + itoa(plan.MaxResults) +
				"&fields=title,authors,abstract,externalIds,year,citationCount,url,openAccessPdf"
		}
		status, body, err := s.client.do(ctx, target, nil)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		_ = status
		parsed := parseS2Results(body, plan.Identifier != "")
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

type s2SearchResponse struct {
	Total int       `json:"total"`
	Data  []s2Paper `json:"data"`
}

type s2Paper struct {
	PaperID       string `json:"paperId"`
	Title         string `json:"title"`
	Abstract      string `json:"abstract"`
	Year          int    `json:"year"`
	CitationCount int    `json:"citationCount"`
	URL           string `json:"url"`
	OpenAccessPDF struct {
		URL string `json:"url"`
	} `json:"openAccessPdf"`
	ExternalIDs map[string]any `json:"externalIds"`
	Authors     []struct {
		Name string `json:"name"`
	} `json:"authors"`
}

func parseS2Results(body []byte, single bool) []SearchResult {
	if single {
		var p s2Paper
		if err := json.Unmarshal(body, &p); err != nil || p.PaperID == "" {
			return nil
		}
		return []SearchResult{s2ToResult(p)}
	}
	var resp s2SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	results := make([]SearchResult, 0, len(resp.Data))
	for _, p := range resp.Data {
		results = append(results, s2ToResult(p))
	}
	return results
}

func s2ToResult(p s2Paper) SearchResult {
	res := SearchResult{
		Backend: s2Name,
		Title:   p.Title,
		URL:     p.URL,
		DOI:     stringField(p.ExternalIDs, "DOI"),
		ArxivID: stringField(p.ExternalIDs, "ArXiv"),
	}
	if res.URL == "" && res.ArxivID != "" {
		res.URL = "https://arxiv.org/abs/" + res.ArxivID
	}
	for _, a := range p.Authors {
		if a.Name != "" {
			res.Authors = append(res.Authors, a.Name)
		}
	}
	if p.Year > 0 {
		t := time.Date(p.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
		res.PublishedAt = &t
	}
	res.Snippet = strings.TrimSpace(p.Abstract)
	if len(res.Snippet) > 300 {
		res.Snippet = res.Snippet[:300] + "…"
	}
	return res
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return ""
	}
}
