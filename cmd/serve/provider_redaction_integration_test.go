package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestV2PreferencePublicationFiltersUnadvertisedCredentials(t *testing.T) {
	key := "opaque-v2-load-credential"
	p := gatewayprovider.Provider{ID: "disabled", Name: "disabled", BaseURL: "https://up.example/v1", Protocol: ir.ProtocolOpenAIChat, Auth: gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: key}, Models: []gatewayprovider.ModelMapping{{Alias: "disabled/m", UpstreamModel: "m"}}}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := gatewayprovider.WriteDocument(path, gatewayprovider.Document{Providers: []gatewayprovider.Provider{p}}); err != nil {
		t.Fatal(err)
	}
	m := &modelPrefs{}
	if err := m.load(path); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(secretResponseRedaction())
	engine.GET("/error", func(c *gin.Context) {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream echoed " + key}})
	})
	assertFiltered := func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/error", http.NoBody)
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), key) {
			t.Fatalf("credential escaped through error response: status=%d body=%s", response.Code, response.Body)
		}
	}
	assertFiltered()
	key = "opaque-v2-env-credential"
	t.Setenv("TEST_V2_PROVIDER_CREDENTIAL", key)
	p.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, KeyEnv: "TEST_V2_PROVIDER_CREDENTIAL"}
	m.onChange = func([]customProvider) { assertFiltered() }
	if err := m.upsertProviderV2(&p); err != nil {
		t.Fatal(err)
	}
	assertFiltered()
}

func TestGrepStyleAdminAPIsAndHTMLNeverExposeRawConfiguredKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawSecret := "sk-super-secret-unique-key-9876543210"

	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	m := &modelPrefs{}
	if err := m.load(modelsPath); err != nil {
		t.Fatal(err)
	}
	// Also test global prefs used by adminRoutes
	oldPrefs := prefs
	prefs = m
	t.Cleanup(func() { prefs = oldPrefs })

	prov := gatewayprovider.Provider{
		ID:       "prov-secret-check",
		Name:     "SecretCheck",
		BaseURL:  "https://api.secretcheck.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: rawSecret},
		Headers:  map[string]string{"X-Custom": "safe-value"},
		Models:   []gatewayprovider.ModelMapping{{Alias: "secretcheck/model", UpstreamModel: "m"}},
		Enabled:  true,
	}
	if err := m.upsertProviderV2(&prov); err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(adminAccess("127.0.0.1:8080", "", false))
	registerAdminAPIRoutes(engine, m, nil, nil, nil)
	engine.GET("/admin", func(c *gin.Context) {
		handleAdminDashboard(c, nil)
	})

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/api/providers"},
		{http.MethodGet, "/admin/api/providers/prov-secret-check"},
		{http.MethodGet, "/admin?page=models"},
		{http.MethodGet, "/admin?page=config"},
		{http.MethodGet, "/admin?page=dashboard"},
		{http.MethodGet, "/admin?page=chromes"},
		{http.MethodGet, "/admin"},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequestWithContext(t.Context(), ep.method, ep.path, http.NoBody)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s %s returned status %d: %s", ep.method, ep.path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, rawSecret) {
			t.Fatalf("CRITICAL SECURITY LEAK: %s %s leaked raw configured key in response body: %s", ep.method, ep.path, body)
		}
	}

	// Verify redacted hint is present in JSON and HTML
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/providers/prov-secret-check", http.NoBody)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "sk-...3210") {
		t.Fatalf("expected redacted key hint sk-...3210 in API response, got: %s", w.Body.String())
	}

	reqHTML := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin?page=models", http.NoBody)
	reqHTML.Host = "localhost:8080"
	wHTML := httptest.NewRecorder()
	engine.ServeHTTP(wHTML, reqHTML)
	if !strings.Contains(wHTML.Body.String(), "sk-...3210") {
		t.Fatalf("expected redacted key hint sk-...3210 in admin HTML page, got: %s", wHTML.Body.String())
	}
}

