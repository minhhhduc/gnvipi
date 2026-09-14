package main

// Local model manager: a single page at /admin to switch playground models on and
// off. Every toggle saves immediately to the mask file, so the choice survives a
// gateway restart.

import (
	"encoding/json"
	"html"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"
	"glm52-nvidia/internal/provider/nvidia"
)

var deadModels sync.Map // model id -> struct{}

type gatewayModel struct {
	Type        string `json:"type"`
	Object      string `json:"object"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
}

// prefs is the process-wide model mask, loaded from -models-file at startup.
var prefs = &modelPrefs{}

type modelPrefs struct {
	mu           sync.RWMutex
	path         string
	hidden       map[string]struct{}
	deleted      map[string]struct{}
	providers    []customProvider
	onChange     func([]customProvider) // rewrites config.yaml so cliproxy reloads
	onMaskChange func()                 // triggers re-registration of models in cliproxy
}

type prefsFile struct {
	Meta      map[string]any   `json:"_meta,omitempty"`
	Groups    []ModelGroup     `json:"groups"`
	MasksID   []string         `json:"masks_id"`
	DeletedID []string         `json:"deleted_id"`
	Providers []customProvider `json:"providers,omitempty"` // kept for admin UI custom providers
}

type ModelGroup struct {
	Publisher string      `json:"publisher"`
	Models    []ModelItem `json:"models"`
}

type ModelItem struct {
	Model      string `json:"model"`
	Slug       string `json:"slug"`
	Namespace  string `json:"namespace"`
	FunctionID string `json:"function_id"`
}

// customProvider is an OpenAI-compatible endpoint added from /admin. CLIProxyAPI
// owns the actual calls and the Anthropic<->OpenAI translation.
type customProvider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	Alias   string `json:"alias,omitempty"`
	// MessagesOnly marks an upstream whose WAF blocks the /v1/chat/completions
	// path; requests for its models are forwarded to /v1/messages instead.
	MessagesOnly bool `json:"messages_only,omitempty"`
}

func (p customProvider) alias() string {
	if p.Alias != "" {
		return p.Alias
	}
	prefix := p.Name + "/"
	if strings.HasPrefix(p.Model, prefix) {
		return p.Model
	}
	return prefix + p.Model
}

func (p customProvider) modelID() string {
	return p.alias()
}

// load reads the mask file; a missing file simply means everything is on.
func (p *modelPrefs) load(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path, p.hidden, p.deleted, p.providers = path, map[string]struct{}{}, map[string]struct{}{}, nil
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file prefsFile
	if err = json.Unmarshal(raw, &file); err != nil {
		log.Printf("models file %s: %v (ignored)", path, err)
		return
	}
	for _, id := range file.MasksID {
		p.hidden[id] = struct{}{}
	}
	for _, id := range file.DeletedID {
		p.deleted[id] = struct{}{}
	}
	p.providers = file.Providers
	log.Printf("models file %s: %d model(s) off, %d deleted, %d custom endpoint(s)", path, len(p.hidden), len(p.deleted), len(p.providers))

	if len(file.Groups) > 0 {
		models.Models = make(map[string]models.ModelInfo)
		for _, group := range file.Groups {
			for _, m := range group.Models {
				models.Models[m.Model] = models.ModelInfo{
					Slug:       m.Slug,
					Namespace:  m.Namespace,
					FunctionID: m.FunctionID,
				}
			}
		}
	}
}

func (p *modelPrefs) isHidden(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, ok := p.deleted[id]; ok {
		return true
	}
	_, ok := p.hidden[id]
	return ok
}

func (p *modelPrefs) isDeleted(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.deleted[id]
	return ok
}

// setHidden replaces the mask and writes it back to disk.
func (p *modelPrefs) setHidden(ids []string) error {
	p.mu.Lock()
	p.hidden = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		p.hidden[id] = struct{}{}
	}
	p.mu.Unlock()
	return p.save()
}

// setDeleted replaces the deleted set and writes it back to disk.
func (p *modelPrefs) setDeleted(ids []string) error {
	p.mu.Lock()
	p.deleted = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		p.deleted[id] = struct{}{}
	}
	p.mu.Unlock()
	return p.save()
}

// addDeleted adds a single model to the deleted set.
func (p *modelPrefs) addDeleted(id string) error {
	p.mu.Lock()
	if p.deleted == nil {
		p.deleted = map[string]struct{}{}
	}
	p.deleted[id] = struct{}{}
	// Also hide it so it won't be served.
	if p.hidden == nil {
		p.hidden = map[string]struct{}{}
	}
	p.hidden[id] = struct{}{}
	p.mu.Unlock()
	return p.save()
}

// removeDeleted restores a model from the deleted set.
func (p *modelPrefs) removeDeleted(id string) error {
	p.mu.Lock()
	delete(p.deleted, id)
	p.mu.Unlock()
	return p.save()
}

// listDeleted returns the deleted model ids.
func (p *modelPrefs) listDeleted() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.deleted))
	for id := range p.deleted {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// listProviders returns the custom endpoints added from /admin.
func (p *modelPrefs) listProviders() []customProvider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]customProvider(nil), p.providers...)
}

// setProviders replaces the custom endpoints and asks the gateway to reload them.
func (p *modelPrefs) setProviders(list []customProvider) error {
	p.mu.Lock()
	p.providers = list
	notify := p.onChange
	onMask := p.onMaskChange
	p.mu.Unlock()
	if err := p.save(); err != nil {
		return err
	}
	if notify != nil {
		notify(list)
	}
	if onMask != nil {
		onMask() // refresh /v1/models and Claude Code's model cache
	}
	return nil
}

// save writes the whole mask file: hidden ids, deleted ids, plus custom endpoints.
func (p *modelPrefs) save() error {
	p.mu.RLock()
	path := p.path
	hiddenIDs := make([]string, 0, len(p.hidden))
	for id := range p.hidden {
		hiddenIDs = append(hiddenIDs, id)
	}
	deletedIDs := make([]string, 0, len(p.deleted))
	for id := range p.deleted {
		deletedIDs = append(deletedIDs, id)
	}

	// Reconstruct groups from models.Models
	groupMap := make(map[string][]ModelItem)
	for id, m := range models.Models {
		publisher, _, _ := strings.Cut(id, "/")
		if publisher == "" {
			publisher = "unknown"
		}
		groupMap[publisher] = append(groupMap[publisher], ModelItem{
			Model:      id,
			Slug:       m.Slug,
			Namespace:  m.Namespace,
			FunctionID: m.FunctionID,
		})
	}

	var groups []ModelGroup
	for pub, list := range groupMap {
		sort.Slice(list, func(i, j int) bool {
			return list[i].Model < list[j].Model
		})
		groups = append(groups, ModelGroup{
			Publisher: pub,
			Models:    list,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Publisher < groups[j].Publisher
	})

	file := prefsFile{
		Meta: map[string]any{
			"count":      len(models.Models),
			"namespaces": []string{models.Namespace},
		},
		Groups:    groups,
		MasksID:   hiddenIDs,
		DeletedID: deletedIDs,
		Providers: append([]customProvider(nil), p.providers...),
	}
	p.mu.RUnlock()
	if path == "" {
		if notify := p.onMaskChange; notify != nil {
			notify()
		}
		return nil
	}
	sort.Strings(file.MasksID)
	sort.Strings(file.DeletedID)
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	err = os.WriteFile(path, append(out, '\n'), 0o600)
	if err == nil {
		if notify := p.onMaskChange; notify != nil {
			notify()
		}
	}
	return err
}

// compatProviders converts the saved endpoints into CLIProxyAPI provider
// config. Endpoints sharing a name are merged into one provider entry so none
// of their models shadow another.
func compatProviders(list []customProvider) []config.OpenAICompatibility {
	type merged struct {
		baseURL string
		apiKey  string
		models  []*customProvider
	}
	order := make([]*merged, 0, len(list))
	byName := make(map[string]*merged, len(list))
	for i := range list {
		p := &list[i]
		m, ok := byName[p.Name]
		if !ok {
			m = &merged{baseURL: p.BaseURL, apiKey: p.APIKey}
			byName[p.Name] = m
			order = append(order, m)
		}
		m.models = append(m.models, p)
	}

	out := make([]config.OpenAICompatibility, 0, len(order))
	for _, m := range order {
		entry := config.OpenAICompatibility{
			Name:    m.models[0].Name,
			BaseURL: m.baseURL,
		}
		if m.apiKey != "" {
			entry.APIKeyEntries = []config.OpenAICompatibilityAPIKey{{APIKey: m.apiKey}}
		}
		for _, p := range m.models {
			entry.Models = append(entry.Models,
				config.OpenAICompatibilityModel{
					Name:        p.Model,
					Alias:       p.alias(),
					DisplayName: p.alias(),
				},
			)
		}
		out = append(out, entry)
	}
	return out
}

// chromeAdmin is the browser group whose Chrome processes /admin/chromes
// manages. nil when serve runs without -auto.
var chromeAdmin *captcha.BrowserGroup

// adminRoutes serves the manager page and its actions.
func adminRoutes(c *gin.Context, catalog []*cliproxy.ModelInfo, claude func() []gatewayModel) {
	// c.Abort() was incorrectly added here
	page := c.DefaultQuery("page", "dashboard")
	switch c.Request.URL.Path {
	case "/admin":
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(adminHTML(page, catalog, claude())))
	case "/admin/chromes":
		if chromeAdmin == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "serve không chạy -auto (không có chrome nào)"})
			return
		}
		switch c.Request.Method {
		case http.MethodGet:
			c.JSON(http.StatusOK, gin.H{"chromes": chromeAdmin.Snapshot()})
		case http.MethodPost:
			var payload struct {
				Index int    `json:"index"`
				Mode  string `json:"mode"` // "pause" | "resume"
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			switch payload.Mode {
			case "pause":
				if err := chromeAdmin.Pause(payload.Index); err != nil {
					c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
					return
				}
				c.JSON(http.StatusOK, gin.H{"paused": payload.Index})
			case "resume":
				if err := chromeAdmin.Resume(payload.Index); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				c.JSON(http.StatusOK, gin.H{"resumed": payload.Index})
			default:
				c.JSON(http.StatusBadRequest, gin.H{"error": "mode phải là pause hoặc resume"})
			}
		default:
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method không hỗ trợ"})
		}
	case "/admin/stats":
		if n, err := strconv.Atoi(c.Query("frames")); err == nil && n >= 1 {
			ev, tf, err := nvidia.ReadFrame(n)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"frame": n, "total_frames": tf, "events": ev})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"models":       nvidia.GlobalStats.Snapshot(),
			"series":       nvidia.GlobalStats.Series(60),
			"events":       nvidia.GlobalStats.Events(200),
			"started_at":   nvidia.GlobalStats.StartedAt(),
			"total_frames": nvidia.FramesCount(),
		})
	case "/admin/models":
		var payload prefsFile
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := prefs.setHidden(payload.MasksID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"off": len(payload.MasksID)})
	case "/admin/providers":
		var list []customProvider
		if err := c.ShouldBindJSON(&list); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		for i, p := range list {
			if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.BaseURL) == "" ||
				strings.TrimSpace(p.Model) == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint " + strconv.Itoa(i+1) +
					": cần name, base_url và model"})
				return
			}
		}
		if err := prefs.setProviders(list); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"providers": len(list)})
	case "/admin/delete":
		var payload struct {
			ID      string `json:"id"`
			Restore bool   `json:"restore"`
		}
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(payload.ID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cần id"})
			return
		}
		if payload.Restore {
			if err := prefs.removeDeleted(payload.ID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"restored": payload.ID})
		} else {
			if err := prefs.addDeleted(payload.ID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"deleted": payload.ID})
		}
	case "/admin/retry-failed":
		deadModels.Clear()
		c.JSON(http.StatusOK, gin.H{"ok": true})
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown admin path"})
	}
}

// modelRow renders one switch row; dead marks a model the upstream refused.
func modelRow(id, name string, hidden, dead bool) string {
	checked, badge := " checked", ""
	if hidden {
		checked = ""
	}
	if dead {
		badge = `<span class=badge title="upstream báo không có function khả dụng">lỗi upstream</span>`
	}
	return modelRowHTML(id, name, checked, badge)
}

func modelRowHTML(id, name, checked, badge string) string {
	return `<div class=row data-id="` + html.EscapeString(id) + `">` +
		`<input type=checkbox class=sw` + checked + ` value="` + html.EscapeString(id) + `">` +
		`<span class=name>` + html.EscapeString(name) + badge + `</span>` +
		`<span class=id>` + html.EscapeString(id) + `</span>` +
		`<button type=button class=del-btn title="Xóa khỏi danh sách" onclick="deleteModel(event,'` + html.EscapeString(id) + `')">×</button>` +
		`</div>`
}

// adminHTML renders one page per ?page= (dashboard | models | endpoints |
// chromes) — mỗi trang một chức năng.
func adminHTML(page string, catalog []*cliproxy.ModelInfo, claude []gatewayModel) string {
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		if m != nil {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)

	providers := prefs.listProviders()

	// --- Trang models: danh sách mọi model (NVIDIA + custom) ---
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
		rows.WriteString(`<h2>tổng quan (mọi api, tính từ lúc bật server)</h2><div id=charts></div>
<h2>log yêu cầu gần đây</h2><div id=log></div>
<h2>thống kê theo model</h2><div id=stats></div>
<h2>log request (khung 200 request, giữ lại qua restart)</h2><div id=frames></div><div id=framebox></div>`)
	case "chromes":
		if chromeAdmin != nil {
			rows.WriteString(`<h2>chrome captcha</h2><div id=chromes></div>`)
		} else {
			rows.WriteString(`<div class=row><span class=id>serve không chạy -auto (không có chrome nào)</span></div>`)
		}
	default: // models + endpoints chung một trang
		rows.WriteString(`<h2>thêm endpoint mới</h2>` + providerForm)
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
					`<button type=button class=del-btn title="Xóa endpoint" onclick="deleteProvider(event, ` + strconv.Itoa(rm.Index) + `)">×</button>` +
					`</div>`)
			} else {
				_, isDead := deadModels.Load(rm.ID)
				if isDead {
					failed++
					badge = `<span class=badge title="upstream báo không có function khả dụng">lỗi upstream</span>`
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

	return adminHead(page, chromeAdmin != nil) + `<main>` + rows.String() + `</main>
<div id=toast></div>
<script>
const PAGE = ` + strconv.Quote(page) + `;
const FAILED = ` + strconv.Itoa(failed) + `;
const PROVIDERS = ` + string(blob) + `;
const DELETED = ` + string(deletedBlob) + `;
const CHROMES = ` + strconv.FormatBool(chromeAdmin != nil) + `;
` + adminScript
}

// adminHead renders the shared shell: nav tabs + CSS. active tab theo page.
// adminHeadTabs renders just the nav tab bar.
func adminHeadTabs(active string, chromes bool) string {
	type tab struct{ id, label string }
	tabs := []tab{{"dashboard", "thống kê"}, {"models", "models"}}
	if chromes {
		tabs = append(tabs, tab{"chromes", "chrome captcha"})
	}
	b := strings.Builder{}
	for _, t := range tabs {
		cls := "tab"
		if t.id == active {
			cls += " on"
		}
		b.WriteString(`<a class="` + cls + `" href="?page=` + t.id + `">` + t.label + `</a>`)
	}
	return b.String()
}

func adminHead(active string, chromes bool) string {
	return `<!doctype html><meta charset="utf-8"><title>Gateway admin</title>
<meta name=viewport content="width=device-width,initial-scale=1">
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#242833;--fg:#e6e8ee;--dim:#8b93a7;--on:#3fb27f;--bad:#e06c6c;--warn:#e0a84c}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 ui-sans-serif,system-ui,"Segoe UI",sans-serif}
header{position:sticky;top:0;z-index:2;padding:1rem 1.25rem;border-bottom:1px solid var(--line);
  background:rgba(15,17,21,.93);backdrop-filter:blur(8px)}
.wrap,main{max-width:60rem;margin:0 auto}
main{padding:1rem 1.25rem 4rem}
h1{margin:0;font-size:1.15rem}
.sub{color:var(--dim);font-size:.82rem;margin-top:.15rem}
.bar{display:flex;gap:.5rem;margin-top:.75rem;flex-wrap:wrap}
input[type=search]{flex:1;min-width:12rem;background:var(--card);border:1px solid var(--line);
  color:var(--fg);border-radius:8px;padding:.45rem .7rem;font:inherit}
button{background:var(--card);border:1px solid var(--line);color:var(--fg);border-radius:8px;
  padding:.45rem .8rem;font:inherit;cursor:pointer}
button:hover{background:#1d212b;border-color:#39405a}
h2{color:var(--dim);font-size:.72rem;text-transform:uppercase;letter-spacing:.09em;
  margin:1.4rem 0 .4rem;font-weight:600}
.row{display:flex;align-items:center;gap:.75rem;padding:.5rem .75rem;margin-bottom:.35rem;
  border:1px solid var(--line);border-radius:10px;background:var(--card);cursor:pointer}
.row:hover{border-color:#333a4a}
.row.hide{display:none}
.name{flex:1;display:flex;align-items:center;gap:.5rem}
.id{color:var(--dim);font-size:.75rem;font-family:ui-monospace,Consolas,monospace}
.badge{color:var(--bad);border:1px solid var(--bad);border-radius:999px;font-size:.66rem;
  padding:0 .4rem;line-height:1.4}
.badge.ok{color:var(--on);border-color:var(--on)}
.badge.warn{color:var(--warn);border-color:var(--warn)}
.sw{appearance:none;flex:none;width:40px;height:22px;border:0;border-radius:999px;background:#2b3040;
  position:relative;cursor:pointer;transition:background .15s}
.sw:checked{background:var(--on)}
.sw::after{content:"";position:absolute;top:3px;left:3px;width:16px;height:16px;border-radius:50%;
  background:#fff;transition:transform .15s}
.sw:checked::after{transform:translateX(18px)}
.row:has(.sw:not(:checked)) .name{color:var(--dim);text-decoration:line-through}
.del-btn{flex:none;background:none;border:1px solid transparent;color:var(--dim);font-size:1.1rem;
  padding:0 .4rem;border-radius:6px;line-height:1;cursor:pointer;opacity:0;transition:opacity .15s,color .15s}
.row:hover .del-btn{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad)}
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));gap:.5rem;
  padding:.75rem;border:1px solid var(--line);border-radius:10px;background:var(--card);margin-bottom:.6rem}
