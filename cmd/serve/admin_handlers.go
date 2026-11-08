package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

func handleAdminDashboard(c *gin.Context, catalog []*cliproxy.ModelInfo) {
	page := c.DefaultQuery("page", "dashboard")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(adminHTML(page, catalog)))
}

func handleAdminStats(c *gin.Context) {
	interval := nvidia.StatsReadInterval()
	if n, err := strconv.Atoi(c.Query("frames")); err == nil && n >= 1 {
		ev, tf, err := nvidia.ReadFrame(n)
		if err != nil {
			if errors.Is(err, nvidia.ErrStatsReadLimited) {
				c.Header("Retry-After", strconv.FormatInt(int64((interval-1)/time.Second+1), 10))
				c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"frame": n, "total_frames": tf, "events": ev})
		return
	}
	adminStatsCache.Lock()
	if adminStatsCache.data == nil || time.Since(adminStatsCache.at) >= interval {
		adminStatsCache.data = gin.H{
			"models":          nvidia.GlobalStats.Snapshot(),
			"series":          nvidia.GlobalStats.Series(60),
			"series_by_model": nvidia.GlobalStats.ModelSeries(60),
			"events":          nvidia.GlobalStats.Events(200),
			"started_at":      nvidia.GlobalStats.StartedAt(),
			"total_frames":    nvidia.FramesCount(),
		}
		adminStatsCache.at = time.Now()
	}
	data := adminStatsCache.data
	adminStatsCache.Unlock()
	c.JSON(http.StatusOK, data)
}

func handleAdminModels(c *gin.Context) {
	var payload prefsFile
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := prefs.setHidden(payload.MasksID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"off": len(payload.MasksID)})
}

func handleAdminAutomode(c *gin.Context) {
	var payload struct {
		Model         string `json:"model"`
		ChatResponses *bool  `json:"chat_responses,omitempty"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := strings.TrimSpace(payload.Model)
	valid := false
	if id != "automode" && !prefs.isHidden(id) {
		_, valid = models.Models[id]
		if cycle, _ := prefs.cycleSnapshot(); cycle.Name == id && id != "" {
			valid = true
		}
		if !valid {
			for _, p := range prefs.listProviders() {
				if p.modelID() == id {
					valid = true
					break
				}
			}
		}
		if !valid && prefs.providerStore != nil {
			storeList := prefs.providerStore.List()
			for i := range storeList {
				p := &storeList[i]
				if !p.Enabled {
					continue
				}
				for _, m := range p.Models {
					if m.Hidden {
						continue
					}
					if m.Alias == id || m.UpstreamModel == id {
						valid = true
						break
					}
				}
				if valid {
					break
				}
			}
		}
		if !valid && prefs.resolver != nil && id != routing.VirtualAutoMode && id != routing.VirtualCycle {
			if route, err := prefs.resolver.Resolve(c.Request.Context(), id, ""); err == nil && route.Kind == routing.Custom {
				valid = true
			}
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "choose an enabled gateway model"})
		return
	}
	if err := prefs.setAutoModelConfig(id, payload.ChatResponses); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": id})
}

func handleAdminSearch(c *gin.Context) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	payload.URL = normalizeSearchURL(payload.URL)
	if payload.URL != "" && !validSearchURL(payload.URL) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enter an HTTPS search URL containing {query}"})
		return
	}
	if err := prefs.setSearchURL(payload.URL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": payload.URL})
}

func handleAdminCycle(c *gin.Context) {
	var payload cycleConfig
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := prefs.setCycle(payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": "cycle", "models": payload.Models})
}

func handleAdminProviders(c *gin.Context) {
	var list []customProvider
	if err := c.ShouldBindJSON(&list); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for i, p := range list {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.BaseURL) == "" ||
			strings.TrimSpace(p.Model) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint " + strconv.Itoa(i+1) +
				": name, base_url and model required"})
			return
		}
		if p.MessagesOnly && p.ResponsesOnly {
			c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint " + strconv.Itoa(i+1) +
				": choose one API format: responses or messages"})
			return
		}
		// Control characters (newline, tab, NUL) would let a value escape its
		// YAML scalar and inject config keys, so they never reach the file.
		for _, field := range []struct{ label, value string }{
			{"name", p.Name},
			{"base_url", p.BaseURL},
			{"api_key", p.APIKey},
			{"model", p.Model},
			{"alias", p.Alias},
		} {
			if strings.ContainsFunc(field.value, unicode.IsControl) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint " + strconv.Itoa(i+1) +
					": " + field.label + " contains control characters"})
				return
			}
		}
	}
	if err := prefs.setProviders(list); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": len(list)})
}

func handleAdminDelete(c *gin.Context) {
	var payload struct {
		ID      string `json:"id"`
		Restore bool   `json:"restore"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(payload.ID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	if payload.Restore {
		if err := prefs.removeDeleted(payload.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"restored": payload.ID})
	} else {
		if err := prefs.addDeleted(payload.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"deleted": payload.ID})
	}
}

func handleAdminRetryFailed(c *gin.Context) {
	deadModels.Clear()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