func TestEnvVarResolutionAtRequestTime(t *testing.T) {
	prov := gatewayprovider.Provider{
		ID:       "env-prov",
		Name:     "EnvProv",
		BaseURL:  "https://api.envprov.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, KeyEnv: "TEST_DYN_REQ_KEY"},
		Models:   []gatewayprovider.ModelMapping{{Alias: "envprov/m", UpstreamModel: "m"}},
	}

	// 1. Initial env var value
	t.Setenv("TEST_DYN_REQ_KEY", "initial-dynamic-key-1111")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.envprov.example/v1/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ApplyHeaders(req, nil, &prov); err != nil {
		t.Fatalf("ApplyHeaders failed: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer initial-dynamic-key-1111" {
		t.Fatalf("expected Authorization header with initial key, got: %q", got)
	}

	// 2. Updated env var value at next request time
	t.Setenv("TEST_DYN_REQ_KEY", "updated-dynamic-key-2222")
	req2, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.envprov.example/v1/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ApplyHeaders(req2, nil, &prov); err != nil {
		t.Fatalf("ApplyHeaders failed: %v", err)
	}
	if got := req2.Header.Get("Authorization"); got != "Bearer updated-dynamic-key-2222" {
		t.Fatalf("expected Authorization header with updated dynamic key, got: %q", got)
	}
}

func TestMigrationOfLegacyInlineKeysOnFirstSave(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	secretsPath := filepath.Join(dir, "secrets.json")

	secret := "sk-legacy-to-migrate-55555"

	legacyDoc := gatewayprovider.Document{
		Providers: []gatewayprovider.Provider{
			{
				ID:       "legacy-save-prov",
				Name:     "LegacyProv",
				BaseURL:  "https://api.legacy.example/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: secret},
				Models:   []gatewayprovider.ModelMapping{{Alias: "m", UpstreamModel: "m"}},
			},
		},
	}
	raw, err := json.Marshal(legacyDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	m := &modelPrefs{}
	if err := m.load(modelsPath); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	// Secrets file should not exist yet before first save
	if _, err := os.Stat(secretsPath); err == nil {
		t.Fatal("secrets.json should not exist before first save")
	}

	// Trigger save
	if err := m.save(); err != nil {
		t.Fatalf("m.save failed: %v", err)
	}

	// Check catalog file on disk: must NOT contain plaintext key
	catalogBytes, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(catalogBytes, []byte(secret)) {
		t.Fatalf("catalog models.json leaked raw secret: %s", string(catalogBytes))
	}
	if !bytes.Contains(catalogBytes, []byte("key_ref")) {
		t.Fatalf("catalog models.json must contain key_ref: %s", string(catalogBytes))
	}

	// Check secrets file on disk: must contain the secret
	secretsBytes, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("secrets.json missing: %v", err)
	}
	if !bytes.Contains(secretsBytes, []byte(secret)) {
		t.Fatalf("secrets.json missing migrated key: %s", string(secretsBytes))
	}

	// Reload in a fresh modelPrefs instance
	m2 := &modelPrefs{}
	if err := m2.load(modelsPath); err != nil {
		t.Fatalf("m2.load failed: %v", err)
	}
	pv, found, err := m2.getProviderV2("legacy-save-prov")
	if err != nil || !found {
		t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
	}
	if pv.Auth.Key != secret {
		t.Fatalf("reloaded provider key mismatch: got %q, want %q", pv.Auth.Key, secret)
	}
}

func TestPreservingSecretsWhenUpdatingProviderWithRedactedSentinel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawSecret := "sk-persistent-secret-88888"

	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	m := &modelPrefs{}
	if err := m.load(modelsPath); err != nil {
		t.Fatal(err)
	}
	oldPrefs := prefs
	prefs = m
	t.Cleanup(func() { prefs = oldPrefs })

	prov := gatewayprovider.Provider{
		ID:       "prov-sentinel-test",
		Name:     "SentinelTest",
		BaseURL:  "https://api.sentinel.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: rawSecret},
		Headers:  map[string]string{"X-Test": "val"},
		Models:   []gatewayprovider.ModelMapping{{Alias: "sentinel/m", UpstreamModel: "m"}},
	}
	if err := m.upsertProviderV2(&prov); err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(adminAccess("127.0.0.1:8080", "", false))
	registerAdminAPIRoutes(engine, m, nil, nil, nil)
	engine.POST("/admin/providers", handleAdminProviders)

	sentinels := []string{"[REDACTED]", "unchanged", "sk-...8888"}

	for _, sentinel := range sentinels {
		t.Run("PUT Admin API: "+sentinel, func(t *testing.T) {
			updatePayload := prov
			updatePayload.Auth.Key = sentinel
			updatePayload.Headers = map[string]string{"X-Test": "val"}

			bodyBytes, err := json.Marshal(updatePayload)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/admin/api/providers/prov-sentinel-test", bytes.NewReader(bodyBytes))
			req.Host = "localhost:8080"
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("PUT failed with code %d: %s", w.Code, w.Body.String())
			}

			// Verify in-memory key
			stored, found, err := m.getProviderV2("prov-sentinel-test")
			if err != nil || !found {
				t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
			}
			if stored.Auth.Key != rawSecret {
				t.Fatalf("key was overwritten with sentinel %q: got %q", sentinel, stored.Auth.Key)
			}

			// Verify on-disk secrets.json
			secrets, err := gatewayprovider.LoadSecrets(gatewayprovider.DefaultSecretsPath(modelsPath))
			if err != nil || secrets["prov-sentinel-test"] != rawSecret {
				t.Fatalf("on-disk secret was corrupted by sentinel %q: got %q", sentinel, secrets["prov-sentinel-test"])
			}
		})
	}

	// Test admin HTML form POST /admin/providers with redacted sentinel
	t.Run("POST /admin/providers HTML form sentinel", func(t *testing.T) {
		formProviders := []customProvider{
			{
				Name:    prov.Name,
				BaseURL: prov.BaseURL,
				APIKey:  "sk-...8888", // redacted sentinel from JavaScript PROVIDERS
				Model:   "m",
				Alias:   "sentinel/m",
			},
			{
				Name:    "NewProv",
				BaseURL: "https://api.newprov.example/v1",
				APIKey:  "sk-new-form-prov-key-1234",
				Model:   "new-m",
				Alias:   "newprov/m",
			},
		}

		bodyBytes, err := json.Marshal(formProviders)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/providers", bytes.NewReader(bodyBytes))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("POST /admin/providers failed with code %d: %s", w.Code, w.Body.String())
		}

		// Verify existing provider's secret was preserved
		stored, found, err := m.getProviderV2("prov-sentinel-test")
		if err != nil || !found {
			t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
		}
		if stored.Auth.Key != rawSecret {
			t.Fatalf("key was overwritten by admin form submission: got %q, want %q", stored.Auth.Key, rawSecret)
		}

		// Verify new provider was added
		storedNew, foundNew, err := m.getProviderV2("NewProv")
		if err != nil || !foundNew {
			t.Fatalf("NewProv not found: err=%v", err)
		}
		if storedNew.Auth.Key != "sk-new-form-prov-key-1234" {
			t.Fatalf("NewProv key mismatch: got %q", storedNew.Auth.Key)
		}
	})
}

