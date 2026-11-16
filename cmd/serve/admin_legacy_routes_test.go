package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func newAdminSurfaceTestEngine(t *testing.T, p *modelPrefs, state *authState) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAccess("127.0.0.1:8080", "", state))
	engine.Use(adminAPIAuthorization(state))
	registerAdminUI(engine)
	engine.Use(adminConsoleRoot)
	if state == nil {
		registerLegacyAdminRoutes(engine, nil, nil)
	}
	registerAdminAPIRoutes(engine, p, nil, nil, state)
	return engine
}

func TestDatabaseModeOmitsLegacyAdminRoutesOnLoopback(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	state := &authState{usersExist: &atomic.Bool{}}
	state.usersExist.Store(true)
	engine := newAdminSurfaceTestEngine(t, p, state)

	legacyCases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/admin/stats", ""},
		{http.MethodGet, "/admin/chromes", ""},
		{http.MethodPost, "/admin/chromes", `{"action":"start"}`},
		{http.MethodPost, "/admin/models", `{"masks_id":["meta/llama-3.3-70b-instruct"]}`},
		{http.MethodPost, "/admin/automode", `{"model":"automode"}`},
		{http.MethodPost, "/admin/cycle", `{"models":[]}`},
		{http.MethodPost, "/admin/search", `{"url":"https://www.bing.com/search?q={query}"}`},
		{http.MethodPost, "/admin/providers", `[]`},
		{http.MethodPost, "/admin/delete", `{"id":"meta/llama-3.3-70b-instruct"}`},
		{http.MethodPost, "/admin/retry-failed", `{}`},
	}

	for _, tc := range legacyCases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "http://127.0.0.1:8080"+tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", "http://127.0.0.1:8080")
			}
			res := httptest.NewRecorder()
			engine.ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404 (body %s)", tc.method, tc.path, res.Code, res.Body.String())
			}
		})
	}

	if got := p.searchTemplate(); got != "" {
		t.Fatalf("searchTemplate mutated to %q despite omitted legacy route", got)
	}

	// Verify /admin/api/* still requires authentication in database mode.
	apiReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/admin/api/stats", http.NoBody)
	apiRes := httptest.NewRecorder()
	engine.ServeHTTP(apiRes, apiReq)
	if apiRes.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin/api/stats = %d, want 401 (body %s)", apiRes.Code, apiRes.Body.String())
	}
}

func TestFileModeKeepsLegacyAdminRoutesOnLoopback(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	engine := newAdminSurfaceTestEngine(t, p, nil)

	getReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/admin/stats", http.NoBody)
	getRes := httptest.NewRecorder()
	engine.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusOK {
		t.Fatalf("GET /admin/stats = %d, want 200 (body %s)", getRes.Code, getRes.Body.String())
	}

	postReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:8080/admin/retry-failed", strings.NewReader(`{}`))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Origin", "http://127.0.0.1:8080")
	postRes := httptest.NewRecorder()
	engine.ServeHTTP(postRes, postReq)
	if postRes.Code != http.StatusOK {
		t.Fatalf("POST /admin/retry-failed = %d, want 200 (body %s)", postRes.Code, postRes.Body.String())
	}
}
