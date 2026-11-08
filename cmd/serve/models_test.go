package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

func TestBuildCatalogVirtualModels(t *testing.T) {
	p := &modelPrefs{
		hidden:  map[string]struct{}{},
		deleted: map[string]struct{}{},
	}
	catalog := buildCatalog(p, nil)

	foundAutomode, foundCycle := false, false
	for _, m := range catalog {
		if m.ID == "automode" {
			foundAutomode = true
			if m.Type != "virtual" || m.OwnedBy != "gateway" || m.Object != "model" {
				t.Errorf("automode model metadata mismatch: %+v", m)
			}
		}
		if m.ID == "cycle" {
			foundCycle = true
			if m.Type != "virtual" || m.OwnedBy != "gateway" || m.Object != "model" {
				t.Errorf("cycle model metadata mismatch: %+v", m)
			}
		}
	}
	if !foundAutomode || !foundCycle {
		t.Fatalf("expected virtual models in catalog: foundAutomode=%v, foundCycle=%v", foundAutomode, foundCycle)
	}

	// Test masking virtual models
	p.hidden["automode"] = struct{}{}
	catalogMasked := buildCatalog(p, nil)
	for _, m := range catalogMasked {
		if m.ID == "automode" {
			t.Fatalf("masked virtual model 'automode' should not appear in catalog")
		}
	}
}

func TestBuildCatalogNvidiaModels(t *testing.T) {
	fakeNvidiaModels := []*cliproxy.ModelInfo{
		{ID: "nvidia/llama-3.1-nemotron-70b-instruct", OwnedBy: "nvidia", Created: 100},
		{ID: "moonshotai/kimi-k3", OwnedBy: "moonshotai", Created: 200},
		{ID: "meta/llama-3.1-8b-instruct", OwnedBy: "meta", Created: 300},
	}
	getNvidia := func() []*cliproxy.ModelInfo { return fakeNvidiaModels }

	p := &modelPrefs{
		hidden:  map[string]struct{}{"meta/llama-3.1-8b-instruct": {}},
		deleted: map[string]struct{}{},
	}

	catalog := buildCatalog(p, getNvidia)

	byID := make(map[string]gatewayModel)
	for _, m := range catalog {
		byID[m.ID] = m
	}

	if _, exists := byID["meta/llama-3.1-8b-instruct"]; exists {
		t.Errorf("hidden NVIDIA model should be excluded from catalog")
	}

	kimi, exists := byID["moonshotai/kimi-k3"]
	if !exists {
		t.Fatalf("expected kimi-k3 in catalog")
	}
	if kimi.OwnedBy != "moonshotai" {
		t.Errorf("kimi owned_by = %q, want moonshotai", kimi.OwnedBy)
	}
	if kimi.UpstreamProtocol != string(ir.ProtocolOpenAIChat) {
		t.Errorf("kimi upstream_protocol = %q, want %q", kimi.UpstreamProtocol, ir.ProtocolOpenAIChat)
	}
	if kimi.Capabilities == nil || !kimi.Capabilities.Reasoning || !kimi.Capabilities.Streaming {
		t.Errorf("kimi capabilities mismatch: %+v", kimi.Capabilities)
	}

	nemo, exists := byID["nvidia/llama-3.1-nemotron-70b-instruct"]
	if !exists {
		t.Fatalf("expected nemotron in catalog")
	}
	if nemo.OwnedBy != "nvidia" {
		t.Errorf("nemo owned_by = %q, want nvidia", nemo.OwnedBy)
	}
	if nemo.Capabilities == nil || !nemo.Capabilities.Streaming {
		t.Errorf("nemo capabilities should have streaming=true")
	}
}

