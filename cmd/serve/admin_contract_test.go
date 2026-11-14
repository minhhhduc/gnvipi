package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// contractEngine is the production admin surface without the legacy routes:
// the JSON API and the embedded SPA behind adminAccess.
func contractEngine(t *testing.T, p *modelPrefs, token string, catalog []*cliproxy.ModelInfo) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(requestBodyLimit(1<<20, 1<<20))
	engine.Use(adminAccess("127.0.0.1:8080", token, nil))
	registerAdminAPIRoutes(engine, p, catalog, newChromeLifecycle(context.Background(), nil, nil), nil)
	registerAdminUI(engine)
	return engine
}

func object(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal JSON object: %v: %s", err, body)
	}
	return m
}

func TestAdminAPIContract_Matrix(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	catalog := []*cliproxy.ModelInfo{{ID: "vendor/model-a"}, {ID: models.DefaultModel}}
	engine := contractEngine(t, p, "", catalog)

	provider := gatewayprovider.Provider{
		ID:       "contract-provider",
		Name:     "Contract Provider",
		BaseURL:  "https://api.example.com/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: "sk-test-123456"},
		Models:   []gatewayprovider.ModelMapping{{Alias: "contract/model", UpstreamModel: "model"}},
		Enabled:  true,
	}

	steps := []struct {
		name, method, path string
		body               any
		want               int
		keys               []string
	}{
		{"providers.list", http.MethodGet, "/admin/api/providers", nil, http.StatusOK, nil},
		{"providers.create", http.MethodPost, "/admin/api/providers", provider, http.StatusCreated, []string{"id", "protocol"}},
		{"providers.get", http.MethodGet, "/admin/api/providers/contract-provider", nil, http.StatusOK, []string{"id", "auth"}},
		{"providers.update", http.MethodPut, "/admin/api/providers/contract-provider", gatewayprovider.Provider{ID: provider.ID, Name: "Renamed", BaseURL: provider.BaseURL, Protocol: provider.Protocol, Auth: provider.Auth, Models: provider.Models}, http.StatusOK, []string{"name"}},
		{"providers.test.invalid", http.MethodPost, "/admin/api/providers/test", gatewayprovider.Provider{}, http.StatusBadRequest, []string{"error"}},
		{"providers.delete", http.MethodDelete, "/admin/api/providers/contract-provider", nil, http.StatusOK, []string{"ok", "deleted"}},
		{"stats", http.MethodGet, "/admin/api/stats", nil, http.StatusOK, []string{"models", "series", "events", "total_frames"}},
		{"models.list", http.MethodGet, "/admin/api/models", nil, http.StatusOK, []string{"models", "off", "deleted"}},
		{"models.mask", http.MethodPost, "/admin/api/models", map[string]any{"masks_id": []string{"vendor/model-a"}}, http.StatusOK, []string{"off"}},
		{"models.delete", http.MethodDelete, "/admin/api/models/vendor/model-a", nil, http.StatusOK, []string{"deleted"}},
		{"models.restore", http.MethodPost, "/admin/api/models/vendor/model-a/restore", nil, http.StatusOK, []string{"restored"}},
		{"models.retry", http.MethodPost, "/admin/api/models/retry-failed", nil, http.StatusOK, []string{"ok"}},
		{"automode.get", http.MethodGet, "/admin/api/config/automode", nil, http.StatusOK, []string{"model", "chat_responses"}},
		{"automode.set", http.MethodPost, "/admin/api/config/automode", map[string]any{"model": models.DefaultModel}, http.StatusOK, []string{"model"}},
		{"cycle.get", http.MethodGet, "/admin/api/config/cycle", nil, http.StatusOK, []string{"name", "models", "available", "current"}},
		{"cycle.set", http.MethodPost, "/admin/api/config/cycle", map[string]any{"models": []string{models.DefaultModel}}, http.StatusOK, []string{"models"}},
		{"search.get", http.MethodGet, "/admin/api/config/search", nil, http.StatusOK, []string{"url"}},
		{"search.set", http.MethodPost, "/admin/api/config/search", map[string]any{"url": "https://www.google.com/search?q={query}"}, http.StatusOK, []string{"url"}},
		{"chromes.get", http.MethodGet, "/admin/api/chromes", nil, http.StatusOK, []string{"enabled", "state", "chromes", "ready"}},
		{"chromes.invalid", http.MethodPost, "/admin/api/chromes", map[string]any{"mode": "bogus"}, http.StatusBadRequest, []string{"error"}},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			res := makeRequest(t, engine, tc.method, tc.path, tc.body, nil)
			if res.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d: %s", tc.method, tc.path, res.Code, tc.want, res.Body.String())
			}
			assertJSON(t, res)
			if len(tc.keys) == 0 {
				return
			}
			m := object(t, res.Body.Bytes())
			for _, key := range tc.keys {
				if _, ok := m[key]; !ok {
					t.Fatalf("%s %s missing %q: %s", tc.method, tc.path, key, res.Body.String())
				}
			}
		})
	}

	// A model id with a slash must survive the catch-all path intact.
	res := makeRequest(t, engine, http.MethodDelete, "/admin/api/models/vendor/model-a", nil, nil)
	if got := object(t, res.Body.Bytes())["deleted"]; got != "vendor/model-a" {
		t.Fatalf("deleted = %v, want vendor/model-a", got)
	}

	res = makeRequest(t, engine, http.MethodGet, "/admin/api/models", nil, nil)
	var list struct {
		Models  []map[string]any `json:"models"`
		Deleted []string         `json:"deleted"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	gone := true
	for _, m := range list.Models {
		if m["id"] == "vendor/model-a" {
			gone = false
		}
	}
	if !gone {
		t.Fatalf("deleted model still listed: %s", res.Body.String())
	}

	res = makeRequest(t, engine, http.MethodGet, "/admin/api/nope", nil, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown API path = %d, want 404", res.Code)
	}
}

func TestAdminAPIContract_SecurityBoundaries(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	t.Run("token protects API and assets uniformly", func(t *testing.T) {
		engine := contractEngine(t, p, "contract-secret", nil)
		for _, path := range []string{"/admin/api/stats", "/admin/app.js"} {
			res := makeRequest(t, engine, http.MethodGet, path, nil, nil)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("GET %s unauthenticated = %d, want 401", path, res.Code)
			}
		}
		auth := map[string]string{"Authorization": "Bearer contract-secret"}
		for _, path := range []string{"/admin/api/stats", "/admin/app.js"} {
			res := makeRequest(t, engine, http.MethodGet, path, nil, auth)
			if res.Code != http.StatusOK {
				t.Fatalf("GET %s authenticated = %d, want 200: %s", path, res.Code, res.Body.String())
			}
		}
	})

	t.Run("cross-origin mutation is rejected", func(t *testing.T) {
		engine := contractEngine(t, p, "", nil)
		body := map[string]any{"masks_id": []string{}}
		res := makeRequest(t, engine, http.MethodPost, "/admin/api/models", body, map[string]string{"Origin": "https://evil.example"})
		if res.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST = %d, want 403", res.Code)
		}
		res = makeRequest(t, engine, http.MethodPost, "/admin/api/models", body, map[string]string{"Origin": "http://127.0.0.1:8080"})
		if res.Code != http.StatusOK {
			t.Fatalf("same-origin POST = %d, want 200: %s", res.Code, res.Body.String())
		}
	})
}
