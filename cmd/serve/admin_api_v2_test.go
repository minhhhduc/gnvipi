package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

func testAdminAPIEngine(t *testing.T, p *modelPrefs, catalog []*cliproxy.ModelInfo) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAccess("127.0.0.1:8080", "", false))
	registerAdminAPIRoutes(engine, p, catalog, nil, nil)
	return engine
}

func assertJSON(t *testing.T, res interface{ Header() http.Header }) {
	t.Helper()
	if ct := res.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestAdminAPIV2_ModelsCatalogAndMutations(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	catalog := []*cliproxy.ModelInfo{{ID: models.DefaultModel}, {ID: "openai/gpt-4o"}}
	engine := testAdminAPIEngine(t, p, catalog)

	res := makeRequest(t, engine, http.MethodGet, "/admin/api/models", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET models = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	var list struct {
		Models []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Publisher string `json:"publisher"`
			Hidden    bool   `json:"hidden"`
			Deleted   bool   `json:"deleted"`
			Custom    bool   `json:"custom"`
		} `json:"models"`
		Off     int      `json:"off"`
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(list.Models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(list.Models), res.Body.String())
	}
	if list.Models[0].ID != "openai/gpt-4o" && list.Models[1].ID != "openai/gpt-4o" {
		t.Fatalf("catalog missing openai/gpt-4o: %+v", list.Models)
	}
	for _, m := range list.Models {
		if m.Publisher == "" || m.Name == "" {
			t.Fatalf("model missing publisher/name: %+v", m)
		}
	}

	// Toggle visibility.
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/models", map[string]any{"masks_id": []string{"openai/gpt-4o"}}, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST models mask = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/models", nil, nil)
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal masked models: %v", err)
	}
	if list.Off != 1 || len(list.Deleted) != 0 {
		t.Fatalf("mask state off=%d deleted=%v, want off=1 deleted=[]", list.Off, list.Deleted)
	}
	for _, m := range list.Models {
		if m.ID == "openai/gpt-4o" && !m.Hidden {
			t.Fatal("openai/gpt-4o should be hidden")
		}
	}

	// Delete and restore a model whose id contains a slash.
	res = makeRequest(t, engine, http.MethodDelete, "/admin/api/models/"+models.DefaultModel, nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("DELETE model = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/models", nil, nil)
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal deleted models: %v", err)
	}
	found := false
	for _, m := range list.Models {
		if m.ID == models.DefaultModel {
			found = true
		}
	}
	if found {
		t.Fatalf("deleted model still listed: %s", res.Body.String())
	}

	res = makeRequest(t, engine, http.MethodPost, "/admin/api/models/"+models.DefaultModel+"/restore", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST restore = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/models", nil, nil)
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal restored models: %v", err)
	}
	found = false
	for _, m := range list.Models {
		if m.ID == models.DefaultModel {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored model not listed: %s", res.Body.String())
	}

	res = makeRequest(t, engine, http.MethodPost, "/admin/api/models/retry-failed", nil, nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"ok":true`) {
		t.Fatalf("POST retry-failed = %d: %s", res.Code, res.Body.String())
	}
}

func TestAdminAPIV2_ConfigAndStats(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	catalog := []*cliproxy.ModelInfo{{ID: models.DefaultModel}, {ID: "openai/gpt-4o"}}
	engine := testAdminAPIEngine(t, p, catalog)

	// Stats: the dashboard payload.
	res := makeRequest(t, engine, http.MethodGet, "/admin/api/stats", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET stats = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	var stats map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	for _, key := range []string{"models", "series", "events", "total_frames"} {
		if _, ok := stats[key]; !ok {
			t.Fatalf("stats missing %q: %s", key, res.Body.String())
		}
	}

	// Automode: read then set a real gateway model.
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/automode", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET automode = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/config/automode", map[string]any{"model": models.DefaultModel}, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST automode = %d: %s", res.Code, res.Body.String())
	}
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/automode", nil, nil)
	var auto struct {
		Model         string `json:"model"`
		ChatResponses bool   `json:"chat_responses"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &auto); err != nil {
		t.Fatalf("unmarshal automode: %v", err)
	}
	if auto.Model != models.DefaultModel {
		t.Fatalf("automode model = %q, want %q", auto.Model, models.DefaultModel)
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/config/automode", map[string]any{"model": "not/a-real-model"}, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("POST invalid automode = %d, want 400", res.Code)
	}

	// Cycle: read, set, read back.
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/cycle", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET cycle = %d: %s", res.Code, res.Body.String())
	}
	var cycle struct {
		Name      string   `json:"name"`
		Models    []string `json:"models"`
		Available []string `json:"available"`
		Current   string   `json:"current"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &cycle); err != nil {
		t.Fatalf("unmarshal cycle: %v", err)
	}
	if cycle.Name != "cycle" || cycle.Models == nil || cycle.Available == nil {
		t.Fatalf("cycle payload = %s", res.Body.String())
	}
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/config/cycle", map[string]any{"models": []string{models.DefaultModel}}, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST cycle = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/cycle", nil, nil)
	if err := json.Unmarshal(res.Body.Bytes(), &cycle); err != nil {
		t.Fatalf("unmarshal cycle: %v", err)
	}
	if len(cycle.Models) != 1 || cycle.Models[0] != models.DefaultModel || cycle.Current != models.DefaultModel {
		t.Fatalf("cycle after set = %s", res.Body.String())
	}

	// Search.
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/search", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET search = %d: %s", res.Code, res.Body.String())
	}
	searchURL := "https://www.google.com/search?q={query}"
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/config/search", map[string]any{"url": searchURL}, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("POST search = %d: %s", res.Code, res.Body.String())
	}
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/config/search", nil, nil)
	var search struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &search); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if search.URL != searchURL {
		t.Fatalf("search url = %q, want %q", search.URL, searchURL)
	}

	// Chrome status: no lifecycle in tests returns an idle, well-formed payload.
	res = makeRequest(t, engine, http.MethodGet, "/admin/api/chromes", nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET chromes = %d: %s", res.Code, res.Body.String())
	}
	assertJSON(t, res)
	var chromes struct {
		Enabled bool   `json:"enabled"`
		State   string `json:"state"`
		Chromes []any  `json:"chromes"`
		Ready   int    `json:"ready"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &chromes); err != nil {
		t.Fatalf("unmarshal chromes: %v", err)
	}
	if chromes.Enabled || chromes.State != "idle" || chromes.Chromes == nil || chromes.Ready != 0 {
		t.Fatalf("chromes payload = %s", res.Body.String())
	}
}

func TestAdminAPIV2_MethodAndUnknownPaths(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := testAdminAPIEngine(t, p, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/admin/api/stats"},
		{http.MethodPost, "/admin/api/stats"},
		{http.MethodGet, "/admin/api/config/unknown"},
		{http.MethodPost, "/admin/api/models/unknown-action"},
	} {
		res := makeRequest(t, engine, tc.method, tc.path, nil, nil)
		if res.Code != http.StatusNotFound && res.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, want 404/405", tc.method, tc.path, res.Code)
		}
	}
}
