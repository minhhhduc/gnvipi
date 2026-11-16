package main

import (
	"net/url"
	"strings"
)

// configuredPublicURL resolves the optional public gateway URL from
// GNVIPI_PUBLIC_URL or GNVIPI_DOMAIN for display in Admin Console API snippets.
// Returns "" when neither is set so the UI falls back to window.location.origin.
func configuredPublicURL(lookup func(string) (string, bool)) string {
	for _, key := range []string{"GNVIPI_PUBLIC_URL", "GNVIPI_DOMAIN"} {
		raw, ok := lookup(key)
		if !ok {
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			scheme := "https://"
			host := strings.ToLower(strings.SplitN(raw, ":", 2)[0])
			if host == "localhost" || host == "127.0.0.1" || strings.HasPrefix(host, "[::1]") {
				scheme = "http://"
			}
			raw = scheme + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
	}
	return ""
}
