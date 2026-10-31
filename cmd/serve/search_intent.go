package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Derive intent from the raw query using heuristics only: no network, no model,
// no allocation of a classifier call. Keeping this deterministic means the
// search path stays usable when nothing else is up.

var (
	arxivIDRe    = regexp.MustCompile(`(?i)\b(?:arxiv[:\s]?|\b)(\d{4}\.\d{4,5}(?:v\d+)?)\b`)
	doiRe        = regexp.MustCompile(`(?i)\b(10\.\d{4,9}/\S+?)(?:[\s)]|$)`)
	provisionRe  = regexp.MustCompile(`(?i)\b(article|art\.?|section|sec\.?|rule|paragraph|para\.?|regulation|chapter)\s+([ivxl0-9]+(?:[\-.][0-9a-z]+)*)`)
	instrumentRe = regexp.MustCompile(`(?i)\b(CISG|UNCLOS|Vienna Convention|Hamburg Rules|Warsaw Convention|UNIDROIT|Incoterms|GATT|WTO|TRIPS|Berne Convention|Paris Convention|ECOSOC|EU Regulation|GDPR|UPU|NATO|WIPO|ICC)\b`)
	stopwordRe   = regexp.MustCompile(`(?i)^(?:a|an|the|of|in|on|for|and|or|to|with|about|under|into|over|by|at|is|are|was|were|how|what|when|where|which|who|why|can|does|do|did|please|find|search|show|me|my|paper|papers|article|articles|about|related|regarding)$`)
)

// deriveSearchContext classifies a query into an intent. Identifier detection
// wins over academic over legal over general; later types can still carry both
// flags (a legal query is also routed through academic providers as a variant).
func deriveSearchContext(query string) SearchContext {
	q := strings.TrimSpace(query)
	ctx := SearchContext{Original: q, Type: SearchGeneral}
	if q == "" {
		return ctx
	}

	if m := arxivIDRe.FindStringSubmatch(q); m != nil {
		ctx.ArxivID = m[1]
	}
	if m := doiRe.FindStringSubmatch(q); m != nil {
		ctx.DOI = m[1]
	}
	if m := instrumentRe.FindStringSubmatch(q); m != nil {
		ctx.Instrument = strings.ToUpper(m[1])
		if ctx.Instrument == "ART." || ctx.Instrument == "SEC." {
			ctx.Instrument = ""
		}
	}
	if m := provisionRe.FindStringSubmatch(q); m != nil {
		kind := strings.ToLower(m[1])
		num := m[2]
		if kind == "art." || kind == "art" || kind == "article" {
			kind = "Article"
		} else if kind == "sec." || kind == "sec" || kind == "section" {
			kind = "Section"
		} else if kind == "para." || kind == "para" || kind == "paragraph" {
			kind = "Paragraph"
		}
		ctx.Provision = kind + " " + num
	}

	low := strings.ToLower(q)
	ctx.Legal = legalSignals(low) || ctx.Instrument != "" || ctx.Provision != ""
	ctx.Academic = academicSignals(low) || ctx.ArxivID != "" || ctx.DOI != ""

	switch {
	case ctx.ArxivID != "" || ctx.DOI != "":
		ctx.Type = SearchIdentifier
	case ctx.Academic:
		ctx.Type = SearchAcademic
	case ctx.Legal:
		ctx.Type = SearchLegal
	}
	ctx.Keywords, ctx.Phrases = splitTerms(q)
	return ctx
}

func legalSignals(low string) bool {
	for _, s := range []string{
		"case law", "judgment", "judgement", "court", "tribunal", "statute",
		"treaty", "convention", "directive", "regulation", "exemption", "liability",
		"jurisprudence", "precedent", "litigation", "arbitration", "act of ",
		"code of ", "legal", "law", "writ", "summons", "hearing",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

func academicSignals(low string) bool {
	for _, s := range []string{
		"arxiv", "paper", "papers", "study", "studies", "research", "survey",
		"benchmark", "dataset", "model", "neural", "transformer", "training",
		"fine-tun", "machine learning", "deep learning", "nlp", "translation",
		"doi", "conference", "journal", "preprint", "embedding", "retrieval",
		"llm", "evaluation", "semantics", "corpus", "annotation",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// splitTerms separates the query into bare keywords and multi-word phrases that
// are worth quoting for precision. It quotes only adjacent capitalized or
// hyphenated technical terms, so a natural-language question is not turned into
// a wall of exact phrases.
func splitTerms(q string) (keywords, phrases []string) {
	tokens := strings.Fields(q)
	var current []string
	flush := func() {
		if len(current) > 1 {
			phrases = append(phrases, strings.Join(current, " "))
		} else if len(current) == 1 {
			keywords = append(keywords, current[0])
		}
		current = nil
	}
	for _, tok := range tokens {
		clean := strings.Trim(tok, ".,;:!?()[]{}\"'")
		if clean == "" {
			continue
		}
		if isPhraseWorthy(clean) {
			current = append(current, clean)
			continue
		}
		flush()
		keywords = append(keywords, clean)
	}
	flush()
	return keywords, phrases
}

// isPhraseWorthy reports whether a token is part of a technical multi-word term:
// capitalized (but not an all-caps acronym standing alone), or hyphenated.
func isPhraseWorthy(tok string) bool {
	if stopwordRe.MatchString(tok) || isAllCapsAcronym(tok) {
		return false
	}
	letters := false
	for _, r := range tok {
		switch {
		case unicode.IsUpper(r):
			return true
		case unicode.IsLetter(r):
			letters = true
		}
	}
	return !letters && strings.Contains(tok, "-")
}

func isAllCapsAcronym(tok string) bool {
	if len(tok) < 2 || len(tok) > 6 {
		return false
	}
	for _, r := range tok {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
