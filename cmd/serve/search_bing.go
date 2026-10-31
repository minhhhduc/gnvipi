package main

import (
	"context"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Bing backend — general web discovery and the strongest HTML engine for legal
// and official sources. Query adaptation turns natural-language queries into
// Bing syntax: quoted phrases for technical terms, an authoritative-domain
// variant for legal instruments, never a blind copy of the raw query.

const bingName = "bing"

type bingBackend struct {
	client *sharedSearchClient
}

func (b *bingBackend) Name() string { return bingName }

func (b *bingBackend) Healthy() bool { return b.client.health.Healthy() }

// Adapt converts intent to Bing query syntax.
//
//   - identifiers are looked up bare (the router sends them to arXiv first)
//   - legal queries get a broad variant plus an authoritative-domain variant
//   - academic queries keep their technical phrases quoted
//   - everything else quotes extracted phrases and keeps plain keywords
func (b *bingBackend) Adapt(ctx SearchContext) QueryPlan {
	plan := QueryPlan{Backend: bingName, MaxResults: 10}

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

	broad := strings.Join(parts, " ")
	if broad == "" {
		broad = ctx.Original
	}
	plan.Queries = append(plan.Queries, broad)

	// Legal queries benefit from restricting to primary sources, but only as a
	// second variant: the broad query must still find niche commentary.
	if ctx.Type == SearchLegal {
		authoritative := b.authoritativeVariant(ctx, broad)
		if authoritative != broad {
			plan.Queries = append(plan.Queries, authoritative)
		}
	}
	return plan
}

// authoritativeVariant narrows a legal query to official instrument domains.
// The domain list is selected by instrument, not applied globally.
func (b *bingBackend) authoritativeVariant(ctx SearchContext, broad string) string {
	domains := legalAuthorityDomains(ctx.Instrument)
	if len(domains) == 0 {
		return broad
	}
	sites := make([]string, len(domains))
	for i, d := range domains {
		sites[i] = "site:" + d
	}
	return broad + " (" + strings.Join(sites, " OR ") + ")"
}

func legalAuthorityDomains(instrument string) []string {
	switch strings.ToUpper(instrument) {
	case "CISG":
		return []string{"uncitral.un.org", "cisg-online.org", "law.pace.edu", "cisg.law.pace.edu"}
	case "UNCLOS":
		return []string{"un.org", "unclos.un.org", "imo.org"}
	case "UNIDROIT":
		return []string{"unidroit.org"}
	case "INCOTERMS":
		return []string{"iccwbo.org"}
	case "GDPR":
		return []string{"eur-lex.europa.eu", "edpb.europa.eu"}
	case "WTO", "GATT":
		return []string{"wto.org"}
	case "WIPO":
		return []string{"wipo.int"}
	case "HAMBURG RULES":
		return []string{"uncitral.un.org"}
	default:
		if instrument == "" {
			return nil
		}
		return []string{"un.org", "eur-lex.europa.eu", "gov"}
	}
}

func (b *bingBackend) Search(ctx context.Context, plan QueryPlan) ([]SearchResult, error) {
	var results []SearchResult
	for _, q := range plan.Queries {
		target := "https://www.bing.com/search?q=" + url.QueryEscape(q) + "&setlang=en-US&cc=US"
		_, body, err := b.client.do(ctx, target, nil)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		parsed, perr := parseBingResults(body)
		if perr != nil && len(results) == 0 && len(plan.Queries) == 1 {
			return nil, perr
		}
		results = append(results, parsed...)
		if len(results) >= plan.MaxResults {
			break
		}
	}
	return dedupeURLs(results), nil
}

// parseBingResults reads Bing's result markup. Bing wraps destination URLs in a
// /ck/a redirect; the real URL is recovered from the visible <cite> text, which
// is what Bing actually shows the user and stays stable across markup churn.
func parseBingResults(body []byte) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, ErrParser
	}
	results := make([]SearchResult, 0, 10)
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if len(results) >= 10 {
			return
		}
		if n.Type == html.ElementNode && n.Data == "li" && hasClass(n, "b_algo") {
			results = append(results, parseBingAlgo(n))
			return // children belong to this result; do not descend
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)
	if len(results) == 0 && looksLikeChallenge(body) {
		return nil, ErrChallenge
	}
	return results, nil
}

func parseBingAlgo(li *html.Node) SearchResult {
	res := SearchResult{Backend: bingName}
	var citation string
	var rawCite string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if res.Title == "" && n.Type == html.ElementNode && n.Data == "h2" {
			res.Title = strings.TrimSpace(nodeText(n))
		}
		if rawCite == "" && n.Type == html.ElementNode && n.Data == "cite" {
			rawCite = citeMarkup(n)
			citation = cleanCite(rawCite)
		}
		if res.Snippet == "" && n.Type == html.ElementNode && (n.Data == "p" || hasClass(n, "b_caption")) {
			res.Snippet = strings.TrimSpace(nodeText(n))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(li)
	if citation == "" {
		// Fall back to the h2 anchor; Bing wraps destinations in a /ck/a
		// redirect that is opaque without the cite, so prefer cite whenever it
		// exists but never return an empty URL when the anchor is direct.
		if href := firstHref(li); href != "" {
			citation = href
		}
	}
	res.URL = citation
	if res.Snippet != "" && len(res.Snippet) > 220 {
		res.Snippet = res.Snippet[:220] + "…"
	}
	return res
}

// citeMarkup renders a <cite> with its emphasis markup intact, because Bing
// splits the host across <strong> tags and nodeText alone loses the join.
func citeMarkup(n *html.Node) string {
	var b strings.Builder
	var write func(*html.Node)
	write = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			write(c)
		}
	}
	write(n)
	return strings.TrimSpace(b.String())
}

func firstHref(n *html.Node) string {
	var href string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if href != "" {
			return
		}
		if node.Type == html.ElementNode && node.Data == "a" {
			for _, a := range node.Attr {
				if a.Key == "href" {
					href = a.Val
					return
				}
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return href
}

// cleanCite turns Bing's highlighted citation text ("<strong>gemini</strong>.google.com")
// into a URL, preferring https.
func cleanCite(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Bing inserts <strong> markers around the matched term; strip them so the
	// host parses as a URL rather than a fragment of markup.
	text = strings.NewReplacer("<strong>", "", "</strong>", "").Replace(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if !strings.Contains(text, "://") {
		text = "https://" + text
	}
	if u, err := url.Parse(text); err == nil && u.Host != "" {
		return u.String()
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}
