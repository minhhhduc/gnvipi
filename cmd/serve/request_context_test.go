package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

type requestContextRecorder struct {
	calls atomic.Int32
	done  chan core.Completion
}

func (r *requestContextRecorder) Record(c core.Completion) {
	r.calls.Add(1)
	r.done <- c
}

func TestGatewayRequestContextAcceptsAndGeneratesSafeIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, inbound := range []string{"client-request-42", "", "bad\nheader", "bad\rheader", "with space", strings.Repeat("x", 129), "non-ascii-α"} {
		t.Run(inbound, func(t *testing.T) {
			e := gin.New()
			e.Use(gatewayRequestContext())
			var rc *core.RequestContext
			e.POST("/v1/messages", func(c *gin.Context) {
				rc = core.RequestContextFrom(c.Request.Context())
				if rc == nil || rc.InboundProtocol != ir.ProtocolAnthropicMessages || c.Request.Header.Get(core.RequestIDHeader) != rc.ID {
					t.Fatal("request context was not established before the handler")
				}
				c.String(http.StatusOK, "first byte")
				c.String(http.StatusOK, "second byte")
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", http.NoBody)
			req.Header.Set(core.RequestIDHeader, inbound)
			e.ServeHTTP(w, req)
			id := w.Header().Get(core.RequestIDHeader)
			if id == "" || len(w.Header().Values(core.RequestIDHeader)) != 1 || id != rc.ID || rc.FirstByte.IsZero() || rc.FirstByte.Before(rc.Start) {
				t.Fatalf("response ID=%q context=%+v", id, rc)
			}
			if inbound == "client-request-42" && id != inbound {
				t.Fatalf("accepted ID changed: %q", id)
			}
			if inbound != "client-request-42" && (id == inbound || strings.ContainsAny(id, "\r\n ") || len(id) > 128) {
				t.Fatalf("invalid ID was not safely replaced: %q", id)
			}
		})
	}
}

func TestGatewayRequestContextRejectsAmbiguousIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(gatewayRequestContext())
	e.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	seen := make(map[string]bool)
	for range 32 {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Add(core.RequestIDHeader, "first")
		req.Header.Add(core.RequestIDHeader, "second")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		id := w.Header().Get(core.RequestIDHeader)
		if id == "" || id == "first" || id == "second" || seen[id] {
			t.Fatalf("ambiguous or repeated ID: %q", id)
		}
		seen[id] = true
	}
}

func TestGatewayRequestContextCompletesOptedInTrafficOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, outcome := range []string{"success", "error", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			r := &requestContextRecorder{done: make(chan core.Completion, 8)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e := gin.New()
			e.Use(gatewayRequestContext())
			e.POST("/v1/chat/completions", func(c *gin.Context) {
				rc := core.RequestContextFrom(c.Request.Context())
				rc.ClientModel = "normalized/model"
				rc.BeginRecording(c.Request.Context(), r)
				switch outcome {
				case "success":
					c.String(http.StatusOK, "ok")
					rc.Complete(ir.Usage{InputTokens: 8, OutputTokens: 4}, nil)
				case "error":
					c.AbortWithStatus(http.StatusBadGateway)
				case "cancel":
					cancel()
					c.Abort()
				}
			})
			e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", http.NoBody))
			select {
			case c := <-r.done:
				if r.calls.Load() != 1 || (c.Err != nil) != (outcome != "success") {
					t.Fatalf("completion=%+v calls=%d", c, r.calls.Load())
				}
				if outcome == "success" && (c.Usage.InputTokens != 8 || c.Usage.OutputTokens != 4 || c.Request.FirstByte.IsZero()) {
					t.Fatalf("successful usage/timing lost: %+v", c)
				}
				if outcome == "cancel" && !errors.Is(c.Err, context.Canceled) {
					t.Fatalf("cancel recorded as %v", c.Err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request was not recorded")
			}
		})
	}
}
func TestGatewayRequestContextReusesCycleContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rc := core.NewRequestContext("original-cycle-request")
	r := &requestContextRecorder{done: make(chan core.Completion, 8)}
	rc.BeginRecording(t.Context(), r)
	e := gin.New()
	e.Use(gatewayRequestContext())
	e.POST("/v1/chat/completions", func(c *gin.Context) {
		if got := core.RequestContextFrom(c.Request.Context()); got != rc {
			t.Fatal("cycle re-entry replaced the original request context")
		}
		c.String(http.StatusOK, "nested result")
	})
	req := httptest.NewRequestWithContext(core.WithRequestContext(t.Context(), rc), http.MethodPost, "/v1/chat/completions", http.NoBody)
	req.Header.Set(core.RequestIDHeader, "replacement")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Header().Get(core.RequestIDHeader) != "original-cycle-request" || r.calls.Load() != 0 {
		t.Fatalf("nested dispatch changed identity or completed early: headers=%v calls=%d", w.Header(), r.calls.Load())
	}
	rc.Complete(ir.Usage{}, nil)
}

