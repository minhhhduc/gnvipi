package main

import (
	"net/url"
	"strings"
)

// search_url.go — small URL helpers kept out of the backend files so the
// ranking code stays readable.

type nilURL struct{}

func (nilURL) String() string { return "" }

type parsedURL struct {
	inner *url.URL
}

func (p parsedURL) String() string { return p.inner.String() }

var _ interface{ String() string } = parsedURL{}

func parseURL(raw string) (parsedURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return parsedURL{}, err
	}
	return parsedURL{inner: u}, nil
}

// hostOf returns the canonical lowercase host without a leading www.
func hostOf(p parsedURL) string {
	if p.inner == nil {
		return ""
	}
	host := strings.ToLower(p.inner.Hostname())
	return strings.TrimPrefix(host, "www.")
}

func (p parsedURL) Path() string {
	if p.inner == nil {
		return ""
	}
	return p.inner.Path
}