func TestBuildCatalogV2Providers(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "models.json")
	store, err := gatewayprovider.OpenStore(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := &modelPrefs{
		path:          storePath,
		providerStore: store,
		hidden:        map[string]struct{}{},
		deleted:       map[string]struct{}{},
	}

	customCaps := json.RawMessage(`{"top_k":false,"count_tokens":true}`)

	providers := []gatewayprovider.Provider{
		{
			ID:       "openrouter",
			Name:     "OpenRouter",
			BaseURL:  "https://openrouter.ai/api/v1",
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  true,
			Models: []gatewayprovider.ModelMapping{
				{Alias: "openrouter/free-chat", UpstreamModel: "meta-llama/llama-3"},
				{Alias: "openrouter/hidden-model", UpstreamModel: "hidden", Hidden: true},
			},
		},
		{
			ID:       "anthropic-direct",
			Name:     "Anthropic Inc",
			BaseURL:  "https://api.anthropic.com",
			Protocol: ir.ProtocolAnthropicMessages,
			Enabled:  true,
			Extensions: map[string]json.RawMessage{
				"capabilities": customCaps,
			},
			Models: []gatewayprovider.ModelMapping{
				{Alias: "claude-3-7-sonnet", UpstreamModel: "claude-3-7-sonnet-20250219"},
			},
		},
		{
			ID:       "disabled-provider",
			Name:     "Disabled",
			BaseURL:  "https://api.disabled.com",
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  false,
			Models: []gatewayprovider.ModelMapping{
				{Alias: "disabled-alias", UpstreamModel: "disabled-upstream"},
			},
		},
	}

	err = store.Update(func(doc *gatewayprovider.Document) error {
		doc.Providers = providers
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	catalog := buildCatalog(p, nil)
	byID := make(map[string]gatewayModel)
	for _, m := range catalog {
		byID[m.ID] = m
	}

	// 1. Verify visible alias
	chatModel, ok := byID["openrouter/free-chat"]
	if !ok {
		t.Fatalf("expected openrouter/free-chat in catalog")
	}
	if chatModel.OwnedBy != "OpenRouter" {
		t.Errorf("owned_by = %q, want OpenRouter", chatModel.OwnedBy)
	}
	if chatModel.UpstreamProtocol != string(ir.ProtocolOpenAIChat) {
		t.Errorf("upstream_protocol = %q, want %q", chatModel.UpstreamProtocol, ir.ProtocolOpenAIChat)
	}
	if chatModel.Capabilities == nil || !chatModel.Capabilities.Streaming || !chatModel.Capabilities.StructuredOutput {
		t.Errorf("chatModel capabilities mismatch: %+v", chatModel.Capabilities)
	}

	// 2. Hidden mapping excluded
	if _, ok := byID["openrouter/hidden-model"]; ok {
		t.Errorf("hidden mapping should not appear in catalog")
	}

	// 3. Disabled provider excluded
	if _, ok := byID["disabled-alias"]; ok {
		t.Errorf("disabled provider models should not appear in catalog")
	}

	// 4. Anthropic direct with capability override
	claudeModel, ok := byID["claude-3-7-sonnet"]
	if !ok {
		t.Fatalf("expected claude-3-7-sonnet in catalog")
	}
	if claudeModel.OwnedBy != "Anthropic Inc" {
		t.Errorf("owned_by = %q, want Anthropic Inc", claudeModel.OwnedBy)
	}
	if claudeModel.UpstreamProtocol != string(ir.ProtocolAnthropicMessages) {
		t.Errorf("upstream_protocol = %q, want %q", claudeModel.UpstreamProtocol, ir.ProtocolAnthropicMessages)
	}
	// Verify override: base anthropic has TopK: true, but our override sets top_k: false and count_tokens: true
	if claudeModel.Capabilities == nil {
		t.Fatalf("expected claudeModel capabilities to be non-nil")
	}
	if claudeModel.Capabilities.TopK {
		t.Errorf("expected TopK override to false, got true")
	}
	if !claudeModel.Capabilities.CountTokens {
		t.Errorf("expected CountTokens override to true, got false")
	}
	if !claudeModel.Capabilities.Tools {
		t.Errorf("expected base Tools capability to remain true")
	}
}

func TestBuildCatalogWildcardPolicy(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "models.json")
	store, err := gatewayprovider.OpenStore(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := &modelPrefs{
		path:          storePath,
		providerStore: store,
		hidden:        map[string]struct{}{},
		deleted:       map[string]struct{}{},
	}

	providers := []gatewayprovider.Provider{
		{
			ID:       "openrouter",
			Name:     "OpenRouter",
			BaseURL:  "https://openrouter.ai/api/v1",
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  true,
			Models: []gatewayprovider.ModelMapping{
				{Alias: "openrouter/*", UpstreamModel: "*"},
				{Alias: "openrouter/exact-model", UpstreamModel: "meta/llama-3"},
			},
		},
	}

	err = store.Update(func(doc *gatewayprovider.Document) error {
		doc.Providers = providers
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Default policy (WildcardPolicyPlaceholder)
	catPlaceholder := buildCatalog(p, nil)
	foundPlaceholder := false
	for _, m := range catPlaceholder {
		if m.ID == "openrouter/*" {
			foundPlaceholder = true
			if m.OwnedBy != "OpenRouter" || m.UpstreamProtocol != string(ir.ProtocolOpenAIChat) {
				t.Errorf("wildcard entry metadata mismatch: %+v", m)
			}
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected wildcard placeholder entry 'openrouter/*' under WildcardPolicyPlaceholder")
	}

	// 2. Omit policy (WildcardPolicyOmit)
	catOmit := buildCatalog(p, nil, WithWildcardPolicy(WildcardPolicyOmit))
	for _, m := range catOmit {
		if m.ID == "openrouter/*" {
			t.Fatalf("wildcard entry 'openrouter/*' should be omitted under WildcardPolicyOmit")
		}
	}
	// Exact model should still exist under omit policy
	foundExact := false
	for _, m := range catOmit {
		if m.ID == "openrouter/exact-model" {
			foundExact = true
		}
	}
	if !foundExact {
		t.Fatalf("exact model 'openrouter/exact-model' should exist under WildcardPolicyOmit")
	}
}

func TestBuildCatalogAllowShadow(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "models.json")
	store, err := gatewayprovider.OpenStore(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := &modelPrefs{
		path:          storePath,
		providerStore: store,
		hidden:        map[string]struct{}{},
		deleted:       map[string]struct{}{},
	}

	fakeNvidiaModels := []*cliproxy.ModelInfo{
		{ID: "custom-shadowed-model", OwnedBy: "nvidia", Created: 100},
	}
	getNvidia := func() []*cliproxy.ModelInfo { return fakeNvidiaModels }

	providers := []gatewayprovider.Provider{
		{
			ID:       "custom-provider",
			Name:     "CustomOwner",
			BaseURL:  "https://custom.example.com",
			Protocol: ir.ProtocolAnthropicMessages,
			Enabled:  true,
			Models: []gatewayprovider.ModelMapping{
				{Alias: "custom-shadowed-model", UpstreamModel: "upstream-target", AllowShadow: true},
			},
		},
	}

	err = store.Update(func(doc *gatewayprovider.Document) error {
		doc.Providers = providers
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	catalog := buildCatalog(p, getNvidia)
	var shadowed gatewayModel
	count := 0
	for _, m := range catalog {
		if m.ID == "custom-shadowed-model" {
			shadowed = m
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one entry for alias 'custom-shadowed-model', got %d", count)
	}
	if shadowed.OwnedBy != "CustomOwner" {
		t.Errorf("expected shadowed model owned_by to be CustomOwner, got %q", shadowed.OwnedBy)
	}
	if shadowed.UpstreamProtocol != string(ir.ProtocolAnthropicMessages) {
		t.Errorf("expected shadowed model upstream_protocol to be anthropic-messages, got %q", shadowed.UpstreamProtocol)
	}
}

func TestModelsAndHealthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelsList := []gatewayModel{
		{
			Type:        "virtual",
			Object:      "model",
			ID:          "automode",
			Name:        "automode",
			DisplayName: "automode",
			OwnedBy:     "gateway",
		},
		{
			Type:             "model",
			Object:           "model",
			ID:               "meta/llama-3.1-70b-instruct",
			Name:             "meta/llama-3.1-70b-instruct",
			DisplayName:      "meta/llama-3.1-70b-instruct",
			OwnedBy:          "meta",
			UpstreamProtocol: "openai-chat",
			Capabilities:     &adapter.Capabilities{Streaming: true},
		},
	}

	engine := gin.New()
	engine.Use(modelsAndHealthMiddleware(func() []gatewayModel { return modelsList }, nil))

	for _, path := range []string{"/v1/models", "/v1/models/"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("path %s returned status %d, want %d", path, rec.Code, http.StatusOK)
		}

		var resp struct {
			Object  string         `json:"object"`
			Data    []gatewayModel `json:"data"`
			HasMore bool           `json:"has_more"`
			FirstID string         `json:"first_id"`
			LastID  string         `json:"last_id"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse JSON response: %v", err)
		}

		if resp.Object != "list" {
			t.Errorf("object = %q, want list", resp.Object)
		}
		if resp.HasMore {
			t.Errorf("has_more = true, want false")
		}
		if resp.FirstID != "automode" {
			t.Errorf("first_id = %q, want automode", resp.FirstID)
		}
		if resp.LastID != "meta/llama-3.1-70b-instruct" {
			t.Errorf("last_id = %q, want meta/llama-3.1-70b-instruct", resp.LastID)
		}
		if len(resp.Data) != 2 {
			t.Fatalf("expected 2 items in data, got %d", len(resp.Data))
		}
		if resp.Data[1].UpstreamProtocol != "openai-chat" {
			t.Errorf("data[1].upstream_protocol = %q, want openai-chat", resp.Data[1].UpstreamProtocol)
		}
		if resp.Data[1].Capabilities == nil || !resp.Data[1].Capabilities.Streaming {
			t.Errorf("data[1].capabilities missing streaming flag")
		}
	}
}

func TestWriteClaudeModelCacheRefreshedOnProviderChange(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)

	catalog := []gatewayModel{
		{ID: "automode", DisplayName: "automode"},
		{ID: "my-custom-model", DisplayName: "my-custom-model"},
	}

	writeClaudeModelCache(":8080", catalog)

	cacheFile := filepath.Join(tempHome, ".claude", "cache", "gateway-models.json")
	content, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("failed to read claude model cache: %v", err)
	}

	var parsed struct {
		BaseURL   string `json:"baseUrl"`
		FetchedAt int64  `json:"fetchedAt"`
		Models    []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}

	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("failed to unmarshal cache file: %v", err)
	}

	if parsed.BaseURL != "http://localhost:8080" {
		t.Errorf("baseUrl = %q, want http://localhost:8080", parsed.BaseURL)
	}
	if len(parsed.Models) != 2 {
		t.Fatalf("expected 2 models in cache, got %d", len(parsed.Models))
	}
	if parsed.Models[0].ID != "automode" || parsed.Models[1].ID != "my-custom-model" {
		t.Errorf("cached models mismatch: %+v", parsed.Models)
	}
}

func TestClaudeCacheRefreshedOnPrefsOnChange(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)

	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "models.json")
	store, err := gatewayprovider.OpenStore(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := &modelPrefs{
		path:          storePath,
		providerStore: store,
		hidden:        map[string]struct{}{},
		deleted:       map[string]struct{}{},
	}

	cacheRefreshed := false
	getModels := func() []gatewayModel {
		return buildCatalog(p, nil)
	}

	p.onChange = func(_ []customProvider) {
		writeClaudeModelCache(":8080", getModels())
		cacheRefreshed = true
	}

	// Trigger a provider change via commitPrefs
	err = p.commitPrefs(func(file *prefsFile) error {
		file.ProvidersV2 = []gatewayprovider.Provider{
			{
				ID:       "test-provider",
				Name:     "TestProvider",
				BaseURL:  "https://test.example.com",
				Protocol: ir.ProtocolOpenAIChat,
				Enabled:  true,
				Models: []gatewayprovider.ModelMapping{
					{Alias: "test/model-1", UpstreamModel: "m1"},
				},
			},
		}
		return nil
	}, true)
	if err != nil {
		t.Fatalf("commitPrefs failed: %v", err)
	}

	if !cacheRefreshed {
		t.Fatalf("expected onChange to be invoked and refresh the Claude model cache")
	}

	cacheFile := filepath.Join(tempHome, ".claude", "cache", "gateway-models.json")
	content, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("failed to read claude model cache file: %v", err)
	}

	var parsed struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("failed to parse cache file: %v", err)
	}

	foundTestModel := false
	for _, m := range parsed.Models {
		if m.ID == "test/model-1" {
			foundTestModel = true
			break
		}
	}
	if !foundTestModel {
		t.Fatalf("expected test/model-1 in refreshed Claude model cache, got: %+v", parsed.Models)
	}
}
