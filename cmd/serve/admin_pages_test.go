package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

func TestAdminModelsPage_RenderProviderV2UI(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	catalog := []*cliproxy.ModelInfo{
		{ID: "meta/llama-3.3-70b-instruct"},
		{ID: "mistralai/mistral-large-2"},
	}

	htmlContent := adminHTML("models", catalog)

	// Check Provider List section
	if !strings.Contains(htmlContent, "upstream providers") {
		t.Fatalf("expected 'upstream providers' heading, got:\n%s", htmlContent)
	}
	if !strings.Contains(htmlContent, `id="provider-list"`) {
		t.Fatalf("missing id=\"provider-list\" in HTML")
	}
	if !strings.Contains(htmlContent, `id="no-providers-msg"`) {
		t.Fatalf("missing id=\"no-providers-msg\" placeholder when no providers configured")
	}

	// Check Provider v2 Editor form & elements
	if !strings.Contains(htmlContent, `id="provider-editor"`) {
		t.Fatalf("missing id=\"provider-editor\" form in HTML")
	}
	if !strings.Contains(htmlContent, `id="p-id"`) || !strings.Contains(htmlContent, `id="p-name"`) || !strings.Contains(htmlContent, `id="p-base-url"`) {
		t.Fatalf("missing provider ID, Name, or Base URL inputs in HTML")
	}

	// Protocol dropdown
	if !strings.Contains(htmlContent, `id="p-protocol"`) {
		t.Fatalf("missing id=\"p-protocol\" select in HTML")
	}
	for _, proto := range []string{
		string(ir.ProtocolOpenAIChat),
		string(ir.ProtocolAnthropicMessages),
		string(ir.ProtocolOpenAIResponses),
	} {
		if !strings.Contains(htmlContent, `value="`+proto+`"`) {
			t.Fatalf("missing protocol option %q in HTML", proto)
		}
	}

	// Auth scheme selection
	if !strings.Contains(htmlContent, `id="p-auth-scheme"`) {
		t.Fatalf("missing id=\"p-auth-scheme\" select in HTML")
	}
	for _, scheme := range []string{"bearer", "x-api-key", "none"} {
		if !strings.Contains(htmlContent, `value="`+scheme+`"`) {
			t.Fatalf("missing auth scheme option %q in HTML", scheme)
		}
	}

	// Key and KeyEnv inputs
	if !strings.Contains(htmlContent, `id="p-auth-key"`) {
		t.Fatalf("missing id=\"p-auth-key\" input in HTML")
	}
	if !strings.Contains(htmlContent, `id="p-auth-key-env"`) {
		t.Fatalf("missing id=\"p-auth-key-env\" input in HTML")
	}

	// Model mappings section
	if !strings.Contains(htmlContent, `id="model-mappings-list"`) || !strings.Contains(htmlContent, `id="add-model-btn"`) {
		t.Fatalf("missing model mappings container or add button in HTML")
	}

	// Custom headers section
	if !strings.Contains(htmlContent, `id="custom-headers-list"`) || !strings.Contains(htmlContent, `id="add-header-btn"`) {
		t.Fatalf("missing custom headers container or add button in HTML")
	}

	// Advanced settings: timeouts, concurrency, TLS profile
	if !strings.Contains(htmlContent, `id="p-advanced"`) {
		t.Fatalf("missing id=\"p-advanced\" details in HTML")
	}
	if !strings.Contains(htmlContent, `id="p-max-concurrency"`) {
		t.Fatalf("missing id=\"p-max-concurrency\" input in HTML")
	}
	if !strings.Contains(htmlContent, `id="p-timeout-connect"`) || !strings.Contains(htmlContent, `id="p-timeout-queue"`) || !strings.Contains(htmlContent, `id="p-timeout-idle"`) {
		t.Fatalf("missing timeout options inputs in HTML")
	}
	if !strings.Contains(htmlContent, `id="p-tls"`) {
		t.Fatalf("missing id=\"p-tls\" select in HTML")
	}

	// Actions & inline test badge
	if !strings.Contains(htmlContent, `id="save-provider-btn"`) {
		t.Fatalf("missing id=\"save-provider-btn\" button in HTML")
	}
	if !strings.Contains(htmlContent, `id="test-editor-btn"`) {
		t.Fatalf("missing id=\"test-editor-btn\" button in HTML")
	}
	if !strings.Contains(htmlContent, `id="editor-test-badge"`) {
		t.Fatalf("missing id=\"editor-test-badge\" span in HTML")
	}

	// Models catalog section
	if !strings.Contains(htmlContent, "meta/llama-3.3-70b-instruct") {
		t.Fatalf("expected catalog model in HTML, got:\n%s", htmlContent)
	}
}

