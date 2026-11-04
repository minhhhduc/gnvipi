package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
	"github.com/sirupsen/logrus"
)

var redactionLogging sync.Once

func configureSecretRedaction(adminToken, gatewayKeys, captchaToken string) {
	keys := []string{adminToken, captchaToken}
	for _, key := range strings.Split(gatewayKeys, ",") {
		keys = append(keys, strings.TrimSpace(key))
	}
	redact.Default.Update(keys...)
	redactionLogging.Do(func() {
		log.SetOutput(redact.Default.WrapWriter(log.Writer()))
		logrus.AddHook(redact.Default.Hook())
	})
}

func refreshProviderSecrets(list []customProvider, v2 ...gatewayprovider.Provider) {
	keys := make([]string, 0, len(list)+len(v2))
	for i := range list {
		keys = append(keys, list[i].APIKey)
	}
	for i := range v2 {
		key := v2[i].Auth.Key
		if v2[i].Auth.KeyEnv != "" {
			key = os.Getenv(v2[i].Auth.KeyEnv)
		}
		keys = append(keys, key)
	}
	redact.Default.Update(keys...)
}

const diagnosticBodyLimit = 1 << 20

// Error details and SSE frames must be complete before disclosure filtering.
// Buffering is bounded; successful non-SSE output is passed through untouched.
type secretResponseWriter struct {
	gin.ResponseWriter
	ctx            context.Context
	protocol       gwerr.Protocol
	code           int
	logicalSize    int
	virtualWritten bool
	body           bytes.Buffer
	overflow       bool
	pending        []byte
	streamErr      error
	responseID     string
}

func secretResponseRedaction() gin.HandlerFunc {
	return func(c *gin.Context) {
		base := c.Writer
		w := &secretResponseWriter{ResponseWriter: base, ctx: c.Request.Context(), protocol: requestErrorProtocol(c.Request.URL.Path)}
		c.Writer = w
		c.Next()
		c.Writer = base
		if err := w.finish(); err != nil {
			log.Printf("write redacted diagnostic response: %v", err)
		}
	}
}

func requestErrorProtocol(path string) gwerr.Protocol {
	if strings.HasPrefix(path, "/v1/messages") {
		return gwerr.ProtocolAnthropicMessages
	}
	if strings.HasPrefix(path, "/v1/responses") {
		return gwerr.ProtocolOpenAIResponses
	}
	return gwerr.ProtocolOpenAIChat
}

