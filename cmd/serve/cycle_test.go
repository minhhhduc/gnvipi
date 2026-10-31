package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/models"
)

func TestCycleConfigPersists(t *testing.T) {
	first := models.DefaultModel
	second := ""
	for id := range models.Models {
		if id != first {
			second = id
			break
		}
	}
	if second == "" {
		t.Fatal("test catalog needs two models")
	}
	path := filepath.Join(t.TempDir(), "models.json")
	p := &modelPrefs{path: path}
	if err := p.setCycle(cycleConfig{Models: []string{first, second}}); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	loaded.load(path)
	cfg, index := loaded.cycleSnapshot()
	if cfg.Name != "cycle" || len(cfg.Models) != 2 || cfg.Models[0] != first || cfg.Models[1] != second || index != 0 {
		t.Fatalf("loaded cycle: %#v index=%d", cfg, index)
	}
	if err := p.setCycle(cycleConfig{}); err != nil {
		t.Fatal(err)
	}
	loaded.load(path)
	cfg, _ = loaded.cycleSnapshot()
	if cfg.Name != "cycle" || len(cfg.Models) != 0 {
		t.Fatalf("empty cycle after reload: %#v", cfg)
	}
}

func TestCycleRetriesAndKeepsNextModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	engine := gin.New()
	engine.Use(cycleRouter(p, engine))
	var firstCalls, secondCalls int
	engine.POST("/v1/messages", func(c *gin.Context) {
		var body struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		switch body.Model {
		case "first":
			firstCalls++
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit"})
		case "second":
			secondCalls++
			c.JSON(http.StatusOK, gin.H{"model": "second"})
		default:
			c.Status(http.StatusBadRequest)
		}
	})
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle"}`)))
		return w
	}
	for i := 0; i < 2; i++ {
		w := request()
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
		var result map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["model"] != "second" {
			t.Fatalf("response %d: %s", i, w.Body.String())
		}
	}
	if firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("calls: first=%d second=%d", firstCalls, secondCalls)
	}
	if _, index := p.cycleSnapshot(); index != 1 {
		t.Fatalf("active index = %d", index)
	}
}

func TestCycleStreamsAfterRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	engine := gin.New()
	engine.Use(cycleRouter(p, engine))
	engine.POST("/v1/messages", func(c *gin.Context) {
		var body struct {
			Model string `json:"model"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.Model == "first" {
			c.String(http.StatusTooManyRequests, "limited")
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("event: message_start\ndata: {}\n\n"))
		c.Writer.Flush()
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle","stream":true}`)))
	if w.Code != http.StatusOK || !w.Flushed || !strings.Contains(w.Body.String(), "event: message_start") {
		t.Fatalf("stream response: status=%d flushed=%v body=%q", w.Code, w.Flushed, w.Body.String())
	}
}
