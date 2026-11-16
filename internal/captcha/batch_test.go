package captcha

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestHarnessPageForValidDOM(t *testing.T) {
	const prefix = "data:text/html;base64,"
	dataURL := HarnessPageFor("test-site-key", 3)
	if !strings.HasPrefix(dataURL, prefix) {
		t.Fatalf("HarnessPageFor() = %q, want %q prefix", dataURL, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, prefix))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}

	var rootDiv *html.Node
	var scripts []*html.Node
	var styles []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "div":
				for _, a := range n.Attr {
					if a.Key == "id" && a.Val == "root" {
						rootDiv = n
					}
				}
			case "script":
				scripts = append(scripts, n)
			case "style":
				styles = append(styles, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if rootDiv == nil {
		t.Fatalf("expected <div id=\"root\"> element in parsed DOM, HTML:\n%s", string(raw))
	}
	if len(scripts) != 1 {
		t.Fatalf("expected 1 <script> element, got %d", len(scripts))
	}
	if scripts[0].FirstChild == nil || !strings.Contains(scripts[0].FirstChild.Data, `window.__SITEKEY = "test-site-key"`) {
		t.Fatalf("expected initialization script body with __SITEKEY, got %#v", scripts[0].FirstChild)
	}
	if len(styles) != 1 || styles[0].FirstChild == nil || !strings.Contains(styles[0].FirstChild.Data, "#root{padding:8px}") {
		t.Fatalf("expected closed CSS rule in <style>, got %#v", styles)
	}
}
