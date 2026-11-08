package main

// Local model manager: a single page at /admin to switch playground models on and
// off. Every toggle saves immediately to the mask file, so the choice survives a
// gateway restart.

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

var deadModels sync.Map // model id -> struct{}

var adminStatsCache struct {
	sync.Mutex
	at   time.Time
	data gin.H
}

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
	mu            sync.RWMutex
	changeMu      sync.Mutex // serializes mutations, persistence, and reload callbacks
	path          string
	hidden        map[string]struct{}
	deleted       map[string]struct{}
	providers     []customProvider
	providerStore *gatewayprovider.Store
	loadErr       error // refuses mutation after an unreadable or unsupported models file
	autoModel     string
	searchURL     string
	cycle         *cycleConfig
	cycleIndex    int
	onChange      func([]customProvider) // rewrites config.yaml so cliproxy reloads
	onMaskChange  func()                 // triggers re-registration of models in cliproxy
	resolver      *routing.RegistryResolver
}

type prefsFile struct {
	Meta          map[string]any             `json:"_meta,omitempty"`
	Groups        []ModelGroup               `json:"groups"`
	MasksID       []string                   `json:"masks_id"`
	DeletedID     []string                   `json:"deleted_id"`
	Providers     []customProvider           `json:"providers,omitempty"` // kept for admin UI custom providers
	ProvidersV2   []gatewayprovider.Provider `json:"providers_v2"`
	SchemaVersion int                        `json:"schema_version"`
	AutoModel     string                     `json:"auto_model,omitempty"`
	SearchURL     string                     `json:"search_url,omitempty"`
	Cycle         *cycleConfig               `json:"cycle,omitempty"`
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
	// ResponsesOnly marks an upstream that expects the OpenAI Responses API
	// (`/v1/responses`) and must receive the request without chat translation.
	ResponsesOnly bool `json:"responses_only,omitempty"`
}

func (p *customProvider) alias() string {
	if p.Alias != "" {
		return p.Alias
	}
	prefix := p.Name + "/"
	if strings.HasPrefix(p.Model, prefix) {
		return p.Model
	}
	return prefix + p.Model
}

func (p *customProvider) modelID() string {
	return p.alias()
}

// load reads preferences and migrates legacy providers in memory. It never
// overwrites a malformed or future-schema file; the caller must handle errors.
func (p *modelPrefs) load(path string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = path
	p.hidden, p.deleted = map[string]struct{}{}, map[string]struct{}{}
	p.providers, p.providerStore, p.loadErr = nil, nil, nil
	p.autoModel, p.searchURL, p.cycle, p.cycleIndex = "", "", &cycleConfig{Name: "cycle"}, 0
	store, err := gatewayprovider.OpenStore(path, nil)
	if err != nil {
		p.loadErr = err
		return err
	}
	doc := store.Snapshot()
	file, err := decodePrefsDocument(doc)
	if err != nil {
		p.loadErr = err
		return err
	}
	if err := validateLegacyProviderURLs(fromLegacyProviders(gatewayprovider.LegacyView(file.ProvidersV2))); err != nil {
		p.loadErr = err
		return err
	}
	p.providerStore = store
	p.applyPrefsLocked(&file)
	refreshProviderSecrets(p.providers, file.ProvidersV2...)
	if err := p.refreshRoutingLocked(); err != nil {
		p.loadErr = err
		return err
	}
	log.Printf("models file %s: %d model(s) off, %d deleted, %d custom endpoint(s)", path, len(p.hidden), len(p.deleted), len(p.providers))
	// Merge catalog entries from the local file into the embedded registry.
	// This preserves newly embedded models while still honoring a refreshed
	// per-machine catalog generated by the scraper.
	for _, group := range file.Groups {
		for _, entry := range group.Models {
			if entry.Model == "" || entry.Slug == "" || entry.FunctionID == "" {
				continue
			}
			existing := models.Models[entry.Model]
			ns := entry.Namespace
			if ns == "" {
				ns = models.Namespace
			}
			models.Models[entry.Model] = models.ModelInfo{
				Slug:       entry.Slug,
				Namespace:  ns,
				FunctionID: entry.FunctionID,
				Capability: existing.Capability,
			}
		}
	}
	return nil
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
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error {
		file.MasksID = append([]string(nil), ids...)
		return nil
	}, false)
}

// addDeleted adds a single model to the deleted set.
func (p *modelPrefs) addDeleted(id string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error {
		for _, existing := range file.DeletedID {
			if existing == id {
				return nil
			}
		}
		file.DeletedID = append(file.DeletedID, id)
		return nil
	}, false)
}

