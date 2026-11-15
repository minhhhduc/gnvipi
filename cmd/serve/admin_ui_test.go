package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func adminUIGet(t *testing.T, engine *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestAdminUI_ServesEmbeddedSPA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerAdminUI(engine)
	engine.Use(adminConsoleRoot)
	// CLIProxyAPI owns GET / and the console middleware answers it first.
	engine.GET("/", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"message": "CLI Proxy API Server"}) })

	for _, tc := range []struct{ path, ctype string }{
		{"/", "text/html"},
		{"/playground", "text/html"},
		{"/api", "text/html"},
		{"/models", "text/html"},
		{"/config", "text/html"},
		{"/chromes", "text/html"},
		{"/tokens", "text/html"},
		{"/users", "text/html"},
		{"/usage", "text/html"},
		{"/audit", "text/html"},
		{"/admin/app.css", "text/css"},
		{"/admin/app.js", "javascript"},
	} {
		res := adminUIGet(t, engine, tc.path)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", tc.path, res.Code)
		}
		if ct := res.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctype) {
			t.Fatalf("GET %s Content-Type = %q, want %s", tc.path, ct, tc.ctype)
		}
	}

	body := adminUIGet(t, engine, "/").Body.String()
	if !strings.Contains(body, "/admin/app.css") || !strings.Contains(body, "/admin/app.js") {
		t.Fatalf("index.html does not reference its local assets")
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatalf("index.html references a third-party origin")
	}
}

func TestAdminUI_LegacyAdminURLsRedirectToCleanPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerAdminUI(engine)

	for _, tc := range []struct{ path, want string }{
		{"/admin", "/"},
		{"/admin/", "/"},
		{"/admin?page=models", "/models"},
		{"/admin?page=stats", "/"},
		{"/admin?page=bogus", "/"},
	} {
		res := adminUIGet(t, engine, tc.path)
		if res.Code != http.StatusSeeOther || res.Header().Get("Location") != tc.want {
			t.Fatalf("GET %s = %d %q, want 303 %q", tc.path, res.Code, res.Header().Get("Location"), tc.want)
		}
	}
}

// The SPA and the JSON API share /admin; registering both must not panic and
// each must still answer on its own paths.
func TestAdminUI_CoexistsWithJSONAPI(t *testing.T) {
	p := setupTestPrefs(t)
	oldPrefs := prefs
	prefs = p
	t.Cleanup(func() { prefs = oldPrefs })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerAdminAPIRoutes(engine, p, nil, nil, nil)
	registerAdminUI(engine)
	engine.Use(adminConsoleRoot)
	engine.GET("/", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"message": "CLI Proxy API Server"}) })

	if res := adminUIGet(t, engine, "/"); res.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.Code)
	}
	res := adminUIGet(t, engine, "/admin/api/stats")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/stats = %d, want 200: %s", res.Code, res.Body.String())
	}
	if ct := res.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET /admin/api/stats Content-Type = %q, want application/json", ct)
	}
}
