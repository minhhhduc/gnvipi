package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

// modelsAndHealthMiddleware handles /v1/models catalog discovery and /healthz
// status checks before CLIProxyAPI's default routes. runtime is the
// database-backed configuration snapshot; it is nil in file mode, which keeps
// the previous response shape.
func modelsAndHealthMiddleware(getModels func() []gatewayModel, exec *nvidia.Executor, runtime *providerRuntime) gin.HandlerFunc {
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
			if runtime != nil {
				// A degraded instance keeps serving the last known good
				// snapshot, so the status stays 200 and the body carries the
				// detail an operator needs (spec §8.3).
				health := runtime.health(c.Request.Context())
				status := "ok"
				if health.Degraded {
					status = "degraded"
					out["ok"] = false
				}
				out["status"] = status
				out["degraded"] = health.Degraded
				out["db_ok"] = health.DBOK
				out["watcher_ok"] = health.WatcherOK
				out["snapshot_version"] = health.SnapshotVersion
				out["config_version"] = health.ConfigVersion
				out["snapshot_age"] = health.SnapshotAge.Seconds()
				if health.Detail != "" {
					out["detail"] = health.Detail
				}
			}
			c.JSON(http.StatusOK, out)
			c.Abort()
		default:
			c.Next()
		}
	}
}