// removeDeleted restores a model from the deleted set.
func (p *modelPrefs) removeDeleted(id string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error {
		var retained []string
		for _, existing := range file.DeletedID {
			if existing != id {
				retained = append(retained, existing)
			}
		}
		file.DeletedID = retained
		return nil
	}, false)
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

func (p *modelPrefs) autoModelID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.autoModel != "" {
		return p.autoModel
	}
	return models.DefaultModel
}

func (p *modelPrefs) setAutoModel(id string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error { file.AutoModel = id; return nil }, false)
}

func (p *modelPrefs) searchTemplate() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.searchURL
}

func (p *modelPrefs) setSearchURL(raw string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error { file.SearchURL = raw; return nil }, false)
}

func validSearchURL(raw string) bool {
	if !strings.Contains(raw, "{query}") {
		return false
	}
	u, err := url.Parse(strings.ReplaceAll(raw, "{query}", "test"))
	return err == nil && publicSearchHost(u) && u.Fragment == ""
}

func normalizeSearchURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "{query}") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || !publicSearchHost(u) {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "scholar.google.com":
		u.Path = "/scholar"
	case strings.HasPrefix(host, "google.") || strings.Contains(host, ".google."):
		u.Path = "/search"
	case host == "bing.com" || strings.HasSuffix(host, ".bing.com"):
		u.Path = "/search"
	case host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com"):
		u.Path = "/html/"
	}
	q := u.Query()
	q.Set("q", "{query}")
	u.RawQuery = strings.ReplaceAll(q.Encode(), "%7Bquery%7D", "{query}")
	return u.String()
}

// setProviders replaces the custom endpoints and asks the gateway to reload them.
func (p *modelPrefs) setProviders(list []customProvider) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	if err := validateLegacyProviderURLs(list); err != nil {
		return err
	}
	return p.commitPrefs(func(file *prefsFile) error {
		var err error
		file.ProvidersV2, err = gatewayprovider.MergeLegacy(file.ProvidersV2, toLegacyProviders(list))
		return err
	}, true)
}

// save persists a defensive candidate, then publishes and notifies.
func (p *modelPrefs) save() error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(*prefsFile) error { return nil }, false)
}

// compatProviders generates CLIProxyAPI routes only for enabled openai-chat
// providers. Messages and Responses endpoints remain local passthroughs.
func compatProviders(list []gatewayprovider.Provider) []config.OpenAICompatibility {
	var out []config.OpenAICompatibility
	for i := range list {
		p := &list[i]
		if !p.Enabled || p.Protocol != ir.ProtocolOpenAIChat {
			continue
		}
		entry := config.OpenAICompatibility{Name: p.ID, BaseURL: p.BaseURL, Headers: gatewayprovider.Clone(p).Headers}
		key := p.Auth.Key
		if p.Auth.KeyEnv != "" {
			key = os.Getenv(p.Auth.KeyEnv)
		}
		if key != "" {
			switch p.Auth.Scheme {
			case gatewayprovider.AuthBearer:
				entry.APIKeyEntries = []config.OpenAICompatibilityAPIKey{{APIKey: key}}
			case gatewayprovider.AuthAPIKey:
				if entry.Headers == nil {
					entry.Headers = map[string]string{}
				}
				entry.Headers["X-Api-Key"] = key
			}
		}
		for _, m := range p.Models {
			if !m.Hidden {
				entry.Models = append(entry.Models, config.OpenAICompatibilityModel{Name: m.UpstreamModel, Alias: m.Alias, DisplayName: m.Alias})
			}
		}
		out = append(out, entry)
	}
	return out
}

// adminRoutes serves the manager page and its actions.
func adminRoutes(c *gin.Context, catalog []*cliproxy.ModelInfo) {
	switch c.Request.URL.Path {
	case "/admin":
		handleAdminDashboard(c, catalog)
	case "/admin/stats":
		handleAdminStats(c)
	case "/admin/models":
		handleAdminModels(c)
	case "/admin/automode":
		handleAdminAutomode(c)
	case "/admin/search":
		handleAdminSearch(c)
	case "/admin/cycle":
		handleAdminCycle(c)
	case "/admin/providers":
		handleAdminProviders(c)
	case "/admin/delete":
		handleAdminDelete(c)
	case "/admin/retry-failed":
		handleAdminRetryFailed(c)
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown admin path"})
	}
}