.card input{background:var(--bg);border:1px solid var(--line);color:var(--fg);border-radius:8px;
  padding:.45rem .6rem;font:inherit;min-width:0}
.card button{white-space:nowrap}

#toast{position:fixed;right:1rem;bottom:1rem;padding:.5rem .8rem;border-radius:8px;
  border:1px solid var(--line);background:var(--card);opacity:0;transition:opacity .2s}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(8.5rem,1fr));gap:.5rem;margin-bottom:.6rem}
.tile{border:1px solid var(--line);border-radius:10px;background:var(--card);padding:.7rem .8rem}
.tile .big{font-size:1.35rem;font-weight:600;line-height:1.2}
.tile .lbl{color:var(--dim);font-size:.72rem;margin-bottom:.1rem}
.chartbox{border:1px solid var(--line);border-radius:10px;background:var(--card);padding:.6rem .8rem .5rem;margin-bottom:.6rem}
.note{color:var(--dim);font-size:.72rem;margin:.2rem 0 .5rem}
.pb{border:1px solid;border-radius:999px;font-size:.66rem;padding:0 .4rem;line-height:1.4;white-space:nowrap;margin-right:.4rem}
#frames{display:flex;gap:.3rem;flex-wrap:wrap;margin-bottom:.5rem}
#frames button{padding:.2rem .55rem;font-size:.78rem}
.fp.on{background:#223044;border-color:#39405a}
.logwrap{max-height:22rem;overflow:auto;border:1px solid var(--line);border-radius:10px}
.chhead{display:flex;gap:1rem;margin-bottom:.3rem}
.lg{color:var(--dim);font-size:.72rem;display:inline-flex;align-items:center;gap:.3rem}
.chwrap{position:relative}
.ax{fill:var(--dim);font-size:9px;font-family:ui-monospace,Consolas,monospace}
.tip{position:absolute;pointer-events:none;background:var(--card);border:1px solid var(--line);
  border-radius:8px;padding:.45rem .6rem;min-width:110px;font-size:.72rem;box-shadow:0 4px 14px rgba(0,0,0,.4);z-index:3}
.tip.hidden{display:none}
.tip-h{color:var(--dim);margin-bottom:.25rem;font-family:ui-monospace,Consolas,monospace}
.tip-r{display:flex;justify-content:space-between;gap:1rem;line-height:1.5}
.tip-k{color:var(--dim);display:inline-flex;align-items:center;gap:.35rem}
.tip-sw{width:10px;height:2px;border-radius:1px;display:inline-block}
.tip-r b{font-weight:600}
table.stats{width:100%;border-collapse:collapse;font-size:.8rem;overflow:hidden;border-radius:10px}
table.stats th,table.stats td{padding:.4rem .6rem;border-bottom:1px solid var(--line);text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);background:var(--card);position:sticky;top:0}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" ↑"}
table.stats thead th.on:not(.asc)::after{content:" ↓"}
table.stats td.nm{text-align:left;font-family:ui-monospace,Consolas,monospace;font-size:.75rem}
table.stats tbody tr:hover{background:#1d212b}
table.stats tfoot th{color:var(--dim);background:var(--card)}
#toast.show{opacity:1}
#deleted-section{margin-top:1.5rem;border:1px solid var(--line);border-radius:10px;background:var(--card);overflow:hidden}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.65rem .85rem;cursor:pointer;user-select:none}
#deleted-header:hover{background:#1d212b}
#deleted-header .cnt{color:var(--warn);font-size:.78rem}
#deleted-header .arrow{color:var(--dim);font-size:.7rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.4rem .85rem;border-bottom:1px solid var(--line)}
.del-row:last-child{border-bottom:0}
.del-row .id{flex:1}
.del-row button{border-color:var(--on);color:var(--on);padding:.2rem .55rem;font-size:.8rem}
/* NAV TABS */
	.tab-bar{display:flex;gap:.4rem;padding-top:.5rem;border-top:1px solid var(--line);margin-top:.75rem}
	.tab{color:var(--dim);font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;
	  padding:.3rem .6rem;border-radius:6px;text-decoration:none}
	.tab:hover{color:var(--fg);background:#1d212b}
	.tab.on{color:var(--fg);background:#223044}
	/* hide search on non-models pages */
	#q:not([data-page=models]){display:none}
	</style>
<header><div class=wrap>
  <h1>Gateway admin</h1>
  <div class=tab-bar>` + adminHeadTabs(active, chromes) + `</div>
  <div class=sub id=counts></div>
  <div class=bar>
    <input type=search id=q placeholder="tìm model…" autofocus data-page="models">
    <button id=all>Bật hết</button>
    <button id=none>Tắt hết</button>
    <button id=retry>Thử lại model lỗi</button>
  </div>
</div></header>
`
}

// providerForm is the "add an endpoint" card rendered above the model list.
const providerForm = `<form id=addform class=card>
  <input name=name placeholder="tên (vd: openrouter)" required>
  <input name=base_url placeholder="https://openrouter.ai/api/v1" required>
  <input name=api_key placeholder="api key" type=password>
  <input name=model placeholder="model id bên đó (vd: x-ai/grok-4)" required>
  <label style="display:flex;gap:.4rem;align-items:center;font-size:.8rem;color:var(--dim)"><input type=checkbox name=messages_only>chỉ /v1/messages</label>
  <button type=submit>Thêm endpoint</button>
</form>`

// adminScript keeps the page and the mask file in sync: every toggle saves.
const adminScript = `
const rows = [...document.querySelectorAll('.row')];
const sws = [...document.querySelectorAll('.sw')];
const counts = document.getElementById('counts');
const toastEl = document.getElementById('toast');
let toastTimer;

function flash(msg) {
  toastEl.textContent = msg;
  toastEl.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastEl.classList.remove('show'), 1400);
}

function render() {
  const on = sws.filter(s => s.checked).length;
  counts.textContent = on + ' bật · ' + (sws.length - on) + ' tắt · ' + FAILED + ' lỗi upstream';
}

async function save() {
  render();
  const hidden = sws.filter(s => !s.checked).map(s => s.value);
  try {
    const res = await fetch('/admin/models', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({masks_id: hidden}),
    });
    flash(res.ok ? 'đã lưu' : 'lưu lỗi: ' + res.status);
  } catch (err) {
    flash('lưu lỗi: ' + err);
  }
}

const addForm = document.getElementById('addform');
if (addForm) addForm.addEventListener('submit', async e => {
  e.preventDefault();
  const f = new FormData(e.target);
  const list = (PROVIDERS || []).concat([{
    name: f.get('name').trim(), base_url: f.get('base_url').trim(),
    api_key: f.get('api_key').trim(), model: f.get('model').trim(),
    messages_only: f.get('messages_only') === 'on',
  }]);
  try {
    const res = await fetch('/admin/providers', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(list),
    });
    if (res.ok) {
      flash('đã thêm endpoint');
      setTimeout(() => { location.href = '/admin?page=models'; }, 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('lỗi thêm: ' + err.error);
    }
  } catch (err) {
    flash('lỗi thêm: ' + err);
  }
});

async function deleteProvider(e, idx) {
  e.preventDefault();
  e.stopPropagation();
  const list = (PROVIDERS || []).filter((_, i) => i !== idx);
  try {
    const res = await fetch('/admin/providers', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(list),
    });
    if (res.ok) {
      flash('đã xóa endpoint');
      setTimeout(() => { location.href = '/admin?page=models'; }, 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('lỗi xóa: ' + err.error);
    }
  } catch (err) {
    flash('lỗi xóa: ' + err);
  }
}

// --- Delete model ---
async function deleteModel(e, id) {
  e.preventDefault();
  e.stopPropagation();
  try {
    const res = await fetch('/admin/delete', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({id}),
    });
    if (res.ok) {
      flash('đã xóa: ' + id);
      setTimeout(() => location.reload(), 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('lỗi xóa: ' + err.error);
    }
  } catch (err) {
    flash('lỗi xóa: ' + err);
  }
}

async function restoreModel(id) {
  try {
    const res = await fetch('/admin/delete', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({id, restore: true}),
    });
    if (res.ok) {
      flash('đã khôi phục: ' + id);
      setTimeout(() => location.reload(), 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('lỗi khôi phục: ' + err.error);
    }
  } catch (err) {
    flash('lỗi khôi phục: ' + err);
  }
}

// Deleted models section
function drawDeleted() {
  const deleted = DELETED || [];
  if (deleted.length === 0) return;

  const section = document.createElement('div');
  section.id = 'deleted-section';
  section.className = 'collapsed';

  const header = document.createElement('div');
  header.id = 'deleted-header';
  header.innerHTML = '<span class=arrow>▶</span> <span>Model đã xóa</span> <span class=cnt>' + deleted.length + ' model</span>';
  header.onclick = () => section.classList.toggle('collapsed');

  const list = document.createElement('div');
  list.id = 'deleted-list';
  deleted.forEach(id => {
    const row = document.createElement('div');
    row.className = 'del-row';
    row.innerHTML = '<span class=id>' + id + '</span>';
    const btn = document.createElement('button');
    btn.textContent = 'khôi phục';
    btn.onclick = () => restoreModel(id);
    row.appendChild(btn);
    list.appendChild(row);
  });

  section.appendChild(header);
  section.appendChild(list);
  document.querySelector('main').appendChild(section);
}

drawDeleted();
// Click anywhere on row (except del-btn) toggles the switch.
rows.forEach(r => {
  r.addEventListener('click', e => {
    if (e.target.classList.contains('del-btn')) return;
    if (e.target.classList.contains('sw')) return;
    const sw = r.querySelector('.sw');
    if (sw) { sw.checked = !sw.checked; save(); }
  });
});
sws.forEach(s => s.addEventListener('change', save));
document.getElementById('all').onclick = () => { sws.forEach(s => s.checked = true); save(); };
document.getElementById('none').onclick = () => { sws.forEach(s => s.checked = false); save(); };
document.getElementById('retry').onclick = async () => {
  await fetch('/admin/retry-failed', {method: 'POST'});
  location.reload();
};
document.getElementById('q').addEventListener('input', e => {
  const q = e.target.value.toLowerCase();
  rows.forEach(r => r.classList.toggle('hide', !r.dataset.id.toLowerCase().includes(q)));
});
render();

// Hide search input on non-models pages
if (PAGE !== 'models') {
  document.getElementById('q').style.display = 'none';
  document.getElementById('all').style.display = 'none';
  document.getElementById('none').style.display = 'none';
  document.getElementById('retry').style.display = 'none';
}

// --- Thống kê model: KPI tiles + line charts (crosshair, tooltip) + log
// per-request + bảng sort theo model. Refresh 5s. Palette validated:
// #3987e5/#d95926 trên surface #171a21; badge trạng thái dùng --on/--bad.
if (PAGE === 'dashboard') {
  const box = document.getElementById('stats');
  const logBox = document.getElementById('log');
  const charts = document.getElementById('charts');
  const fmtTime = t => { const d = new Date(t); return d.getFullYear() > 1 ? d.toLocaleTimeString() : '—'; };
  const fmtMS = ms => ms >= 1000 ? (ms / 1000).toFixed(1) + 's' : (ms || 0) + 'ms';
  const fmtN = n => (n || 0).toLocaleString();
  const fmtUp = ms => { if (!(ms > 0)) return '—'; const s = Math.floor(ms / 1000), h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
    return (h ? h + 'g ' : '') + (h || m ? m + 'p ' : '') + (h ? '' : (s % 60) + 's'); };
  const esc = s => String(s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
  // Màu badge provider: chỉ lấy từ palette đã validate / token CSS có sẵn.
  const PCOLS = ['#3987e5', '#d95926', 'var(--on)', 'var(--warn)', 'var(--dim)'];
  function provCell(m) {
    m = m || '';
    const i = m.indexOf('/');
    if (i < 1) return esc(m);
    const p = m.slice(0, i);
    let h = 0; for (let j = 0; j < p.length; j++) h = (h * 31 + p.charCodeAt(j)) >>> 0;
    const c = PCOLS[h % PCOLS.length];
    return '<span class=pb style="color:' + c + ';border-color:' + c + '">' + esc(p) + '</span>' + esc(m.slice(i + 1));
  }
  const C = {req:'#3987e5', in:'#3987e5', out:'#d95926'}; // slot1 blue / slot2 orange
  const COLS = [
    ['model', 'model', 'model được gọi; badge màu = publisher (model playground NVIDIA) hoặc tên endpoint tự thêm ở /admin'],
    ['requests', 'req', 'số request đã gửi qua gateway (kể cả lỗi)'],
    ['errors', 'lỗi', 'upstream trả >=400 hoặc lỗi kết nối'],
    ['streamed', 'stream', 'số request dùng stream (SSE)'],
    ['input_tokens', 'in tok', 'token đầu vào, lấy từ usage của upstream (chỉ tính request thành công)'],
    ['output_tokens', 'out tok', 'token đầu ra, lấy từ usage của upstream'],
    ['avg_ms', 'tb', 'độ trễ trung bình mỗi request (từ lúc nhận tới lúc upstream trả xong)'],
    ['ttfb_avg_ms', 'ttfb tb', 'thời gian chờ token ĐẦU TIÊN, trung bình — chỉ đo được với request stream'],
    ['last_ms', 'last', 'độ trễ lần gọi gần nhất'],
    ['last_ok', 'ok lúc', 'thời điểm request THÀNH CÔNG gần nhất'],
  ];
  const LCOLS = [
    ['time', 'thời gian', 'lúc gateway nhận request'],
    ['model', 'model', 'model được gọi'],
    ['streamed', 'stream', 'request có dùng stream không'],
    ['input_tokens', 'in tok', 'token đầu vào'],
    ['output_tokens', 'out tok', 'token đầu ra'],
    ['ms', 'độ trễ', 'tổng thời gian upstream trả lời'],
    ['ttfb_ms', 'TTFB', 'chờ token đầu tiên (chỉ stream)'],
    ['error', 'trạng thái', 'ok = upstream trả 2xx; lỗi = >=400 hoặc rớt kết nối'],
  ];
  let models = [], series = [], events = [], startedAt = 0;
  let sortKey = 'requests', sortDir = -1, logKey = 'time', logDir = -1;

  // Sparkline cho tile: 12 điểm cuối, line key màu series, nền không vẽ.
  function spark(vals, color) {
    const v = vals.slice(-12), max = Math.max(1, ...v);
    const W = 96, H = 26;
    const x = i => 1 + i * (W - 2) / Math.max(1, v.length - 1);
    const y = t => H - 2 - t * (H - 4) / max;
    const pts = v.map((t, i) => x(i).toFixed(1) + ',' + y(t).toFixed(1)).join(' ');
    return '<svg width=' + W + ' height=' + H + ' viewBox="0 0 ' + W + ' ' + H + '" style="display:block">' +
      '<polyline points="' + pts + '" fill="none" stroke="' + color + '" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>' +
      '<circle cx="' + x(v.length - 1).toFixed(1) + '" cy="' + y(v[v.length - 1]).toFixed(1) + '" r="4" fill="' + color + '" stroke="#171a21" stroke-width="2"/>' +
      '</svg>';
  }

  // Line chart có crosshair + tooltip. defs: [{key,label,color,fmt}].
  // Một trục Y / chart (không dual-axis); tooltip hiển thị đúng series của chart.
  let chartDefs = [];
  function lineChart(defs, data) {
    const W = 640, H = 150, PADL = 38, PADR = 10, PADT = 8, PADB = 20;
    if (!data) data = series;
    const all = defs.map(d => data.map(p => p[d.key] || 0));
    const max = Math.max(1, ...all.flat());
    const tick = max > 4 ? Math.ceil(max / 4) : 1;
    const yMax = Math.max(tick, Math.ceil(max / tick) * tick);
    const x = i => PADL + i * (W - PADL - PADR) / Math.max(1, data.length - 1);
    const y = v => H - PADB - v * (H - PADB - PADT) / yMax;
    let grid = '';
    for (let t = 0; t <= 4; t++) {
      const v = yMax * t / 4, yy = y(v).toFixed(1);
      grid += '<line x1=' + PADL + ' x2=' + (W - PADR) + ' y1=' + yy + ' y2=' + yy + ' stroke="#242833" stroke-width="1"/>' +
        '<text x=' + (PADL - 6) + ' y=' + (yy * 1 + 4) + ' text-anchor="end" class=ax>' + fmtN(Math.round(v)) + '</text>';
    }
    let paths = '', legend = '';
    defs.forEach((d, di) => {
      const vals = data.map(p => p[d.key] || 0);
      const pts = vals.map((v, i) => x(i).toFixed(1) + ',' + y(v).toFixed(1)).join(' ');
      paths += '<path d="M' + x(0).toFixed(1) + ',' + y(vals[0] || 0).toFixed(1) +
        ' L' + pts.replace(/ /g, ' L') + '" fill="none" stroke="' + d.color + '" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>';
      // end-dot với surface ring 2px
      paths += '<circle cx=' + x(vals.length - 1).toFixed(1) + ' cy=' + y(vals[vals.length - 1] || 0).toFixed(1) + ' r=4 fill=' + d.color + ' stroke="#171a21" stroke-width="2"/>';
      legend += '<span class=lg><svg width=14 height=8><line x1=0 y1=4 x2=14 y2=4 stroke=' + d.color + ' stroke-width=2 stroke-linecap=round/></svg>' + d.label + '</span>';
    });
    chartDefs.push(defs);
    return '<div class=chartbox><div class=chhead>' + legend + '</div>' +
      '<div class=chwrap data-chart>' +
      '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" style="width:100%;height:150px;display:block">' +
      grid + paths +
      '<line class=crosshair y1=' + PADT + ' y2=' + (H - PADB) + ' x1=-99 x2=-99 stroke="#8b93a7" stroke-width="1"/>' +
      '</svg><div class=tip hidden></div></div></div>';
  }

  function sortHeader(cols, key, dir, attr) {
    return '<tr>' + cols.map(c => '<th ' + attr + '="' + c[0] + '"' +
      (c[2] ? ' title="' + esc(c[2]) + '"' : '') +
      (c[0] === key ? ' class="on' + (dir > 0 ? ' asc' : '') + '"' : '') + '>' + c[1] + '</th>').join('') + '</tr>';
  }

  function drawCharts() {
    chartDefs = [];
    const tot = k => models.reduce((s, m) => s + (m[k] || 0), 0);
    const last = series[series.length - 1] || {};
    const reqS = series.map(p => p.requests || 0);
    const errPct = tot('requests') ? Math.round(100 * tot('errors') / tot('requests')) : 0;
    const ttfbAvg = tot('streamed')
      ? Math.round(models.reduce((s, m) => s + (m.ttfb_avg_ms || 0) * (m.streamed || 0), 0) / tot('streamed')) : 0;
    const mss = events.map(e => e.ms || 0).sort((a, b) => a - b);
    const p95 = mss.length ? mss[Math.max(0, Math.ceil(0.95 * mss.length) - 1)] : 0;
    const tiles = [
      [fmtN(tot('requests')), 'tổng requests', reqS, C.req],
      [fmtN(last.requests || 0), 'req / phút', reqS, C.req],
      [fmtN(tot('input_tokens')), 'tokens vào', series.map(p => p.input_tokens || 0), C.in],
      [fmtN(tot('output_tokens')), 'tokens ra', series.map(p => p.output_tokens || 0), C.out],
      [fmtN(tot('errors')) + ' (' + errPct + '%)', 'lỗi', series.map(p => p.errors || 0), '#e66767'],
      [ttfbAvg ? fmtMS(ttfbAvg) : '—', 'TTFB tb (stream)', null, null],
      [mss.length ? fmtMS(p95) : '—', 'p95 độ trễ', null, null],
      [fmtUp(startedAt ? Date.now() - startedAt : 0), 'uptime', null, null],
    ];
    charts.innerHTML =
      '<div class=tiles>' + tiles.map(t =>
        '<div class=tile><div class=lbl>' + t[1] + '</div><div class=big>' + t[0] + '</div>' +
        (t[2] ? spark(t[2], t[3]) : '') + '</div>').join('') + '</div>' +
      '<div class=note>tất cả số liệu tính từ lúc bật server' +
        (startedAt ? ' (' + new Date(startedAt).toLocaleString() + ')' : '') +
        ' — mọi api gateway phục vụ: model NVIDIA playground lẫn custom providers đi qua passthrough (tokenharbor, justworker…) đều được tính.</div>' +
      lineChart([{key:'requests', label:'requests / phút', color:C.req}]) +
      lineChart([{key:'input_tokens', label:'tokens vào / phút', color:C.in}, {key:'output_tokens', label:'tokens ra / phút', color:C.out}]) +
      lineChart([{key:'ttfb_avg_ms', label:'TTFB tb / phút', color:C.req, fmt:fmtMS}]) +
      '<div class=note>TTFB = thời gian chờ byte đầu tiên của stream; phút không có request stream nào hiển thị 0.</div>';
    armCharts();
  }

  // Crosshair + tooltip: snap theo X gần nhất, hiện các series của chart đó tại X.
  // Tooltip dùng textContent (giá trị model id là untrusted).
  function armCharts() {
    charts.querySelectorAll('.chwrap').forEach((w, ci) => {
      const svg = w.querySelector('svg'), cross = svg.querySelector('.crosshair'), tip = w.querySelector('.tip');
      const defs = chartDefs[ci] || [];
      const move = e => {
        const r = svg.getBoundingClientRect();
        const px = (e.clientX - r.left) / r.width * 640;
        const i = Math.max(0, Math.min(series.length - 1, Math.round((px - 38) / ((640 - 38 - 10) / Math.max(1, series.length - 1)))));
        const vx = 38 + i * (640 - 38 - 10) / Math.max(1, series.length - 1);
        cross.setAttribute('x1', vx); cross.setAttribute('x2', vx);
        const p = series[i]; if (!p) return;
        tip.textContent = '';
        const head = document.createElement('div');
        head.className = 'tip-h';
        head.textContent = new Date(p.minute).toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
        tip.appendChild(head);
        defs.forEach(d => {
          const row = document.createElement('div'); row.className = 'tip-r';
          const key = document.createElement('span'); key.className = 'tip-k';
          const sw = document.createElement('span'); sw.className = 'tip-sw'; sw.style.background = d.color;
          key.appendChild(sw); key.appendChild(document.createTextNode(d.label));
          const val = document.createElement('b'); val.textContent = (d.fmt || fmtN)(p[d.key] || 0);
          row.appendChild(key); row.appendChild(val); tip.appendChild(row);
        });
        tip.classList.remove('hidden');
        const tx = Math.min(r.width - 130, Math.max(4, e.clientX - r.left + 12));
        tip.style.left = tx + 'px'; tip.style.top = '6px';
      };
      w.addEventListener('pointermove', move);
      w.addEventListener('pointerleave', () => { cross.setAttribute('x1', -99); cross.setAttribute('x2', -99); tip.classList.add('hidden'); });
    });
  }

  // Bảng request: dùng chung cho log live và khung lịch sử.
  function logTable(evs, key, dir) {
    const lval = (e, k) => k === 'time' ? +new Date(e.time)
      : k === 'model' ? (e.model || '')
      : typeof e[k] === 'boolean' ? (e[k] ? 1 : 0) : (e[k] || 0);
    const sorted = evs.slice().sort((a, b) => {
      const va = lval(a, key), vb = lval(b, key);
      return (typeof va === 'string' ? va.localeCompare(vb) : va - vb) * dir;
    });
    return '<div class=logwrap><table class=stats><thead>' + sortHeader(LCOLS, key, dir, 'data-lkey') + '</thead><tbody>' +
      sorted.map(e => '<tr>' +
        '<td>' + fmtTime(e.time) + '</td>' +
        '<td class=nm>' + provCell(e.model) + '</td>' +
        '<td>' + (e.streamed ? 'có' : '—') + '</td>' +
        '<td>' + fmtN(e.input_tokens) + '</td>' +
        '<td>' + fmtN(e.output_tokens) + '</td>' +
        '<td>' + fmtMS(e.ms) + '</td>' +
        '<td>' + (e.ttfb_ms ? fmtMS(e.ttfb_ms) : '—') + '</td>' +
        '<td>' + (e.error ? '<span class=badge>lỗi</span>' : '<span class="badge ok">ok</span>') + '</td>' +
      '</tr>').join('') + '</tbody></table></div>';
  }
  function drawLog() { logBox.innerHTML = logTable(events, logKey, logDir); }

  function draw() {
    if (!models.length) { box.innerHTML = '<div class=row><span class=id>chưa có request nào</span></div>'; return; }
    const sorted = models.slice().sort((a, b) => {
      const va = a[sortKey], vb = b[sortKey];
      return (typeof va === 'string' ? va.localeCompare(vb) : (va || 0) - (vb || 0)) * sortDir;
    });
    const tot = k => models.reduce((s, m) => s + (m[k] || 0), 0);
    box.innerHTML =
      '<div class=note>chỉ hiện model CÓ request; mỗi dòng = 1 model (kể cả api custom); rê chuột lên tiêu đề cột để xem chú giải; bấm tiêu đề để sắp xếp.</div>' +
      '<table class=stats><thead>' + sortHeader(COLS, sortKey, sortDir, 'data-key') + '</thead><tbody>' +
      sorted.map(m => '<tr data-id="' + esc(m.model) + '">' +
        '<td class=nm>' + provCell(m.model) + '</td>' +
        '<td>' + m.requests + '</td>' +
        '<td>' + (m.errors ? '<span class=badge>' + m.errors + '</span>' : '0') + '</td>' +
        '<td>' + (m.streamed || 0) + '</td>' +
        '<td>' + fmtN(m.input_tokens) + '</td>' +
        '<td>' + fmtN(m.output_tokens) + '</td>' +
        '<td>' + fmtMS(m.avg_ms) + '</td>' +
        '<td>' + (m.ttfb_avg_ms ? fmtMS(m.ttfb_avg_ms) : '—') + '</td>' +
        '<td>' + fmtMS(m.last_ms) + '</td>' +
        '<td>' + fmtTime(m.last_ok) + '</td>' +
      '</tr>').join('') +
      '</tbody><tfoot><tr><th>tổng</th><th>' + tot('requests') + '</th><th>' + tot('errors') + '</th><th>' + tot('streamed') +
      '</th><th>' + fmtN(tot('input_tokens')) + '</th><th>' + fmtN(tot('output_tokens')) + '</th><th></th><th></th><th></th><th></th></tr></tfoot></table>';
  }
  async function poll() {
    try {
      const res = await fetch('/admin/stats');
      if (res.ok) {
        const d = await res.json();
        models = d.models || [];
        series = d.series || [];
        events = d.events || [];
        startedAt = d.started_at ? +new Date(d.started_at) : 0;
        drawCharts();
        drawLog();
        draw();
        totalFrames = d.total_frames || 0;
        if (!curFrame && totalFrames) openFrame(totalFrames); else drawFrames(); // poll chỉ cập nhật số chip
      } else { charts.textContent = logBox.textContent = box.textContent = 'lỗi stats: ' + res.status; }
    } catch (err) { charts.textContent = logBox.textContent = box.textContent = 'lỗi stats: ' + err; }
  }
  box.addEventListener('click', e => {
    const th = e.target.closest('th[data-key]');
    if (!th) return;
    if (sortKey === th.dataset.key) { sortDir = -sortDir; } else { sortKey = th.dataset.key; sortDir = -1; }
    draw();
  });
  logBox.addEventListener('click', e => {
    const th = e.target.closest('th[data-lkey]');
    if (!th) return;
    if (logKey === th.dataset.lkey) { logDir = -logDir; } else { logKey = th.dataset.lkey; logDir = -1; }
    drawLog();
  });

  // Khung log lịch sử: #frames = chip 1..total (frame 1 = cũ nhất), >12 thì
  // dùng nút « » dịch cửa sổ 12 chip; nội dung fetch 1 lần/lần bấm, không poll.
  const framesBox = document.getElementById('frames');
  const frameBox = document.getElementById('framebox');
  let totalFrames = 0, curFrame = 0, fKey = 'time', fDir = -1, fEvents = [];
  function drawFrames() {
    if (!totalFrames) { framesBox.innerHTML = ''; return; }
    let lo = 1, hi = totalFrames, nav = '';
    if (totalFrames > 12) {
      lo = Math.floor((curFrame - 1) / 12) * 12 + 1;
      hi = Math.min(totalFrames, lo + 11);
      nav = '<button type=button data-f="prev">«</button>';
    }
    let chips = '';
    for (let f = lo; f <= hi; f++)
      chips += '<button type=button class="fp' + (f === curFrame ? ' on' : '') + '" data-f=' + f + '>' + f + '</button>';
    framesBox.innerHTML = nav + chips + (totalFrames > 12 ? '<button type=button data-f="next">»</button>' : '');
  }
  async function openFrame(n) {
    n = Math.max(1, Math.min(totalFrames, n));
    curFrame = n; drawFrames();
    try {
      const res = await fetch('/admin/stats?frames=' + n);
      const d = await res.json().catch(() => ({}));
      if (!res.ok) { frameBox.textContent = 'lỗi khung: ' + (d.error || res.status); return; }
      totalFrames = d.total_frames || totalFrames; drawFrames();
      fEvents = d.events || [];
      frameBox.innerHTML = logTable(fEvents, fKey, fDir);
    } catch (err) { frameBox.textContent = 'lỗi khung: ' + err; }
  }
  framesBox.addEventListener('click', e => {
    const b = e.target.closest('button[data-f]');
    if (!b) return;
    const f = b.dataset.f;
    openFrame(f === 'prev' ? curFrame - 12 : f === 'next' ? curFrame + 12 : +f);
  });
  frameBox.addEventListener('click', e => {
    const th = e.target.closest('th[data-lkey]');
    if (!th) return;
    if (fKey === th.dataset.lkey) { fDir = -fDir; } else { fKey = th.dataset.lkey; fDir = -1; }
    frameBox.innerHTML = logTable(fEvents, fKey, fDir);
  });
  poll();
  setInterval(poll, 5000);
} // dashboard block end

// --- Chrome captcha (pause = kill process, resume = spawn mới) ---
if (CHROMES && PAGE === 'chromes') {
  const box = document.getElementById('chromes');
  const fmtLast = t => {
    const d = new Date(t);
    return d.getFullYear() > 1 ? d.toLocaleTimeString() : '—';
  };
  async function drawChromes() {
    try {
      const res = await fetch('/admin/chromes');
      if (!res.ok) { box.textContent = 'lỗi chromes: ' + res.status; return; }
      const list = (await res.json()).chromes || [];
      box.innerHTML = list.length ? list.map(c =>
        '<div class=row data-id="chrome' + c.index + '">' +
          '<span class=id>#' + c.index + ' · pid ' + c.pid + '</span>' +
          (c.busy ? '<span class="badge warn">đang bận</span>' : '') +
          (c.paused ? '<span class=badge>tạm dừng</span>' : '<span class="badge ok">running</span>') +
          (c.warmed && !c.paused ? '<span class="badge ok">warm</span>' : '') +
          '<span class=name style="justify-content:flex-end;color:var(--dim);font-size:.78rem">' +
            c.extracts + ' extracts · ok ' + fmtLast(c.last_ok) + '</span>' +
          '<button type=button data-index="' + c.index + '" data-mode="' +
            (c.paused ? 'resume' : 'pause') + '">' +
            (c.paused ? 'Tiếp tục' : 'Tạm dừng') + '</button>' +
        '</div>').join('')
        : '<div class=row><span class=id>không có chrome nào</span></div>';
    } catch (err) {
      box.textContent = 'lỗi chromes: ' + err;
    }
  }
  box.addEventListener('click', async e => {
    const btn = e.target.closest('button[data-mode]');
    if (!btn) return;
    btn.disabled = true;
    try {
      const res = await fetch('/admin/chromes', {
        method: 'POST',
        headers: {'content-type': 'application/json'},
        body: JSON.stringify({index: +btn.dataset.index, mode: btn.dataset.mode}),
      });
      if (res.ok) {
        flash(btn.dataset.mode === 'pause' ? 'đã tạm dừng chrome ' + btn.dataset.index
                                           : 'đã tiếp tục chrome ' + btn.dataset.index);
        drawChromes();
      } else {
        const err = await res.json().catch(() => ({error: res.status}));
        flash('lỗi: ' + err.error);
        btn.disabled = false;
      }
    } catch (err) {
      flash('lỗi: ' + err);
      btn.disabled = false;
    }
  });
  drawChromes();
  setInterval(drawChromes, 5000);
}
</script>`