func TestAdminClearKeyOrSwitchToKeyEnvOrAuthNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	m := &modelPrefs{}
	if err := m.load(modelsPath); err != nil {
		t.Fatal(err)
	}
	oldPrefs := prefs
	prefs = m
	t.Cleanup(func() { prefs = oldPrefs })

	rawSecret := "sk-initial-secret-123456"
	prov := gatewayprovider.Provider{
		ID:       "prov-switch-test",
		Name:     "SwitchTest",
		BaseURL:  "https://api.switchtest.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: rawSecret},
		Models:   []gatewayprovider.ModelMapping{{Alias: "switchtest/m", UpstreamModel: "m"}},
		Enabled:  true,
	}
	if err := m.upsertProviderV2(&prov); err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(adminAccess("127.0.0.1:8080", "", false))
	registerAdminAPIRoutes(engine, m, nil, nil, nil)
	engine.POST("/admin/providers", handleAdminProviders)

	// 1. Switch to KeyEnv via HTML form POST /admin/providers
	t.Run("POST /admin/providers switch to KeyEnv", func(t *testing.T) {
		formProviders := []customProvider{
			{
				Name:    "SwitchTest",
				BaseURL: "https://api.switchtest.example/v1",
				APIKey:  "",
				KeyEnv:  "TEST_SWITCH_KEY_ENV",
				Model:   "m",
				Alias:   "switchtest/m",
			},
		}
		bodyBytes, err := json.Marshal(formProviders)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/providers", bytes.NewReader(bodyBytes))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("POST failed with code %d: %s", w.Code, w.Body.String())
		}

		stored, found, err := m.getProviderV2("prov-switch-test")
		if err != nil || !found {
			t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
		}
		if stored.Auth.KeyEnv != "TEST_SWITCH_KEY_ENV" || stored.Auth.Key != "" || stored.Auth.KeyRef != "" {
			t.Fatalf("expected KeyEnv switch without old key/keyref, got: %#v", stored.Auth)
		}
	})

	// 2. Clear key (switch to AuthNone) via HTML form POST /admin/providers
	t.Run("POST /admin/providers clear key to AuthNone", func(t *testing.T) {
		formProviders := []customProvider{
			{
				Name:    "SwitchTest",
				BaseURL: "https://api.switchtest.example/v1",
				APIKey:  "",
				Model:   "m",
				Alias:   "switchtest/m",
			},
		}
		bodyBytes, err := json.Marshal(formProviders)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/providers", bytes.NewReader(bodyBytes))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("POST failed with code %d: %s", w.Code, w.Body.String())
		}

		stored, found, err := m.getProviderV2("prov-switch-test")
		if err != nil || !found {
			t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
		}
		if stored.Auth.Scheme != gatewayprovider.AuthNone || stored.Auth.Key != "" || stored.Auth.KeyRef != "" {
			t.Fatalf("expected AuthNone without old key/keyref, got: %#v", stored.Auth)
		}
	})

	// 3. Set a new key, then switch to AuthNone via PUT /admin/api/providers/:id
	t.Run("PUT /admin/api/providers switch to AuthNone and KeyEnv", func(t *testing.T) {
		// Restore to Bearer with key
		prov.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: "sk-fresh-secret-4444"}
		if err := m.upsertProviderV2(&prov); err != nil {
			t.Fatal(err)
		}

		// Switch to AuthNone via REST API
		provNone := prov
		provNone.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone}
		putBytes, err := json.Marshal(provNone)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/admin/api/providers/prov-switch-test", bytes.NewReader(putBytes))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("PUT AuthNone failed with code %d: %s", w.Code, w.Body.String())
		}

		stored, found, err := m.getProviderV2("prov-switch-test")
		if err != nil || !found {
			t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
		}
		if stored.Auth.Scheme != gatewayprovider.AuthNone || stored.Auth.Key != "" || stored.Auth.KeyRef != "" {
			t.Fatalf("expected AuthNone, got: %#v", stored.Auth)
		}

		// Switch to KeyEnv via REST API
		provKeyEnv := prov
		provKeyEnv.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, KeyEnv: "REST_KEY_ENV"}
		putBytes, err = json.Marshal(provKeyEnv)
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/admin/api/providers/prov-switch-test", bytes.NewReader(putBytes))
		req.Host = "localhost:8080"
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("PUT KeyEnv failed with code %d: %s", w.Code, w.Body.String())
		}

		stored, found, err = m.getProviderV2("prov-switch-test")
		if err != nil || !found {
			t.Fatalf("getProviderV2 failed: found=%v, err=%v", found, err)
		}
		if stored.Auth.KeyEnv != "REST_KEY_ENV" || stored.Auth.Key != "" || stored.Auth.KeyRef != "" {
			t.Fatalf("expected KeyEnv without old key/keyref, got: %#v", stored.Auth)
		}
	})
}