func TestGatewayRequestContextPropagatesIDsWithoutDoubleCountingLegacy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		path      string
		upstream  string
		body      string
		messages  bool
		responses bool
	}{
		{"chat", "/v1/chat/completions", "/v1/chat/completions", `{"model":"request-id-test/chat","messages":[]}`, false, false},
		{"responses", "/v1/responses", "/v1/responses", `{"model":"request-id-test/responses","input":"ping"}`, false, true},
		{"messages", "/v1/messages", "/v1/messages", `{"model":"request-id-test/messages","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`, true, false},
		{"anthropic_responses", "/v1/messages", "/v1/responses", `{"model":"request-id-test/anthropic_responses","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan struct{ id, path string }, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				observed <- struct{ id, path string }{req.Header.Get(core.RequestIDHeader), req.URL.Path}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(core.RequestIDHeader, "different-upstream-id")
				io.WriteString(w, `{"id":"resp_1","output":[],"usage":{"input_tokens":7,"output_tokens":3}}`)
			}))
			defer upstream.Close()
			oldCompat, oldMessages := compatClient, messagesOnlyClient
			t.Cleanup(func() { compatClient, messagesOnlyClient = oldCompat, oldMessages })
			compatClient, messagesOnlyClient = upstream.Client(), upstream.Client()
			prov := customProvider{Name: "request-id-test", Model: tc.name, BaseURL: upstream.URL, MessagesOnly: tc.messages, ResponsesOnly: tc.responses}
			p := &modelPrefs{providers: []customProvider{prov}}
			before := uint64(0)
			for _, row := range nvidia.GlobalStats.Snapshot() {
				if row.Model == prov.modelID() {
					before = row.Requests
				}
			}
			e := gin.New()
			e.Use(gatewayRequestContext(), anthropicResponsesPassthrough(p), customOpenAIResponsesPassthrough(p), customOpenAICompatPassthrough(p), messagesOnlyPassthrough(p), requestLogger(p))
			e.POST(tc.path, func(c *gin.Context) { t.Fatal("passthrough did not handle request") })
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set(core.RequestIDHeader, "request-"+tc.name)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			var got struct{ id, path string }
			select {
			case got = <-observed:
			case <-time.After(5 * time.Second):
				t.Fatalf("no upstream request: response=%d body=%s", w.Code, w.Body.String())
			}
			if w.Code != http.StatusOK || got.path != tc.upstream || got.id != "request-"+tc.name || w.Header().Get(core.RequestIDHeader) != got.id || len(w.Header().Values(core.RequestIDHeader)) != 1 {
				t.Fatalf("upstream=%+v response=%d headers=%v body=%s", got, w.Code, w.Header(), w.Body.String())
			}
			for _, row := range nvidia.GlobalStats.Snapshot() {
				if row.Model == prov.modelID() {
					if row.Requests != before+1 || row.InputTok == 0 || row.OutputTok == 0 {
						t.Fatalf("legacy traffic accounting changed: before=%d row=%+v", before, row)
					}
					return
				}
			}
			t.Fatal("legacy request was not accounted")
		})
	}
}