func TestAdminModelsPage_RenderExistingAndLegacyProviders(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	rawSecret := "sk-super-secret-key-1234567890"

	// 1. Add a v2 provider
	v2Prov := gatewayprovider.Provider{
		ID:       "openrouter-v2",
		Name:     "OpenRouter",
		BaseURL:  "https://openrouter.ai/api/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthBearer,
			Key:    rawSecret,
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "gpt-4o", UpstreamModel: "openai/gpt-4o", Fallbacks: []string{"claude-3-5"}},
			{Alias: "claude-hidden", UpstreamModel: "anthropic/claude-3-5-sonnet", Hidden: true},
		},
		Timeouts: gatewayprovider.Timeouts{
			Connect: 5 * time.Second,
			Idle:    30 * time.Second,
		},
		Enabled: true,
	}
	if err := p.upsertProviderV2(&v2Prov); err != nil {
		t.Fatalf("upsert v2 provider: %v", err)
	}

	// 2. Add a legacy provider
	legacyList := []customProvider{
		{
			Name:    "LegacyEndpoint",
			BaseURL: "https://api.legacy.example/v1",
			APIKey:  "sk-legacy-key-9999",
			Model:   "legacy-model-1",
			Alias:   "legacy-alias-1",
		},
	}
	if err := p.setProviders(legacyList); err != nil {
		t.Fatalf("set legacy providers: %v", err)
	}

	htmlContent := adminHTML("models", nil)

	// Verify secrets are redacted and never leaked raw
	if strings.Contains(htmlContent, rawSecret) {
		t.Fatalf("CRITICAL: raw secret %q leaked in HTML content!", rawSecret)
	}
	if strings.Contains(htmlContent, "sk-legacy-key-9999") {
		t.Fatalf("CRITICAL: legacy raw secret leaked in HTML content!")
	}
	// Verify redacted mask hint is present
	if !strings.Contains(htmlContent, "sk-...7890") {
		t.Fatalf("expected redacted key hint 'sk-...7890' in HTML content")
	}

	// Verify v2 provider card
	if !strings.Contains(htmlContent, `data-provider-id="openrouter-v2"`) {
		t.Fatalf("missing v2 provider card in HTML")
	}
	if !strings.Contains(htmlContent, "OpenRouter") || !strings.Contains(htmlContent, "https://openrouter.ai/api/v1") {
		t.Fatalf("missing v2 provider name or base URL")
	}
	if !strings.Contains(htmlContent, "openai-chat") {
		t.Fatalf("missing protocol badge 'openai-chat' in HTML")
	}

	// Verify aliases rendered with status badges
	if !strings.Contains(htmlContent, "gpt-4o") {
		t.Fatalf("missing active alias 'gpt-4o' in HTML")
	}
	if !strings.Contains(htmlContent, "claude-hidden") || !strings.Contains(htmlContent, "(hidden)") {
		t.Fatalf("missing hidden alias 'claude-hidden (hidden)' in HTML")
	}

	// Verify quick action buttons and test result inline span
	if !strings.Contains(htmlContent, `class="btn-sm edit-prov-btn" data-id="openrouter-v2"`) {
		t.Fatalf("missing Edit button for openrouter-v2")
	}
	if !strings.Contains(htmlContent, `class="btn-sm test-prov-btn" data-id="openrouter-v2"`) {
		t.Fatalf("missing Test Connection button for openrouter-v2")
	}
	if !strings.Contains(htmlContent, `class="btn-sm btn-del delete-prov-btn" data-id="openrouter-v2"`) {
		t.Fatalf("missing Delete button for openrouter-v2")
	}
	if !strings.Contains(htmlContent, `id="test-res-openrouter-v2"`) {
		t.Fatalf("missing inline test badge id=\"test-res-openrouter-v2\"")
	}

	// Verify legacy provider appears cleanly migrated and editable
	if !strings.Contains(htmlContent, "LegacyEndpoint") {
		t.Fatalf("missing legacy provider 'LegacyEndpoint' in provider list")
	}
}

