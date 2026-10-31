package main

import (
	"encoding/json"
	"html"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// modelRow renders one switch row; dead marks a model the upstream refused.
func modelRow(id, name string, hidden, dead bool) string {
	checked, badge := " checked", ""
	if hidden {
		checked = ""
	}
	if dead {
		badge = `<span class=badge title="upstream reports no available function">upstream error</span>`
	}
	return modelRowHTML(id, name, checked, badge)
}

func modelRowHTML(id, name, checked, badge string) string {
	return `<div class=row data-id="` + html.EscapeString(id) + `">` +
		`<input type=checkbox class=sw` + checked + ` value="` + html.EscapeString(id) + `">` +
		`<span class=name>` + html.EscapeString(name) + badge + `</span>` +
		`<span class=id>` + html.EscapeString(id) + `</span>` +
		`<button type=button class=del-btn title="Remove from list" onclick="deleteModel(event,'` + html.EscapeString(id) + `')">×</button>` +
		`</div>`
}

// adminHTML renders one page per ?page= (dashboard | models | endpoints |
// chromes) — one function per page.
func adminHTML(page string, catalog []*cliproxy.ModelInfo, claude []gatewayModel) string {
	if page == "automode" {
		page = "config"
	}
	browser := currentChromeAdmin()
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		if m != nil {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)

	providers := prefs.listProviders()

	// --- Models page: every model (NVIDIA + custom) ---
	type renderModel struct {
		Publisher string
		Name      string
		ID        string
		IsCustom  bool
		Index     int
		BaseURL   string
	}
	var renderList []renderModel

	for i, p := range providers {
		renderList = append(renderList, renderModel{
			Publisher: p.Name,
			Name:      p.alias(),
			ID:        p.modelID(),
			IsCustom:  true,
			Index:     i,
			BaseURL:   p.BaseURL,
		})
	}

	for _, id := range ids {
		if prefs.isDeleted(id) {
			continue
		}
		publisher, name, _ := strings.Cut(id, "/")
		renderList = append(renderList, renderModel{
			Publisher: publisher,
			Name:      name,
			ID:        id,
			IsCustom:  false,
		})
	}

	sort.SliceStable(renderList, func(i, j int) bool {
		if renderList[i].Publisher != renderList[j].Publisher {
			return renderList[i].Publisher < renderList[j].Publisher
		}
		return renderList[i].Name < renderList[j].Name
	})

	var rows strings.Builder
	failed := 0
	switch page {
	case "dashboard":
		rows.WriteString(`<h2>overview (all APIs, since server start)</h2><div id=charts></div>
<h2>request log (200-request frames, kept across restarts)</h2><div id=frames></div><div id=framebox></div>`)
	case "chromes":
		if browser != nil {
			rows.WriteString(`<h2>captcha chromes — kill to free RAM</h2><div id=chromes></div>`)
		} else {
			rows.WriteString(`<div class=row><span class=id>captcha Chrome is starting or unavailable</span></div>`)
		}
	case "config", "automode":
		rows.WriteString(renderConfigPage(ids, providers))
	default: // models + endpoints share one page
		rows.WriteString(`<h2>add new endpoint</h2>` + providerForm)
		group := ""
		for _, rm := range renderList {
			if rm.Publisher != group {
				group = rm.Publisher
				rows.WriteString(`<h2>` + html.EscapeString(rm.Publisher) + `</h2>`)
			}
			checked, badge := " checked", ""
			if prefs.isHidden(rm.ID) {
				checked = ""
			}
			if rm.IsCustom {
				rows.WriteString(`<div class="row" data-id="` + html.EscapeString(rm.ID) + `">` +
					`<input type=checkbox class=sw` + checked + ` value="` + html.EscapeString(rm.ID) + `">` +
					`<span class=name title="` + html.EscapeString(rm.BaseURL) + `">` + html.EscapeString(rm.Name) + `</span>` +
					`<span class=id>` + html.EscapeString(rm.ID) + `</span>` +
					`<button type=button class=del-btn title="Delete endpoint" onclick="deleteProvider(event, ` + strconv.Itoa(rm.Index) + `)">×</button>` +
					`</div>`)
			} else {
				_, isDead := deadModels.Load(rm.ID)
				if isDead {
					failed++
					badge = `<span class=badge title="upstream reports no available function">upstream error</span>`
				}
				rows.WriteString(modelRowHTML(rm.ID, rm.Name, checked, badge))
			}
		}
	}

	if providers == nil {
		providers = []customProvider{}
	}
	blob, err := json.Marshal(providers)
	if err != nil {
		blob = []byte("[]")
	}

	deletedList := prefs.listDeleted()
	deletedBlob, err := json.Marshal(deletedList)
	if err != nil {
		deletedBlob = []byte("[]")
	}

	return adminHead(page, browser != nil || chromeAdminConfigured()) + `<main>` + rows.String() + `</main>
<div id=toast></div>
<script>
const PAGE = ` + strconv.Quote(page) + `;
const FAILED = ` + strconv.Itoa(failed) + `;
const PROVIDERS = ` + string(blob) + `;
const DELETED = ` + string(deletedBlob) + `;
const CHROMES = ` + strconv.FormatBool(browser != nil) + `;
` + adminScript
}
