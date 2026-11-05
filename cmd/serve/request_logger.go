package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

// CLIProxyAPI's own compat client (the one path with no other recorder).
type statWriter struct {
	gin.ResponseWriter
	tee *statTee
}

func (w statWriter) Write(p []byte) (int, error) { return w.tee.Write(p) }

func (w statWriter) WriteString(s string) (int, error) { return w.tee.Write([]byte(s)) }

// requestLogger logs method, path, status and duration per request, enriched
// with the model name parsed from the JSON body (CLIProxyAPI's access log
// omits it). It also records stats for POST /v1/messages whose model resolves
// to a custom (admin-added, non-messages_only) provider: those requests are
// served by CLIProxyAPI's compat client, which never reports to GlobalStats.
// Disjoint from every other recorder — the nvidia executor only sees nvidia
// models, customOpenAICompatPassthrough only handles /v1/chat/completions, and
// messagesOnlyPassthrough only MessagesOnly==true providers — so no call is
// counted twice.
func requestLogger(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		model := ""
		if c.Request.Body != nil && c.Request.Method == http.MethodPost {
			body, err := io.ReadAll(c.Request.Body)
			if err == nil {
				var payload struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(body, &payload) == nil {
					model = payload.Model
				}
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
			} else {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
			}
		}

		// Custom providers are served by CLIProxyAPI's own client (which records
		// nothing) on every format except the two passthroughs — so capture
		// anything not owned by chat/completions-passthrough (!MessagesOnly) or
		// messages-passthrough (MessagesOnly). Disjoint from the nvidia executor
		// too; nothing double-counts.
		prov := lookupCustomProvider(p, model)
		record := c.Request.Method == http.MethodPost && prov != nil &&
			(c.Request.URL.Path != "/v1/chat/completions" || prov.MessagesOnly) &&
			(c.Request.URL.Path != "/v1/messages" || !prov.MessagesOnly)
		var tee *statTee
		if record {
			tee = &statTee{dst: c.Writer}
			c.Writer = statWriter{ResponseWriter: c.Writer, tee: tee}
		}

		c.Next()

		if record {
			nvidia.GlobalStats.RecordResponse(model, tee.buf.Bytes(), time.Since(start),
				isEventStream(c.Writer.Header().Get("Content-Type")), c.Writer.Status() >= 400 || c.Request.Context().Err() != nil)
		}

		log.Printf("%d | %s | %s | %s %q model=%q",
			c.Writer.Status(), time.Since(start).Round(time.Microsecond),
			c.ClientIP(), c.Request.Method, c.Request.URL.Path, model)
	}
}