func (w *secretResponseWriter) Status() int {
	if w.code != 0 {
		return w.code
	}
	return w.ResponseWriter.Status()
}
func (w *secretResponseWriter) Written() bool { return w.virtualWritten || w.ResponseWriter.Written() }
func (w *secretResponseWriter) Size() int {
	if w.Status() >= 400 {
		return w.logicalSize
	}
	return w.ResponseWriter.Size()
}
func (w *secretResponseWriter) WriteHeader(code int) {
	if w.ResponseWriter.Written() {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.code = code
	if code < 400 {
		w.redactHeaders()
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *secretResponseWriter) WriteHeaderNow() {
	if w.Status() >= 400 {
		w.virtualWritten = true
		return
	}
	w.redactHeaders()
	w.ResponseWriter.WriteHeaderNow()
}
func (w *secretResponseWriter) Flush() {
	if w.Status() < 400 {
		w.redactHeaders()
		w.ResponseWriter.Flush()
	}
}
func (w *secretResponseWriter) WriteString(value string) (int, error) {
	if w.Status() < 400 && !isEventStream(w.Header().Get("Content-Type")) {
		w.redactHeaders()
		return w.ResponseWriter.WriteString(value)
	}
	return w.writePayload([]byte(value))
}
func (w *secretResponseWriter) Write(p []byte) (int, error) { return w.writePayload(p) }
func (w *secretResponseWriter) writePayload(p []byte) (int, error) {
	if w.Status() >= 400 {
		w.virtualWritten = true
		w.logicalSize += len(p)
		if w.overflow || w.body.Len()+len(p) > diagnosticBodyLimit {
			w.overflow = true
			return len(p), errors.New("upstream diagnostic body exceeded limit")
		}
		return w.body.Write(p)
	}
	if !isEventStream(w.Header().Get("Content-Type")) {
		w.redactHeaders()
		return w.ResponseWriter.Write(p)
	}
	length := len(p)
	for len(p) > 0 {
		step := min(len(p), 32<<10)
		w.pending = append(w.pending, p[:step]...)
		p = p[step:]
		for {
			end := completeSSEFrame(w.pending)
			if end == 0 {
				break
			}
			if err := w.writeFrame(w.pending[:end]); err != nil {
				w.streamErr = err
				return length, err
			}
			if end == len(w.pending) {
				w.pending = w.pending[:0]
			} else {
				w.pending = w.pending[end:]
			}
		}
		if len(w.pending) > diagnosticBodyLimit {
			w.streamErr = errors.New("upstream SSE diagnostic frame exceeded limit")
			return length, w.streamErr
		}
	}
	return length, nil
}

// Locate a complete leading comment or blank-line-delimited event without
// changing line endings. Successful model event frames remain byte-exact.
func completeSSEFrame(data []byte) int {
	lineStart := 0
	for i := 0; i < len(data); {
		if data[i] != '\n' && data[i] != '\r' {
			i++
			continue
		}
		next := i + 1
		if data[i] == '\r' && next < len(data) && data[next] == '\n' {
			next++
		}
		if i == lineStart || lineStart == 0 && data[0] == ':' {
			return next
		}
		lineStart, i = next, next
	}
	return 0
}

func (w *secretResponseWriter) writeFrame(frame []byte) error {
	if len(frame) > diagnosticBodyLimit {
		return errors.New("upstream SSE diagnostic frame exceeded limit")
	}
	w.redactHeaders()
	if len(frame) > 0 && frame[0] == ':' {
		// Complete leading comments contain metadata, never model output.
		original := string(frame)
		if clean := redact.Default.Text(original); clean != original {
			frame = []byte(clean)
		}
		_, err := w.ResponseWriter.Write(frame)
		return err
	}
	reader := sse.NewReader(bytes.NewReader(frame))
	defer reader.Close()
	event, err := reader.Next(w.ctx)
	if errors.Is(err, io.EOF) {
		_, writeErr := w.ResponseWriter.Write(frame)
		return writeErr
	}
	if err != nil {
		return err
	}
	var envelope struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			ID    string          `json:"id"`
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	parsed := json.Unmarshal([]byte(event.Data), &envelope)
	if w.protocol == gwerr.ProtocolOpenAIResponses && parsed == nil && (event.Type == "response.created" || envelope.Type == "response.created") {
		w.responseID = envelope.Response.ID
	}
	isError := event.Type == "error" || event.Type == "response.failed" || parsed == nil && (envelope.Type == "error" || envelope.Type == "response.failed" ||
		len(envelope.Error) > 0 && string(bytes.TrimSpace(envelope.Error)) != "null" ||
		len(envelope.Response.Error) > 0 && string(bytes.TrimSpace(envelope.Response.Error)) != "null")
	if !isError {
		_, err := w.ResponseWriter.Write(frame)
		return err
	}
	clean, err := redact.Default.JSON([]byte(event.Data))
	if err != nil {
		return err
	}
	event.Data = string(clean)
	event.ID, event.Type = redact.Default.Text(event.ID), redact.Default.Text(event.Type)
	writer, err := sse.NewWriter(w.ResponseWriter)
	if err != nil {
		return err
	}
	return writer.WriteEvent(event)
}

func (w *secretResponseWriter) redactHeaders() {
	if w.ResponseWriter.Written() {
		return
	}
	for name, values := range w.Header() {
		for i := range values {
			values[i] = redact.Default.HeaderValue(name, values[i])
		}
	}
}

func (w *secretResponseWriter) finish() error {
	if w.Status() >= 400 {
		var body []byte
		var err error
		if w.overflow {
			body, _, err = gwerr.Encode(w.protocol, &gwerr.Error{Kind: gwerr.KindUpstreamError, Status: w.Status(), Message: "upstream diagnostic response exceeded limit"})
			w.Header().Set("Content-Type", "application/json")
		} else {
			body, err = redact.Default.JSON(w.body.Bytes())
		}
		if err != nil {
			return err
		}
		w.redactHeaders()
		w.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(w.Status())
		w.ResponseWriter.WriteHeaderNow()
		_, err = w.ResponseWriter.Write(body)
		return err
	}
	if w.streamErr != nil {
		w.redactHeaders()
		return gwerr.WriteSSEError(w.ResponseWriter, w.protocol, &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: w.streamErr.Error()}, w.responseID)
	}
	if len(w.pending) > 0 {
		return w.writeFrame(w.pending)
	}
	return nil
}
