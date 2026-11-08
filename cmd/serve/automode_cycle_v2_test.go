package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func setupTestV2Prefs(t *testing.T) (prefs *modelPrefs, path string) {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "models.json")
	provEnabled := gatewayprovider.Provider{
		ID:       "prov-enabled",
		Name:     "prov-enabled",
		BaseURL:  "https://example.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Enabled:  true,
		Models: []gatewayprovider.ModelMapping{
			{Alias: "v2-active", UpstreamModel: "v2-active-upstream"},
			{Alias: "v2-hidden", UpstreamModel: "v2-hidden-upstream", Hidden: true},
		},
	}
	provDisabled := gatewayprovider.Provider{
		ID:       "prov-disabled",
		Name:     "prov-disabled",
		BaseURL:  "https://example.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Enabled:  false,
		Models: []gatewayprovider.ModelMapping{
			{Alias: "v2-disabled", UpstreamModel: "v2-disabled-upstream"},
		},
	}
	doc := gatewayprovider.Document{
		Providers: []gatewayprovider.Provider{provEnabled, provDisabled},
	}
	if err := gatewayprovider.WriteDocument(filePath, doc); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(filePath); err != nil {
		t.Fatal(err)
	}
	return p, filePath
}

func TestHandleAdminAutomodeRejectsDisabledAndHiddenV2Models(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, path := setupTestV2Prefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	router := gin.New()
	router.POST("/admin/automode", handleAdminAutomode)

	postAutomode := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/automode", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		return w
	}

	// 1. Rejected if model belongs to disabled v2 provider
	w := postAutomode(`{"model":"v2-disabled"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "choose an enabled gateway model") {
		t.Fatalf("expected 400 for disabled provider model, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Rejected if model has Hidden: true in v2 provider
	w = postAutomode(`{"model":"v2-hidden"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "choose an enabled gateway model") {
		t.Fatalf("expected 400 for hidden v2 model, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Rejected if model is "automode"
	w = postAutomode(`{"model":"automode"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "choose an enabled gateway model") {
		t.Fatalf("expected 400 for automode, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Accepted for active v2 model, with single atomic persistence for model and chat_responses
	w = postAutomode(`{"model":"v2-active","chat_responses":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for active v2 model, got %d: %s", w.Code, w.Body.String())
	}
	if p.autoModelID() != "v2-active" {
		t.Fatalf("expected autoModel v2-active, got %s", p.autoModelID())
	}
	if !p.autoModelChatResponsesEnabled() {
		t.Fatalf("expected autoModelChatResponsesEnabled true")
	}

	// Verify single atomic persistence loaded from disk
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if loaded.autoModelID() != "v2-active" || !loaded.autoModelChatResponsesEnabled() {
		t.Fatalf("disk reload mismatch: model=%s chat_responses=%v", loaded.autoModelID(), loaded.autoModelChatResponsesEnabled())
	}
}

func TestSetCycleRejectsDisabledAndHiddenV2Models(t *testing.T) {
	p, _ := setupTestV2Prefs(t)

	// 1. Rejected if model belongs to disabled provider
	if err := p.setCycle(cycleConfig{Models: []string{"v2-disabled"}}); err == nil || !strings.Contains(err.Error(), "unknown cycle model") {
		t.Fatalf("expected unknown cycle model error for disabled model, got %v", err)
	}

	// 2. Rejected if model is hidden
	if err := p.setCycle(cycleConfig{Models: []string{"v2-hidden"}}); err == nil || !strings.Contains(err.Error(), "unknown cycle model") {
		t.Fatalf("expected unknown cycle model error for hidden model, got %v", err)
	}

	// 3. Accepted if model is active
	if err := p.setCycle(cycleConfig{Models: []string{"v2-active"}}); err != nil {
		t.Fatalf("expected success for active model, got %v", err)
	}
}

func TestClaudeAutoModelAliasTreatsDisabledAndHiddenV2AsHidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, _ := setupTestV2Prefs(t)

	engine := gin.New()
	engine.Use(claudeAutoModelAlias(p))
	engine.POST("/v1/messages", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Fatal(err)
		}
		c.Data(http.StatusOK, "application/json", body)
	})

	postMessages := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"automode"}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		return w
	}

	// Hidden v2 model target -> 404 automode target is hidden
	p.mu.Lock()
	p.autoModel = "v2-hidden"
	p.mu.Unlock()
	w := postMessages()
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "automode target is hidden") {
		t.Fatalf("expected 404 for hidden automode target, got %d: %s", w.Code, w.Body.String())
	}

	// Disabled provider model target -> 404 automode target is hidden
	p.mu.Lock()
	p.autoModel = "v2-disabled"
	p.mu.Unlock()
	w = postMessages()
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "automode target is hidden") {
		t.Fatalf("expected 404 for disabled provider automode target, got %d: %s", w.Code, w.Body.String())
	}

	// Active v2 model target -> 200 rewritten
	p.mu.Lock()
	p.autoModel = "v2-active"
	p.mu.Unlock()
	w = postMessages()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"v2-active"`) {
		t.Fatalf("expected 200 rewritten for active automode target, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCycleRouterSkipsDisabledAndHiddenV2Models(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, _ := setupTestV2Prefs(t)

	// Configure cycle with hidden, disabled, and active models
	p.mu.Lock()
	p.cycle = &cycleConfig{
		Name:   "cycle",
		Models: []string{"v2-hidden", "v2-disabled", "v2-active"},
	}
	p.cycleIndex = 0
	p.mu.Unlock()

	var routedModel string
	engine := gin.New()
	engine.Use(cycleRouter(p, engine))
	engine.POST("/v1/messages", func(c *gin.Context) {
		var body struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		routedModel = body.Model
		c.JSON(http.StatusOK, gin.H{"model": routedModel})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle"}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if routedModel != "v2-active" {
		t.Fatalf("expected cycle to skip hidden/disabled and route to v2-active, got %s", routedModel)
	}

	// If all models in cycle are disabled or hidden, returns 503
	p.mu.Lock()
	p.cycle = &cycleConfig{
		Name:   "cycle",
		Models: []string{"v2-hidden", "v2-disabled"},
	}
	p.cycleIndex = 0
	p.mu.Unlock()

	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle"}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no available cycle models") {
		t.Fatalf("expected 503 when all cycle models are unavailable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConcurrentClaudeAutoModelAliasAndAdminAutomode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, _ := setupTestV2Prefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	adminRouter := gin.New()
	adminRouter.POST("/admin/automode", handleAdminAutomode)

	clientRouter := gin.New()
	clientRouter.Use(claudeAutoModelAlias(p))
	clientHandler := func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Data(http.StatusOK, "application/json", body)
	}
	clientRouter.POST("/v1/messages", clientHandler)
	clientRouter.POST("/v1/chat/completions", clientHandler)
	clientRouter.POST("/v1/responses", clientHandler)

	var wg sync.WaitGroup
	workers := 8
	iterations := 50

	// Concurrent admin writers toggling chat_responses and model
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				chatResp := (j%2 == 0)
				payload, err := json.Marshal(map[string]any{
					"model":          "v2-active",
					"chat_responses": chatResp,
				})
				if err != nil {
					t.Errorf("marshal error: %v", err)
					return
				}
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/automode", bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				adminRouter.ServeHTTP(w, req)
			}
		}()
	}

	// Concurrent client readers calling endpoints
	paths := []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				path := paths[j%len(paths)]
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"model":"automode"}`))
				req.Header.Set("Content-Type", "application/json")
				clientRouter.ServeHTTP(w, req)
			}
		}()
	}

	wg.Wait()
}
