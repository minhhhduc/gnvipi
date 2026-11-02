package main

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// gatewayRequestContext establishes identity before any middleware can abort.
// Accounting remains opt-in: legacy passthroughs keep their existing recorders.
func gatewayRequestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		if existing := core.RequestContextFrom(c.Request.Context()); existing != nil {
			// Cycle re-entry is still the same inbound request. Only the outer
			// dispatch owns first-byte tracking and terminal fallback recording.
			c.Request.Header.Set(core.RequestIDHeader, existing.ID)
			c.Header(core.RequestIDHeader, existing.ID)
			c.Next()
			return
		}
		id := ""
		if values := c.Request.Header.Values(core.RequestIDHeader); len(values) == 1 {
			id = values[0]
		}
		rc := core.NewRequestContext(id)
		switch c.Request.URL.Path {
		case "/v1/messages":
			rc.InboundProtocol = ir.ProtocolAnthropicMessages
		case "/v1/chat/completions":
			rc.InboundProtocol = ir.ProtocolOpenAIChat
		case "/v1/responses":
			rc.InboundProtocol = ir.ProtocolOpenAIResponses
		}
		c.Request = c.Request.WithContext(core.WithRequestContext(c.Request.Context(), rc))
		c.Request.Header.Set(core.RequestIDHeader, rc.ID)
		c.Header(core.RequestIDHeader, rc.ID)
		c.Writer = &requestContextWriter{ResponseWriter: c.Writer, request: rc}
		defer func() {
			err := c.Request.Context().Err()
			if err == nil && c.Writer.Status() >= http.StatusBadRequest {
				err = fmt.Errorf("HTTP status %d", c.Writer.Status())
			}
			rc.Complete(ir.Usage{}, err)
		}()
		c.Next()
	}
}

type requestContextWriter struct {
	gin.ResponseWriter
	request *core.RequestContext
}

func (w *requestContextWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.request.MarkFirstByte()
	}
	return n, err
}

func (w *requestContextWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if n > 0 {
		w.request.MarkFirstByte()
	}
	return n, err
}

func (w *requestContextWriter) WriteHeaderNow() {
	w.ResponseWriter.WriteHeaderNow()
	w.request.MarkFirstByte()
}

func (w *requestContextWriter) Flush() {
	w.ResponseWriter.Flush()
	w.request.MarkFirstByte()
}