func TestAdminModelsPage_CRUDAndTestConnectionHandlers(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/admin", func(c *gin.Context) {
		handleAdminDashboard(c, nil)
	})
	registerAdminAPIRoutes(engine, p, nil, nil)

	// 1. Initial page render
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin?page=models", http.NoBody)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin?page=models = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upstream providers") {
		t.Fatalf("page missing upstream providers section")
	}

	// 2. Create provider via POST /admin/api/providers
	newProv := gatewayprovider.Provider{
		ID:       "anthropic-mock",
		Name:     "Anthropic Direct",
		BaseURL:  "https://api.anthropic.example/v1",
		Protocol: ir.ProtocolAnthropicMessages,
		Auth: gatewayprovider.Auth{
			Scheme: gatewayprovider.AuthAPIKey,
			Key:    "sk-ant-test-key-54321",
		},
		Models: []gatewayprovider.ModelMapping{
			{Alias: "claude-sonnet", UpstreamModel: "claude-3-5-sonnet-20241022"},
		},
		Enabled: true,
	}
	body, err := json.Marshal(newProv)
	if err != nil {
		t.Fatalf("marshal newProv: %v", err)
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/api/providers = %d, want 201: %s", w.Code, w.Body.String())
	}

	// 3. Verify newly created provider is rendered in GET /admin?page=models
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin?page=models", http.NoBody)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin?page=models = %d", w.Code)
	}
	respHTML := w.Body.String()
	if !strings.Contains(respHTML, `data-provider-id="anthropic-mock"`) {
		t.Fatalf("expected rendered provider anthropic-mock in HTML")
	}
	if !strings.Contains(respHTML, "anthropic-messages") {
		t.Fatalf("expected protocol badge 'anthropic-messages' in HTML")
	}
	if !strings.Contains(respHTML, "claude-sonnet") {
		t.Fatalf("expected alias 'claude-sonnet' in HTML")
	}
	if !strings.Contains(respHTML, "sk-...4321") {
		t.Fatalf("expected masked secret key hint in HTML")
	}

	// 4. Test connection endpoint POST /admin/api/providers/:id/test
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/providers/anthropic-mock/test", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/api/providers/anthropic-mock/test = %d, want 200: %s", w.Code, w.Body.String())
	}
	var testRes ConnectionTestResult
	if err := json.Unmarshal(w.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("unmarshal ConnectionTestResult: %v", err)
	}
	if testRes.Status == "" {
		t.Fatalf("expected status field in ConnectionTestResult")
	}

	// 5. Update provider via PUT /admin/api/providers/:id (toggle enabled)
	newProv.Enabled = false
	newProv.Auth.Key = "" // blank key tests secret preservation
	body, err = json.Marshal(newProv)
	if err != nil {
		t.Fatalf("marshal updated newProv: %v", err)
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/admin/api/providers/anthropic-mock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /admin/api/providers/anthropic-mock = %d: %s", w.Code, w.Body.String())
	}

	// 6. Delete provider via DELETE /admin/api/providers/:id
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/admin/api/providers/anthropic-mock", http.NoBody)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /admin/api/providers/anthropic-mock = %d", w.Code)
	}

	// 7. Verify provider is removed from HTML
	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin?page=models", http.NoBody)
	engine.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `data-provider-id="anthropic-mock"`) {
		t.Fatalf("deleted provider still present in HTML")
	}
}

