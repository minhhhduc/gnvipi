package captcha

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// sitekeyRE matches hCaptcha sitekey in either an attribute (data-sitekey="…")
// or an inline JS literal (sitekey: "…"). The hex pattern is hCaptcha's
// standard UUID-without-dashes form (32 hex chars).
var sitekeyRE = regexp.MustCompile(`(?:data-sitekey\s*=\s*"|sitekey\s*[:=]\s*["'])([0-9a-f]{32})(?:["'])`)

// ScrapeSitekey fetches pageURL and returns the hCaptcha sitekey embedded
// in its HTML. Used at startup to feed the local harness page when
// -captcha-harness=true so we still hit a real hCaptcha verification
// endpoint without loading the full build.nvidia.com playground DOM.
//
// Plain HTTP is fine here — NVIDIA serves the playground HTML with the
// sitekey inlined; no SPA hydration required.
func ScrapeSitekey(ctx context.Context, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("scrape sitekey: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("scrape sitekey: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("scrape sitekey: read body: %w", err)
	}
	m := sitekeyRE.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("scrape sitekey: no hcaptcha sitekey found in %s", pageURL)
	}
	return string(m[1]), nil
}

// ProbePlaygroundAlive reports whether a playground URL still serves HTML.
// Used by cmd/probehard to mark retired models in playground_models.json.
func ProbePlaygroundAlive(ctx context.Context, pageURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false
	}
	// Retired page often serves 200 with a redirect-to-marketing HTML body.
	// Cheap heuristic: presence of hcaptcha sitekey inside the body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "data-hcaptcha-widget-id") || sitekeyRE.Match(body)
}
