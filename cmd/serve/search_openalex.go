package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// OpenAlex backend — scholarly works catalog. Anonymous access is allowed but
// rate limited; the shared limiter keeps us under the documented ceiling and
// the circuit breaker pauses traffic when OpenAlex asks us to.

const openalexName = "openalex"

type openalexBackend struct {
	client *sharedSearchClient
}

func (o *openalexBackend) Name() string { return openalexName }

func (o *openalexBackend) Healthy() bool { return o.client.health.Healthy() }

// Adapt maps intent onto OpenAlex's search API.
//
//   - a DOI resolves directly via /works/{doi}
//   - an arXiv ID is mapped to its OpenAlex record through the external-id filter
//   - other academic queries use search= with quoted phrases and a looser variant
func (o *openalexBackend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: openalexName, MaxResults: 8}
	switch {
	case ctx.DOI != "":
		plan.Identifier = ctx.DOI
		plan.Queries = []string{ctx.DOI}
	case ctx.ArxivID != "":
		plan.Identifier = "arxiv:" + ctx.ArxivID
		plan.Queries = []string{"arxiv:" + ctx.ArxivID}
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
		primary := strings.Join(terms[:min(6, len(terms))], " ")
		plan.Queries = []string{primary}
	}
	return plan
}

func (o *openalexBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		var target string
		switch {
		case plan.Identifier != "" && strings.HasPrefix(plan.Identifier, "arxiv:"):
			target = "https://api.openalex.org/works?filter=ids.openalex:" +
				url.QueryEscape("https://openalex.org/"+strings.TrimPrefix(plan.Identifier, "arxiv:")) +
				"|ids.mag:" + url.QueryEscape(strings.TrimPrefix(plan.Identifier, "arxiv:"))
		case plan.Identifier != "":
			target = "https://api.openalex.org/works/" + url.PathEscape(plan.Identifier)
		default:
			target = "https://api.openalex.org/works?search=" + url.QueryEscape(q) +
				"&per_page=" + itoa(plan.MaxResults)
		}
		status, body, err := o.client.do(ctx, target, map[string]string{
			"User-Agent": "gnvipi-gateway/1.0 (search gateway; mailto:admin@example.com)",
		})
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		_ = status
		parsed := parseOpenAlexResults(body)
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

type openalexResponse struct {
	Results []openalexWork `json:"results"`
	Error   string         `json:"error"`
}

type openalexWork struct {
	ID                    string           `json:"id"`
	DOI                   string           `json:"doi"`
	Title                 string           `json:"display_name"`
	PublicationYear       int              `json:"publication_year"`
	CitedByCount          int              `json:"cited_by_count"`
	AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
	Authorships           []struct {
		Author struct {
			Name string `json:"display_name"`
		} `json:"author"`
	} `json:"authorships"`
	BestOA struct {
		LandingURL string `json:"landing_page_url"`
		PDFURL     string `json:"pdf_url"`
	} `json:"best_oa_location"`
}

func parseOpenAlexResults(body []byte) []SearchResult {
	var resp openalexResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	results := make([]SearchResult, 0, len(resp.Results))
	for _, w := range resp.Results {
		res := SearchResult{
			Backend: openalexName,
			Title:   w.Title,
		}
		res.URL = openalexCanonical(w)
		res.DOI = strings.TrimPrefix(w.DOI, "https://doi.org/")
		if res.URL == "" {
			res.URL = w.ID
		}
		for _, a := range w.Authorships {
			if a.Author.Name != "" {
				res.Authors = append(res.Authors, a.Author.Name)
			}
		}
		if w.PublicationYear > 0 {
			t := time.Date(w.PublicationYear, time.January, 1, 0, 0, 0, 0, time.UTC)
			res.PublishedAt = &t
		}
		res.Snippet = openalexAbstract(w.AbstractInvertedIndex)
		if len(res.Snippet) > 300 {
			res.Snippet = res.Snippet[:300] + "…"
		}
		results = append(results, res)
	}
	return results
}

func openalexCanonical(w openalexWork) string {
	if w.BestOA.LandingURL != "" {
		return w.BestOA.LandingURL
	}
	if w.BestOA.PDFURL != "" {
		return w.BestOA.PDFURL
	}
	return ""
}

// openalexAbstract reconstructs text from OpenAlex's inverted index.
func openalexAbstract(inverted map[string][]int) string {
	if len(inverted) == 0 {
		return ""
	}
	maxPos := 0
	for _, positions := range inverted {
		for _, p := range positions {
			if p > maxPos {
				maxPos = p
			}
		}
	}
	words := make([]string, maxPos+1)
	for word, positions := range inverted {
		for _, p := range positions {
			if p <= maxPos {
				words[p] = word
			}
		}
	}
	return strings.Join(words, " ")
}