func TestAdminPages_ReviewCommentsFixes(t *testing.T) {
	// Comment 1: sws in adminBaseScript must be scoped to the model rows so
	// provider toggles (and captcha rows, which also use .row) are not included.
	// Still narrower than .row .sw: data-id marks a model/custom row.
	if !strings.Contains(adminBaseScript, "document.querySelectorAll('.row[data-id] .sw')") {
		t.Fatalf("expected adminBaseScript to query '.row[data-id] .sw', got script:\n%s", adminBaseScript)
	}

	// Comment 2: Wildcard alias keeping upstream as empty string rather than defaulting to alias
	if !strings.Contains(adminProviderScript, "alias.endsWith('/*') ? upstream : (upstream || alias)") {
		t.Fatalf("expected adminProviderScript to preserve upstream for wildcard alias")
	}

	// Comment 3: Existing provider configuration preservation on edit
	if !strings.Contains(adminProviderScript, "payload = JSON.parse(JSON.stringify(existing))") {
		t.Fatalf("expected adminProviderScript to clone existing provider on edit")
	}
	if !strings.Contains(adminProviderScript, "auth.header_name = existing.auth.header_name") {
		t.Fatalf("expected adminProviderScript to preserve existing.auth.header_name")
	}
	if !strings.Contains(adminProviderScript, "JSON.parse(JSON.stringify(existingModel))") {
		t.Fatalf("expected adminProviderScript to preserve existing model mapping fields")
	}
}

// TestAdminPages_ModelsOnlyWiringStaysOffOtherTabs guards the shell/script
// contract: the search box, bulk toggles and retry button only exist on the
// models page, and the caption under the tabs must describe the page it is on.
// Regression: render() ran on every page and counted unrelated .row elements,
// so stats/config/chromes claimed "0 on · 0 off · 0 upstream errors".
func TestAdminPages_ModelsOnlyWiringStaysOffOtherTabs(t *testing.T) {
	catalog := []*cliproxy.ModelInfo{{ID: "meta/llama-3.3-70b-instruct"}}

	models := adminHTML("models", catalog)
	for _, id := range []string{`id=q`, `id=all`, `id=none`, `id=retry`, `id=counts`} {
		if !strings.Contains(models, id) {
			t.Fatalf("models page is missing %s", id)
		}
	}

	for _, page := range []string{"dashboard", "config", "chromes"} {
		got := adminHTML(page, catalog)
		for _, id := range []string{`id=q`, `id=all`, `id=none`, `id=retry`, `id=counts`} {
			if strings.Contains(got, id) {
				t.Errorf("%s page still renders %s; models-only controls must not reach other tabs", page, id)
			}
		}
		if !strings.Contains(got, `class=sub`) {
			t.Errorf("%s page has no subtitle describing itself", page)
		}
	}
}

// TestAdminBaseScript_ScopesModelHooksToDataIDRows pins the selector the fix
// relies on: without [data-id] the models hooks would pick up captcha rows,
// which reuse .row but carry no switch.
func TestAdminBaseScript_ScopesModelHooksToDataIDRows(t *testing.T) {
	if !strings.Contains(adminBaseScript, `document.querySelectorAll('.row[data-id] .sw')`) {
		t.Fatalf("model toggles must be selected by .row[data-id] .sw, got:\n%s", adminBaseScript)
	}
	if strings.Contains(adminBaseScript, "style.display = 'none'") {
		t.Fatalf("control bar must be server-rendered per page, not hidden at runtime")
	}
}
