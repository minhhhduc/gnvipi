package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/models"
)

func TestCycleConfigPersists(t *testing.T) {
	first := models.DefaultModel
	second := ""
	for id := range models.Snapshot() {
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
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	cfg, index := loaded.cycleSnapshot()
	if cfg.Name != "cycle" || len(cfg.Models) != 2 || cfg.Models[0] != first || cfg.Models[1] != second || index != 0 {
		t.Fatalf("loaded cycle: %#v index=%d", cfg, index)
	}
	if err := p.setCycle(cycleConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
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
		engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle"}`)))
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
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model == "first" {
			c.String(http.StatusTooManyRequests, "limited")
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		if _, err := c.Writer.WriteString("event: message_start\ndata: {}\n\n"); err != nil {
			t.Fatal(err)
		}
		c.Writer.Flush()
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle","stream":true}`)))
	if w.Code != http.StatusOK || !w.Flushed || !strings.Contains(w.Body.String(), "event: message_start") {
		t.Fatalf("stream response: status=%d flushed=%v body=%q", w.Code, w.Flushed, w.Body.String())
	}
}

func TestCycleDoesNotRetryAfterCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	engine := gin.New()
	engine.Use(cycleRouter(p, engine))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls []string
	engine.POST("/v1/messages", func(c *gin.Context) {
		var body struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, body.Model)
		cancel()
		if err := c.Request.Context().Err(); err != context.Canceled {
			t.Fatalf("cycle request lost cancellation: %v", err)
		}
		c.String(http.StatusServiceUnavailable, "canceled upstream")
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle"}`)))
	if len(calls) != 1 || calls[0] != "first" {
		t.Fatalf("canceled cycle replayed: %v", calls)
	}
	if _, index := p.cycleSnapshot(); index != 0 {
		t.Fatalf("cancellation advanced cycle to %d", index)
	}
}

// Cycle attempts must keep the authenticated identity so key-aware routing and
// stats attribution survive the engine re-entry.
func TestCycleCarriesUserAndKeyAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{cycle: &cycleConfig{Name: "cycle", Models: []string{"first"}}}
	engine := gin.New()
	engine.Use(cycleRouter(p, engine))
	var attemptUser string
	engine.POST("/v1/messages", func(c *gin.Context) {
		rc := core.RequestContextFrom(c.Request.Context())
		attemptUser = rc.UserID
		rc.KeyID, rc.ResolvedProvider = "key-cycled", "p1"
		rc.BeginRecording(c.Request.Context(), nil)
		rc.Complete(ir.Usage{}, nil)
		c.Status(http.StatusOK)
	})

	outer := core.NewRequestContext("cycle-attribution")
	outer.UserID = "u1"
	recorder := &requestContextRecorder{done: make(chan *core.Completion, 1)}
	ctx := core.WithRecorder(core.WithRequestContext(t.Context(), outer), recorder)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cycle","messages":[]}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if attemptUser != "u1" {
		t.Fatalf("attempt user = %q, want u1", attemptUser)
	}
	select {
	case completion := <-recorder.done:
		if completion.Request.UserID != "u1" || completion.Request.KeyID != "key-cycled" {
			t.Fatalf("completion attribution = user %q key %q, want u1 key-cycled", completion.Request.UserID, completion.Request.KeyID)
		}
	default:
		t.Fatal("cycle completion was not recorded")
	}
}
