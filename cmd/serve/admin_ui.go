package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	adminui "github.com/minhhhduc/gnvipi/web/admin"
)

// registerAdminUI serves the standalone admin SPA. Assets are streamed
// straight out of the embedded FS on each request, so idle gateways keep no
// admin page, stylesheet or script in the heap.
//
// Only exact paths are registered: a wildcard here would swallow the JSON API
// and the legacy /admin endpoints.
func registerAdminUI(engine *gin.Engine) {
	serve := func(c *gin.Context) {
		name := strings.TrimPrefix(c.Request.URL.Path, "/admin")
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			name = "index.html"
		}
		http.ServeFileFS(c.Writer, c.Request, adminui.FS, name)
	}
	for _, path := range []string{"/admin", "/admin/", "/admin/app.css", "/admin/app.js"} {
		engine.GET(path, serve)
	}
}
