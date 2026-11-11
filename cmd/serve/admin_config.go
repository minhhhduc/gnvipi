package main

import (
	"html"
	"strings"
)

// renderConfigPage lays the three virtual-model settings out as sheets: two
// compact ones side by side (auto model, web search) and the cycle priority
// list full width underneath, because it is the only one that grows.
func renderConfigPage(ids []string, providers []customProvider) string {
	var rows strings.Builder
	rows.WriteString(`<div class=cols><section class=sheet><div class=sheet-head><h2>Auto model</h2></div><div class="settings-card">` +
		`<p>Send <code>model: "automode"</code> and the gateway forwards the request, unchanged, to the model picked here.</p>` +
		`<label for=auto-model>Model requests to <code>automode</code> go to</label><select id=auto-model>`)
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
	rows.WriteString(`</select><p>Current target: <code>` + html.EscapeString(selected) + `</code></p></div></section>`)
	rows.WriteString(`<section class=sheet><div class=sheet-head><h2>Web search</h2></div>` +
		`<form id=search-form class="settings-card">` +
		`<p>Backs the gateway search tool that Claude Code calls. Pick an engine or give a public HTTPS URL; custom sites take <code>q</code> or <code>{query}</code>. Saves immediately and survives restarts.</p>` +
		`<label for=search-provider>Search engine</label><select id=search-provider><option value="">Disabled</option><option value="https://www.google.com/search?q={query}">Google</option><option value="https://scholar.google.com/scholar?q={query}">Google Scholar</option><option value="https://www.bing.com/search?q={query}">Bing</option><option value="https://www.duckduckgo.com/html/?q={query}">DuckDuckGo</option><option value=custom>Custom URL</option></select>` +
		`<label for=search-url>Search URL</label><input id=search-url type=text inputmode=url placeholder="https://example.com/search?q={query}" value="` + html.EscapeString(prefs.searchTemplate()) + `">` +
		`<button type=submit class=primary>Save search</button></form></section></div>`)
	rows.WriteString(renderCycleConfig(ids, providers))
	return rows.String()
}
