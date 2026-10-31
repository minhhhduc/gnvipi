package main

import (
	"html"
	"strings"
)

func renderCycleConfig(ids []string, providers []customProvider) string {
	cfg, index := prefs.cycleSnapshot()
	available := make(map[string]bool, len(ids)+len(providers))
	for _, id := range ids {
		if !prefs.isHidden(id) {
			available[id] = true
		}
	}
	for _, p := range providers {
		if id := p.modelID(); !prefs.isHidden(id) {
			available[id] = true
		}
	}
	ordered := make([]string, 0, len(available))
	for _, id := range cfg.Models {
		if available[id] {
			ordered = append(ordered, id)
			delete(available, id)
		}
	}
	for _, id := range ids {
		if available[id] {
			ordered = append(ordered, id)
			delete(available, id)
		}
	}
	for _, p := range providers {
		if id := p.modelID(); available[id] {
			ordered = append(ordered, id)
			delete(available, id)
		}
	}
	var b strings.Builder
	b.WriteString(`<h2>Model cycle</h2><form id=cycle-form class="card settings-card"><p>The <code>cycle</code> model is always available. Select models in priority order; changes save automatically. A 429, missing model, timeout or upstream server error moves to the next model.</p><label>Models in order</label><div id=cycle-list class=cycle-list>`)
	selected := map[string]bool{}
	for _, id := range cfg.Models {
		selected[id] = true
	}
	for _, id := range ordered {
		checked := ""
		if selected[id] {
			checked = " checked"
		}
		b.WriteString(`<div class=cycle-option><label><input type=checkbox value="` + html.EscapeString(id) + `"` + checked + `> ` + html.EscapeString(id) + `</label><span><button type=button data-move=up title="Move up">↑</button><button type=button data-move=down title="Move down">↓</button></span></div>`)
	}
	b.WriteString(`</div>`)
	if len(cfg.Models) > 0 {
		b.WriteString(`<p>Current target: <code>` + html.EscapeString(cfg.Models[index%len(cfg.Models)]) + `</code></p>`)
	} else {
		b.WriteString(`<p>Select a model before sending requests to <code>cycle</code>.</p>`)
	}
	b.WriteString(`<button type=submit>Save cycle</button></form>`)
	return b.String()
}
