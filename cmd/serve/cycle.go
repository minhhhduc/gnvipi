package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/models"
)

// cycleConfig maps the always-advertised "cycle" model to real models.
type cycleConfig struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

func (p *modelPrefs) cycleSnapshot() (cfg cycleConfig, index int) {
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
	cfg.Models = append([]string(nil), cfg.Models...)
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
		if !found && p.providerStore != nil {
			storeList := p.providerStore.List()
			for i := range storeList {
				provider := &storeList[i]
				if !provider.Enabled {
					continue
				}
				for _, m := range provider.Models {
					if m.Hidden {
						continue
					}
					if m.Alias == id || m.UpstreamModel == id {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}
		if !found && p.resolver != nil && id != routing.VirtualAutoMode && id != routing.VirtualCycle {
			if route, err := p.resolver.Resolve(context.Background(), id, ""); err == nil && route.Kind == routing.Custom {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unknown cycle model %q", id)
		}
	}
	if err := p.commitPrefs(func(file *prefsFile) error {
		file.Cycle = &cycleConfig{Name: "cycle", Models: append([]string(nil), cfg.Models...)}
		return nil
	}, false); err != nil {
		return err
	}
	p.mu.Lock()
	p.cycleIndex = 0
	p.mu.Unlock()
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
		rc := core.RequestContextFrom(c.Request.Context())
		if rc == nil {
			rc = core.NewRequestContext(c.Request.Header.Get(core.RequestIDHeader))
			c.Request = c.Request.WithContext(core.WithRequestContext(c.Request.Context(), rc))
		}
		if rc.ClientModel == "" {
			rc.ClientModel = cfg.Name
		}
		var last *cycleAttemptWriter
		for offset := range cfg.Models {
			if path == "/v1/messages/count_tokens" && offset > 0 {
				break
			}
			id := cfg.Models[(start+offset)%len(cfg.Models)]
			if p.isModelUnavailable(id) {
				continue
			}
			rewritten, _ := rewriteJSONModel(body, cfg.Name, id)
			writer := &cycleAttemptWriter{dest: c.Writer, header: make(http.Header), recorded: make(chan struct{})}
			attempt := core.NewRequestContext(rc.ID)
			attempt.ClientModel = rc.ClientModel
			writer.request = attempt
			attemptContext := core.WithRequestContext(core.WithRecorder(c.Request.Context(), writer), attempt)
			req := c.Request.Clone(attemptContext)
			req.Body = io.NopCloser(bytes.NewReader(rewritten))
			req.ContentLength = int64(len(rewritten))
			engine.ServeHTTP(writer, req)
			if writer.status == 0 {
				writer.status = http.StatusOK
			}
			if writer.committed || c.Request.Context().Err() != nil || path == "/v1/messages/count_tokens" || !retryableCycleStatus(writer.status) {
				writer.commit()
				writer.complete(rc, c.Request.Context())
				c.Abort()
				return
			}
			p.advanceCycle(cfg.Name, id)
			last = writer
		}
		if last != nil {
			last.commit()
			last.complete(rc, c.Request.Context())
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available cycle models; configure cycle in the gateway"})
		}
		c.Abort()
	}
}

type cycleAttemptWriter struct {
	dest       http.ResponseWriter
	header     http.Header
	status     int
	buffer     bytes.Buffer
	committed  bool
	request    *core.RequestContext
	completion *core.Completion
	recorded   chan struct{}
}

func (w *cycleAttemptWriter) Record(completion *core.Completion) {
	w.completion = completion
	close(w.recorded)
}

// Only the committed attempt contributes normalized client-request accounting.
// Legacy attempts never install a core recorder and leave completion nil.
func (w *cycleAttemptWriter) complete(request *core.RequestContext, ctx context.Context) {
	if w.request == nil || !w.request.RecordingStarted() {
		return
	}
	<-w.recorded
	completion := w.completion
	finished := completion.Request
	request.InboundProtocol, request.UpstreamProtocol = finished.InboundProtocol, finished.UpstreamProtocol
	request.ResolvedProvider, request.UpstreamModel, request.Stream = finished.ResolvedProvider, finished.UpstreamModel, finished.Stream
	if request.FirstByte.IsZero() {
		request.FirstByte = finished.FirstByte
	}
	request.BeginRecording(ctx, nil)
	request.Complete(completion.Usage, completion.Err)
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
		if _, err := w.dest.Write(w.buffer.Bytes()); err != nil {
			log.Printf("write cycle response: %v", err)
		}
	}
}
