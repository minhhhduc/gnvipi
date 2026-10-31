package main

import (
	"html"
	"strings"
)

func renderConfigPage(ids []string, providers []customProvider) string {
	var rows strings.Builder
	rows.WriteString(`<h2>Gateway config</h2><div class="card settings-card"><p>Choose <code>automode</code> in Claude. Requests to that name use the gateway model selected here, with the request content preserved.</p><label for=auto-model>Auto model</label><select id=auto-model>`)
	selected := prefs.autoModelID()
	for _, id := range ids {
		if prefs.isHidden(id) {
			continue
		}
		choice := ""
		if id == selected {
			choice = " selected"
		}
		rows.WriteString(`<option value="` + html.EscapeString(id) + `"` + choice + `>` + html.EscapeString(id) + `</option>`)
	}
	for _, p := range providers {
		id := p.modelID()
		if prefs.isHidden(id) {
			continue
		}
		choice := ""
		if id == selected {
			choice = " selected"
		}
		rows.WriteString(`<option value="` + html.EscapeString(id) + `"` + choice + `>` + html.EscapeString(id) + `</option>`)
	}
	choice := ""
	if selected == "cycle" {
		choice = " selected"
	}
	rows.WriteString(`<option value="cycle"` + choice + `>cycle</option>`)
	rows.WriteString(`</select><p>Current target: <code>` + html.EscapeString(selected) + `</code></p></div>`)
	rows.WriteString(renderCycleConfig(ids, providers))
	rows.WriteString(`<h2>Web search</h2><form id=search-form class="card settings-card"><p>Choose an engine or enter a public HTTPS search URL. Changes save automatically and are restored when the gateway restarts. Google, Google Scholar, Bing and DuckDuckGo links work; custom sites use the <code>q</code> parameter or <code>{query}</code> in the URL. Claude Code uses this through the gateway MCP tool.</p><label for=search-provider>Search engine</label><select id=search-provider><option value="">Disabled</option><option value="https://www.google.com/search?q={query}">Google</option><option value="https://scholar.google.com/scholar?q={query}">Google Scholar</option><option value="https://www.bing.com/search?q={query}">Bing</option><option value="https://www.duckduckgo.com/html/?q={query}">DuckDuckGo</option><option value=custom>Custom URL</option></select><label for=search-url>Search URL</label><input id=search-url type=text inputmode=url placeholder="https://example.com/search?q={query}" value="` + html.EscapeString(prefs.searchTemplate()) + `"><button type=submit>Save search</button></form>`)
	return rows.String()
}
