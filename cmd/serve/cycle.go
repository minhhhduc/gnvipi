package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/models"
)

// cycleConfig maps the always-advertised "cycle" model to real models.
type cycleConfig struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

func (p *modelPrefs) cycleSnapshot() (cycleConfig, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cycle == nil {
		return cycleConfig{Name: "cycle"}, 0
	}
	return cycleConfig{Name: "cycle", Models: append([]string(nil), p.cycle.Models...)}, p.cycleIndex
}

func (p *modelPrefs) setCycle(cfg cycleConfig) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	cfg.Name = "cycle"
	for i := range cfg.Models {
		cfg.Models[i] = strings.TrimSpace(cfg.Models[i])
	}
	seen := map[string]bool{}
	for _, id := range cfg.Models {
		if id == "cycle" || id == "automode" || seen[id] || p.isHidden(id) {
			return fmt.Errorf("invalid or duplicate cycle model %q", id)
		}
		seen[id] = true
		if _, ok := models.Models[id]; ok {
			continue
		}
		found := false
		for _, provider := range p.listProviders() {
			if provider.modelID() == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown cycle model %q", id)
		}
	}
	p.mu.Lock()
	old, oldIndex := p.cycle, p.cycleIndex
	p.cycle = &cycleConfig{Name: "cycle", Models: append([]string(nil), cfg.Models...)}
	p.cycleIndex = 0
	p.mu.Unlock()
	if err := p.save(); err != nil {
		p.mu.Lock()
		p.cycle, p.cycleIndex = old, oldIndex
		p.mu.Unlock()
		return err
	}
	return nil
}

func (p *modelPrefs) advanceCycle(alias, failed string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cycle != nil && alias == "cycle" && len(p.cycle.Models) > 0 && p.cycle.Models[p.cycleIndex] == failed {
		p.cycleIndex = (p.cycleIndex + 1) % len(p.cycle.Models)
	}
}

func retryableCycleStatus(status int) bool {
	return status == 404 || status == 408 || status == 410 || status == 429 || status >= 500
}

// cycleRouter retries before any error response reaches the client. Successful
// SSE responses are passed through immediately; a stream already in progress
// cannot safely be replayed on another model.
func cycleRouter(p *modelPrefs, engine http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method != http.MethodPost || path != "/v1/messages" && path != "/v1/messages/count_tokens" && path != "/v1/chat/completions" && path != "/v1/responses" {
			c.Next()
			return
		}
		cfg, start := p.cycleSnapshot()
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			c.Abort()
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Model != cfg.Name {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}
		var last *cycleAttemptWriter
		for offset := range cfg.Models {
			if path == "/v1/messages/count_tokens" && offset > 0 {
				break
			}
			id := cfg.Models[(start+offset)%len(cfg.Models)]
			if p.isHidden(id) {
				continue
			}
			rewritten, _ := rewriteJSONModel(body, cfg.Name, id)
			req := c.Request.Clone(c.Request.Context())
			req.Body = io.NopCloser(bytes.NewReader(rewritten))
			req.ContentLength = int64(len(rewritten))
			writer := &cycleAttemptWriter{dest: c.Writer, header: make(http.Header)}
			engine.ServeHTTP(writer, req)
			if writer.status == 0 {
				writer.status = http.StatusOK
			}
			if path == "/v1/messages/count_tokens" || !retryableCycleStatus(writer.status) {
				writer.commit()
				c.Abort()
				return
			}
			p.advanceCycle(cfg.Name, id)
			last = writer
		}
		if last != nil {
			last.commit()
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available cycle models; configure cycle in the gateway"})
		}
		c.Abort()
	}
}

type cycleAttemptWriter struct {
	dest      http.ResponseWriter
	header    http.Header
	status    int
	buffer    bytes.Buffer
	committed bool
}

func (w *cycleAttemptWriter) Header() http.Header { return w.header }
func (w *cycleAttemptWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if !retryableCycleStatus(status) {
		w.commit()
	}
}
func (w *cycleAttemptWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		return w.dest.Write(data)
	}
	if w.buffer.Len() < 1<<20 {
		w.buffer.Write(data[:min(len(data), (1<<20)-w.buffer.Len())])
	}
	return len(data), nil
}
func (w *cycleAttemptWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		if f, ok := w.dest.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *cycleAttemptWriter) CloseNotify() <-chan bool {
	if n, ok := w.dest.(http.CloseNotifier); ok {
		return n.CloseNotify()
	}
	return make(chan bool)
}
func (w *cycleAttemptWriter) commit() {
	if w.committed {
		return
	}
	for key, values := range w.header {
		w.dest.Header()[key] = append([]string(nil), values...)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.dest.WriteHeader(status)
	w.committed = true
	if w.buffer.Len() > 0 {
		_, _ = w.dest.Write(w.buffer.Bytes())
	}
}
