package main

import (
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	adminui "github.com/minhhhduc/gnvipi/web/admin"
)

// consolePages are the clean root paths the SPA serves; the dashboard is "/".
var consolePages = []string{"playground", "api", "models", "config", "chromes", "tokens", "users", "usage", "audit"}

// legacyConsolePaths maps the old ?page= values to their clean path.
var legacyConsolePaths = map[string]string{
	"stats": "/", "playground": "/playground", "api": "/api", "models": "/models",
	"config": "/config", "chromes": "/chromes", "tokens": "/tokens", "users": "/users",
	"usage": "/usage", "audit": "/audit",
}

// adminShellGetPath reports whether path is an Admin UI shell page or redirect
// served by registerAdminUI or adminConsoleRoot.
func adminShellGetPath(path string) bool {
	return path == "/" || path == "/admin" || path == "/admin/" || slices.Contains(consolePages, strings.TrimPrefix(path, "/"))
}

// adminSurfacePath reports every URL that belongs to the console: the /admin
// API and legacy endpoints, the assets, and the clean page paths.
func adminSurfacePath(path string) bool {
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		return true
	}
	return adminShellGetPath(path)
}

// consolePathFor maps a legacy ?page= value to its clean path.
func consolePathFor(page string) (string, bool) {
	target, ok := legacyConsolePaths[page]
	if !ok {
		return "/", false
	}
	return target, true
}

// registerAdminUI serves the standalone admin SPA. Assets are streamed
// straight out of the embedded FS on each request, so idle gateways keep no
// admin page, stylesheet or script in the heap.
//
// Only exact paths are registered: a wildcard here would swallow the JSON API
// and the legacy /admin endpoints.
func registerAdminUI(engine *gin.Engine) {
	serveIndex := func(c *gin.Context) {
		http.ServeFileFS(c.Writer, c.Request, adminui.FS, "index.html")
	}
	serveAsset := func(c *gin.Context) {
		name := strings.TrimPrefix(c.Request.URL.Path, "/admin")
		http.ServeFileFS(c.Writer, c.Request, adminui.FS, strings.TrimPrefix(name, "/"))
	}
	// Old console URLs land on their clean path; unknown pages go home.
	redirect := func(c *gin.Context) {
		target, _ := consolePathFor(c.Query("page"))
		c.Redirect(http.StatusSeeOther, target)
	}
	engine.GET("/admin", redirect)
	engine.GET("/admin/", redirect)
	engine.GET("/admin/app.css", serveAsset)
	engine.GET("/admin/app.js", serveAsset)

	const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="9" fill="#4f46e5"/><text x="16" y="21" text-anchor="middle" fill="#ffffff" font-family="system-ui,sans-serif" font-size="14" font-weight="800">GN</text></svg>`
	serveFavicon := func(c *gin.Context) {
		c.Data(http.StatusOK, "image/svg+xml", []byte(faviconSVG))
	}
	engine.GET("/admin/favicon.svg", serveFavicon)
	engine.GET("/favicon.ico", serveFavicon)

	for _, page := range consolePages {
		engine.GET("/"+page, serveIndex)
	}
}

// adminConsoleRoot serves the dashboard at "/" without registering the route:
// the embedded CLIProxyAPI already owns GET / for its server banner. The
// middleware runs after adminAccess, so the page is gated like every other.
func adminConsoleRoot(c *gin.Context) {
	if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/" {
		http.ServeFileFS(c.Writer, c.Request, adminui.FS, "index.html")
		c.Abort()
		return
	}
	c.Next()
}
