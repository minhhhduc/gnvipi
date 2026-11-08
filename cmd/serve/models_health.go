package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

// modelsAndHealthMiddleware handles /v1/models catalog discovery and /healthz
// status checks before CLIProxyAPI's default routes.
func modelsAndHealthMiddleware(getModels func() []gatewayModel, exec *nvidia.Executor) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method == http.MethodGet && (path == "/v1/models" || path == "/v1/models/") {
			data := getModels()
			if data == nil {
				data = []gatewayModel{}
			}
			firstID, lastID := "", ""
			if len(data) > 0 {
				firstID, lastID = data[0].ID, data[len(data)-1].ID
			}
			c.JSON(http.StatusOK, gin.H{
				"object":   "list",
				"data":     data,
				"has_more": false,
				"first_id": firstID,
				"last_id":  lastID,
			})
			c.Abort()
			return
		}

		if path != "/healthz" {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodHead:
			c.Status(http.StatusOK)
			c.Abort()
		case http.MethodGet:
			out := gin.H{"ok": true}
			if p := exec.Pool(); p != nil {
				fills, takes, errs, expired := p.Stats()
				out["pool"] = gin.H{
					"ready":   p.Ready(),
					"fills":   fills,
					"takes":   takes,
					"errors":  errs,
					"expired": expired,
				}
			}
			c.JSON(http.StatusOK, out)
			c.Abort()
		default:
			c.Next()
		}
	}
}
