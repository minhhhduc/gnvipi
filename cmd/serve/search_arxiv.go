package main

import (
	"context"
	"encoding/xml"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// arXiv backend — the API is used instead of HTML scraping because it is a
// supported interface and returns full metadata. arXiv requests a descriptive
// User-Agent, which we honor.

const arxivName = "arxiv"

type arxivBackend struct {
	client *sharedSearchClient
}

func (a *arxivBackend) Name() string { return arxivName }

func (a *arxivBackend) Healthy() bool { return a.client.health.Healthy() }

// Adapt maps intent onto arXiv's query language.
//
//   - an exact arXiv ID becomes an id: lookup, which is cheaper and exact
//   - a DOI is searched as-is; arXiv indexes DOIs for many records
//   - other academic queries search all: with quoted phrases, and drop
//     web-search words that harm recall on an index of titles/abstracts
func (a *arxivBackend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: arxivName, MaxResults: 8}
	switch {
	case ctx.ArxivID != "":
		plan.Identifier = ctx.ArxivID
		plan.Queries = []string{"id:" + ctx.ArxivID}
	case ctx.DOI != "":
		plan.Queries = []string{"all:" + strconv.Quote(ctx.DOI)}
	default:
		var terms []string
		if ctx.Instrument != "" && ctx.Provision != "" {
			// A provision citation is a precise legal search: quote the whole
			// "CISG Article 79" phrase so arXiv matches it as a unit.
			terms = append(terms, "all:"+strconv.Quote(ctx.Instrument+" "+ctx.Provision))
		} else {
			for _, p := range ctx.Phrases {
				terms = append(terms, "all:"+strconv.Quote(p))
			}
			if ctx.Instrument != "" {
				terms = append(terms, "all:"+ctx.Instrument)
			}
		}
		for _, k := range ctx.Keywords {
			if arxivStopword(k) {
				continue
			}
			terms = append(terms, "all:"+k)
		}
		if len(terms) == 0 {
			terms = []string{"all:" + strconv.Quote(ctx.Original)}
		}
		// Second variant: a looser keyword search that catches works the exact
		// phrase form misses.
		loose := append([]string{}, arxivKeywords(ctx)...)
		if ctx.Instrument != "" && ctx.Provision != "" && !contains(loose, ctx.Provision) {
			loose = append(loose, ctx.Provision)
		}
		looseQuery := "all:" + strings.Join(loose, " ")
		plan.Queries = []string{strings.Join(terms, " AND "), looseQuery}
	}
	return plan
}

// arxivStopword lists words that add noise to a title/abstract index but are
// common in natural-language requests.
func arxivStopword(kw string) bool {
	return stopwordRe.MatchString(kw) || strings.EqualFold(kw, "papers") ||
		strings.EqualFold(kw, "paper")
}

func arxivKeywords(ctx SearchContext) []string {
	out := make([]string, 0, len(ctx.Keywords)+len(ctx.Phrases))
	out = append(out, ctx.Phrases...)
	out = append(out, ctx.Keywords...)
	return out
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func (a *arxivBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		target := "https://export.arxiv.org/api/query?search_query=" + url.QueryEscape(q) +
			"&start=0&max_results=" + strconv.Itoa(plan.MaxResults)
		status, body, err := a.client.do(ctx, target, map[string]string{"User-Agent": "gnvipi-gateway/1.0 (search gateway; mailto:admin@example.com)"})
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		_ = status
		parsed := parseArxivResults(body)
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	ID        string `xml:"id"`
	Title     string `xml:"title"`
	Summary   string `xml:"summary"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
	Authors   []struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Links []struct {
		Href string `xml:"href,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	DOI string `xml:"doi"`
}

func parseArxivResults(body []byte) []SearchResult {
	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil
	}
	results := make([]SearchResult, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		id := extractArxivID(e.ID)
		if id == "" {
			continue
		}
		res := SearchResult{
			Backend: arxivName,
			Title:   strings.Join(strings.Fields(e.Title), " "),
			ArxivID: id,
			Authors: make([]string, 0, len(e.Authors)),
			DOI:     e.DOI,
			Snippet: strings.Join(strings.Fields(e.Summary), " "),
			URL:     "https://arxiv.org/abs/" + id,
		}
		for _, a := range e.Authors {
			res.Authors = append(res.Authors, a.Name)
		}
		if t, err := time.Parse(time.RFC3339, e.Published); err == nil {
			res.PublishedAt = &t
		}
		if len(res.Snippet) > 300 {
			res.Snippet = res.Snippet[:300] + "…"
		}
		results = append(results, res)
	}
	return results
}

var arxivVersionRe = regexp.MustCompile(`^(.*?)(v\d+)?$`)

func extractArxivID(entryURL string) string {
	entryURL = strings.TrimSpace(entryURL)
	if i := strings.LastIndex(entryURL, "/abs/"); i != -1 {
		return stripArxivVersion(entryURL[i+5:])
	}
	if i := strings.LastIndex(entryURL, "/"); i != -1 {
		return stripArxivVersion(entryURL[i+1:])
	}
	return ""
}

// stripArxivVersion drops the vN revision suffix: /abs/2608.14210v1 and
// /abs/2608.14210 are the same record.
func stripArxivIDVersion(id string) string {
	return arxivVersionRe.ReplaceAllString(id, "$1")
}

func stripArxivVersion(id string) string { return stripArxivIDVersion(id) }
