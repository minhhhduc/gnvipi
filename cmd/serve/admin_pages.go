package main

import (
	"encoding/json"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

func modelRowHTML(id, name, checked, badge string) string {
	return `<div class=row data-id="` + html.EscapeString(id) + `">` +
		`<input type=checkbox class=sw` + checked + ` value="` + html.EscapeString(id) + `">` +
		`<span class=name>` + html.EscapeString(name) + badge + `</span>` +
		`<span class=id>` + html.EscapeString(id) + `</span>` +
		`<button type=button class=del-btn title="Remove from list" onclick="deleteModel(event,'` + html.EscapeString(id) + `')">×</button>` +
		`</div>`
}

// renderAdminModelsPage renders the provider v2 list and editor along with the
// full models catalog.
//
// Order is by how often a row is touched: providers (few, edited by hand),
// then the editor, then the catalog (many, read-only). The editor is collapsed
// because it is the tallest thing on the page and most visits never open it.
func renderAdminModelsPage(ids []string, providers []gatewayprovider.Provider, legacyProviders []customProvider, failed *int) string {
	var rows strings.Builder

	// 1. Upstream Providers List View
	rows.WriteString(`<section class=sheet>` +
		`<div class=sheet-head><h2>Upstream providers</h2>` +
		`<span class=sub>` + strconv.Itoa(len(providers)) + ` configured</span>` +
		`<button type=button id=add-provider-btn class=btn-sm>Add provider</button>` +
		`</div><div id="provider-list">`)
	if len(providers) == 0 {
		rows.WriteString(`<div class="sub sheet-body" id="no-providers-msg">No upstream providers configured yet. Add one to route gateway models to an upstream API.</div>`)
	} else {
		for i := range providers {
			p := &providers[i]
			checked := ""
			if p.Enabled {
				checked = " checked"
			}
			rows.WriteString(`<div class="prov-card" data-provider-id="` + html.EscapeString(p.ID) + `">` +
				`<div class="prov-head">` +
				`<span class="badge pb ok">` + html.EscapeString(string(p.Protocol)) + `</span>` +
				`<strong>` + html.EscapeString(p.Name) + `</strong>` +
				`<span class="chip">` + html.EscapeString(p.ID) + `</span>` +
				`<span class="prov-meta">` + html.EscapeString(p.BaseURL) + `</span>` +
				`<input type="checkbox" class="sw prov-toggle"` + checked + ` data-id="` + html.EscapeString(p.ID) + `" title="Toggle enabled">` +
				`</div>` +
				`<div class="prov-aliases"><span class="alias-lbl">Aliases</span>`)
			if len(p.Models) == 0 {
				rows.WriteString(`<span class="dim" style="font-size:.75rem">none</span>`)
			} else {
				for _, m := range p.Models {
					badgeCls := "badge"
					label := html.EscapeString(m.Alias)
					if m.Hidden {
						badgeCls = "badge warn"
						label += " (hidden)"
					}
					rows.WriteString(`<span class="` + badgeCls + `" title="Upstream: ` + html.EscapeString(m.UpstreamModel) + `">` + label + `</span>`)
				}
			}
			rows.WriteString(`</div>` +
				`<div class="prov-actions">` +
				`<button type="button" class="btn-sm edit-prov-btn" data-id="` + html.EscapeString(p.ID) + `">Edit</button>` +
				`<button type="button" class="btn-sm test-prov-btn" data-id="` + html.EscapeString(p.ID) + `">Test Connection</button>` +
				`<button type="button" class="btn-sm btn-del delete-prov-btn" data-id="` + html.EscapeString(p.ID) + `">Delete</button>` +
				`<span class="test-res" id="test-res-` + html.EscapeString(p.ID) + `"></span>` +
				`</div></div>`)
		}
	}
	rows.WriteString(`</div></section>`)

	// 2. Provider v2 Editor — collapsed until "Add provider" or a row's Edit is
	// used, so the page opens on the lists instead of a tall empty form.
	rows.WriteString(`<details id=editor-panel class=sheet>` +
		`<summary class=sheet-head><span class=sheet-title id="editor-heading">Add upstream provider</span>` +
		`<span class=sub>Aliases, headers, timeouts</span></summary>` +
		`<form id="provider-editor" class="settings-card">` +
		`<input type="hidden" id="p-edit-id" value="">` +
		`<div class="form-grid">` +
		`<div><label for="p-id">Provider ID</label><input id="p-id" name="id" placeholder="e.g. openrouter" required></div>` +
		`<div><label for="p-name">Provider Name</label><input id="p-name" name="name" placeholder="e.g. OpenRouter" required></div>` +
		`<div><label for="p-base-url">Base URL</label><input id="p-base-url" name="base_url" placeholder="https://openrouter.ai/api/v1" required></div>` +
		`<div><label for="p-protocol">Protocol</label>` +
		`<select id="p-protocol" name="protocol">` +
		`<option value="` + string(ir.ProtocolOpenAIChat) + `">OpenAI Chat (` + string(ir.ProtocolOpenAIChat) + `)</option>` +
		`<option value="` + string(ir.ProtocolAnthropicMessages) + `">Anthropic Messages (` + string(ir.ProtocolAnthropicMessages) + `)</option>` +
		`<option value="` + string(ir.ProtocolOpenAIResponses) + `">OpenAI Responses (` + string(ir.ProtocolOpenAIResponses) + `)</option>` +
		`</select></div>` +
		`<div><label for="p-auth-scheme">Auth Scheme</label>` +
		`<select id="p-auth-scheme" name="auth_scheme">` +
		`<option value="` + string(gatewayprovider.AuthBearer) + `">Bearer</option>` +
		`<option value="` + string(gatewayprovider.AuthAPIKey) + `">API Key (` + string(gatewayprovider.AuthAPIKey) + `)</option>` +
		`<option value="api-key">API Key (api-key)</option>` +
		`<option value="` + string(gatewayprovider.AuthNone) + `">None</option>` +
		`</select></div>` +
		`<div><label for="p-auth-key">API Key <span class="dim" id="p-auth-key-hint">(password/write-only)</span></label><input id="p-auth-key" name="key" type="password" placeholder="API key (leave blank to keep existing)"></div>` +
		`<div><label for="p-auth-key-env">KeyEnv (Environment Variable)</label><input id="p-auth-key-env" name="key_env" placeholder="e.g. OPENROUTER_API_KEY"></div>` +
		`</div>` +
		`<div style="margin-top:.75rem">` +
		`<div style="display:flex;justify-content:space-between;align-items:center">` +
		`<label style="font-weight:600;color:var(--fg);font-size:.82rem">Model Mappings</label>` +
		`<button type="button" id="add-model-btn" class="btn-sm">+ Add Model</button>` +
		`</div>` +
		`<div class="sub">Map gateway model alias to upstream model ID. Configure hide/show toggles and fallback chains.</div>` +
		`<div id="model-mappings-list" style="margin-top:.35rem"></div>` +
		`</div>` +
		`<div style="margin-top:.75rem">` +
		`<div style="display:flex;justify-content:space-between;align-items:center">` +
		`<label style="font-weight:600;color:var(--fg);font-size:.82rem">Custom Headers</label>` +
		`<button type="button" id="add-header-btn" class="btn-sm">+ Add Header</button>` +
		`</div>` +
		`<div class="sub">Custom HTTP headers forwarded to the upstream endpoint.</div>` +
		`<div id="custom-headers-list" style="margin-top:.35rem"></div>` +
		`</div>` +
		`<details id="p-advanced" style="margin-top:.75rem">` +
		`<summary style="cursor:pointer;color:var(--dim);font-size:.82rem;font-weight:600">Advanced Settings (Timeouts, Concurrency, TLS)</summary>` +
		`<div style="margin-top:.5rem;padding-left:.5rem;border-left:2px solid var(--line)">` +
		`<div class="form-grid">` +
		`<div><label for="p-max-concurrency">Max Concurrency</label><input type="number" id="p-max-concurrency" min="0" placeholder="0 (unlimited)"></div>` +
		`<div><label for="p-timeout-connect">Connect Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-connect" placeholder="e.g. 10"></div>` +
		`<div><label for="p-timeout-queue">Queue Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-queue" placeholder="e.g. 5"></div>` +
		`<div><label for="p-timeout-idle">Idle Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-idle" placeholder="e.g. 30"></div>` +
		`<div><label for="p-tls">TLS Profile</label><select id="p-tls"><option value="default">Default</option><option value="chrome">Chrome (TLS impersonation)</option></select></div>` +
		`</div></div></details>` +
		`<div class="bar" style="margin-top:.9rem">` +
		`<button type="submit" id="save-provider-btn" class="primary">Save Provider</button>` +
		`<button type="button" id="test-editor-btn">Test Connection</button>` +
		`<button type="button" id="cancel-editor-btn" style="display:none">Cancel</button>` +
		`<span id="editor-test-badge" class="test-res"></span>` +
		`</div></form></details>`)

	// 3. Models catalog
	type renderModel struct {
		Publisher string
		Name      string
		ID        string
		IsCustom  bool
		Index     int
		BaseURL   string
	}
	var renderList []renderModel

	for i, p := range legacyProviders {
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

	// Publisher groups, not one heading per publisher: the catalog is one sheet
	// with sticky-weight dividers, so a 200-model catalog is one scrolling list
	// instead of 20 stacked sections. perPub counts each group once.
	perPub := make(map[string]int, 8)
	for _, rm := range renderList {
		perPub[rm.Publisher]++
	}
	rows.WriteString(`<section class=sheet id=catalog><div class=sheet-head><h2>Model catalog</h2>` +
		`<span class=sub>` + strconv.Itoa(len(renderList)) + ` models · grouped by publisher</span></div>`)
	group := "\x00"
	for _, rm := range renderList {
		if rm.Publisher != group {
			group = rm.Publisher
			rows.WriteString(`<div class=grp data-grp>` + html.EscapeString(rm.Publisher) +
				`<span class=cnt>` + strconv.Itoa(perPub[rm.Publisher]) + `</span></div>`)
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
				if failed != nil {
					*failed++
				}
				badge = `<span class=badge title="upstream reports no available function">upstream error</span>`
			}
			rows.WriteString(modelRowHTML(rm.ID, rm.Name, checked, badge))
		}
	}
	rows.WriteString(`</section>`)
	return rows.String()
}

// adminHTML renders the page named by ?page= (dashboard | models | config |
// chromes). models and endpoints share one page; automode is an alias for
// config; anything else falls through to models.
func adminHTML(page string, catalog []*cliproxy.ModelInfo) string {
	if page == "automode" {
		page = "config"
	}
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		if m != nil {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)

	providers := prefs.listProviders()
	providersV2, err := prefs.listProvidersV2()
	if err != nil {
		providersV2 = nil
	}

	var rows strings.Builder
	failed := 0
	switch page {
	case "dashboard":
		rows.WriteString(`<section class=sec><h2>Traffic</h2><div id=charts></div>
</section>
<section class=sheet><div class=sheet-head><h2>Request log</h2><div id=frames></div></div><div id=framebox></div></section>`)
	case "chromes":
		rows.WriteString(`<section class=sheet><div class=sheet-head><h2>Workers</h2><span class=sub id=chrome-state>checking</span></div><div id=chromes><div class=row static><span class=id>Loading Chrome captcha status…</span></div></div></section>`)
	case "config", "automode":
		rows.WriteString(renderConfigPage(ids, providers))
	default: // models + endpoints share one page
		rows.WriteString(renderAdminModelsPage(ids, providersV2, providers, &failed))
	}

	if providersV2 == nil {
		providersV2 = []gatewayprovider.Provider{}
	}
	redactedV2 := make([]gatewayprovider.Provider, len(providersV2))
	for i := range providersV2 {
		redactedV2[i] = redactProvider(&providersV2[i])
	}
	blobV2, err := json.Marshal(redactedV2)
	if err != nil {
		blobV2 = []byte("[]")
	}

	if providers == nil {
		providers = []customProvider{}
	}
	redactedProviders := make([]customProvider, len(providers))
	for i, p := range providers {
		redactedProviders[i] = p
		redactedProviders[i].APIKey = maskSecret(p.APIKey)
	}
	blob, err := json.Marshal(redactedProviders)
	if err != nil {
		blob = []byte("[]")
	}

	deletedList := prefs.listDeleted()
	deletedBlob, err := json.Marshal(deletedList)
	if err != nil {
		deletedBlob = []byte("[]")
	}

	return adminHead(page) + `<main>` + rows.String() + `</main>
<div id=toast role=status aria-live=polite></div>
<script>
const PAGE = ` + strconv.Quote(page) + `;
const STATS_READ_MS = ` + strconv.FormatInt(int64((nvidia.StatsReadInterval()-1)/time.Millisecond+1), 10) + `;
const FAILED = ` + strconv.Itoa(failed) + `;
const PROVIDERS = ` + string(blob) + `;
const PROVIDERS_V2 = ` + string(blobV2) + `;
const DELETED = ` + string(deletedBlob) + `;
` + adminScript
}
