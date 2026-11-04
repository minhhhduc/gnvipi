package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// Match the integrated diagnostic filter and legacy statistics capture bounds.
const diagnosticBodyLimit = 1 << 20
const statCaptureLimit = 256 << 10

func copyResponseHeaders(dst, src http.Header, requestID string) {
	nominated := make(map[string]bool)
	for name, values := range src {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					nominated[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if provider.ForbiddenForwardHeader(canonical) || nominated[canonical] || canonical == core.RequestIDHeader {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, redact.Default.HeaderValue(canonical, value))
		}
	}
	dst.Del("Content-Length")
	dst.Set(core.RequestIDHeader, requestID)
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func (h *Relay) copyResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rc *core.RequestContext) (ir.Usage, error) {
	copyResponseHeaders(w.Header(), resp.Header, rc.ID)
	streamed := isEventStream(resp.Header.Get("Content-Type"))
	diagnostic := resp.StatusCode < 200 || resp.StatusCode >= 300
	var statusErr error
	if diagnostic {
		statusErr = upstreamStatusError(resp)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); streamed && encoding != "" && !strings.EqualFold(encoding, "identity") {
		// Opaque compressed frames cannot be classified before disclosure, and
		// recompressing would violate the successful-byte relay contract.
		err := &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "upstream SSE must use identity content encoding"}
		if diagnostic {
			err.Status = resp.StatusCode
		}
		w.Header().Del("Content-Encoding")
		writeErr := gwerr.Write(w, gwerr.Protocol(h.provider.Protocol), err)
		return ir.Usage{}, errors.Join(statusErr, err, writeErr)
	}
	if diagnostic && !streamed {
		body, err := readDiagnostic(resp.Body, rc)
		if err != nil {
			w.Header().Del("Content-Encoding")
			responseErr := &gwerr.Error{Kind: gwerr.KindUpstreamError,
				Status: resp.StatusCode, Message: "upstream diagnostic response could not be safely disclosed"}
			writeErr := gwerr.Write(w, gwerr.Protocol(h.provider.Protocol), responseErr)
			return ir.Usage{}, errors.Join(statusErr, err, writeErr)
		}
		decoded, err := decodeDiagnostic(body, resp.Header.Get("Content-Encoding"))
		if err != nil {
			w.Header().Del("Content-Encoding")
			responseErr := &gwerr.Error{Kind: gwerr.KindUpstreamError,
				Status: resp.StatusCode, Message: "upstream diagnostic encoding could not be safely disclosed"}
			writeErr := gwerr.Write(w, gwerr.Protocol(h.provider.Protocol), responseErr)
			return ir.Usage{}, errors.Join(statusErr, err, writeErr)
		}
		clean, err := redact.Default.JSON(decoded)
		if err != nil {
			return ir.Usage{}, errors.Join(statusErr, err)
		}
		usage := scrapeUsageObject(decoded, ir.Usage{})
		if !bytes.Equal(clean, decoded) {
			body = clean
			w.Header().Del("Content-Encoding")
		}
		w.WriteHeader(resp.StatusCode)
		return usage, errors.Join(statusErr, writeBytes(w, body))
	}
	controller := http.NewResponseController(w)
	w.WriteHeader(resp.StatusCode)
	filter := streamFilter{ctx: ctx, writer: w, diagnostic: diagnostic}
	var captured bytes.Buffer
	buf := make([]byte, 4096)
	var copyErr error
	downstreamFailed := false
	for {
		if err := ctx.Err(); err != nil {
			copyErr = err
			break
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			rc.MarkFirstByte()
			if streamed {
				copyErr = filter.write(buf[:n])
				if copyErr != nil && filter.writeErr != nil {
					downstreamFailed = true
				}
			} else {
				if left := statCaptureLimit - captured.Len(); left > 0 {
					_, _ = captured.Write(buf[:min(n, left)])
				}
				copyErr = writeBytes(w, buf[:n])
			}
			if copyErr != nil {
				break
			}
			if streamed {
				// Flush on every upstream read, not just HTTP 200 or [DONE].
				// Incomplete diagnostic frames stay private until classified.
				if err := controller.Flush(); err != nil {
					copyErr = err
					downstreamFailed = true
					break
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
	}
	if streamed {
		if copyErr == nil {
			copyErr = filter.finish()
			downstreamFailed = filter.writeErr != nil
			if copyErr == nil {
				if err := controller.Flush(); err != nil {
					copyErr = err
					downstreamFailed = true
				}
			}
		}
		if copyErr != nil && ctx.Err() == nil && !downstreamFailed {
			// A read/framing failure after headers needs the inbound SSE error
			// shape, not an HTTP status rewrite or a cross-protocol translation.
			writeErr := gwerr.WriteSSEError(w, gwerr.Protocol(h.provider.Protocol), &gwerr.Error{Kind: gwerr.KindFromErr(copyErr),
				Message: redact.Default.Text(copyErr.Error())}, filter.responseID)
			if writeErr == nil {
				writeErr = controller.Flush()
			}
			copyErr = errors.Join(copyErr, writeErr)
		}
		return filter.usage, errors.Join(statusErr, filter.upstreamErr, copyErr)
	}
	return scrapeUsageObject(captured.Bytes(), ir.Usage{}), errors.Join(statusErr, copyErr)
}

func writeBytes(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func readDiagnostic(body io.Reader, rc *core.RequestContext) ([]byte, error) {
	var captured bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			rc.MarkFirstByte()
			if captured.Len()+n > diagnosticBodyLimit {
				return nil, errors.New("upstream diagnostic body exceeded limit")
			}
			_, _ = captured.Write(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return captured.Bytes(), nil
			}
			return nil, err
		}
	}
}

func decodeDiagnostic(body []byte, encoding string) (decoded []byte, err error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		reader, openErr := gzip.NewReader(bytes.NewReader(body))
		if openErr != nil {
			return nil, openErr
		}
		defer func() {
			err = errors.Join(err, reader.Close())
		}()
		decoded, err = io.ReadAll(io.LimitReader(reader, diagnosticBodyLimit+1))
		if err != nil {
			return nil, err
		}
		if len(decoded) > diagnosticBodyLimit {
			return nil, errors.New("upstream decoded diagnostic body exceeded limit")
		}
		return decoded, nil
	default:
		return nil, errors.New("unsupported upstream diagnostic encoding")
	}
}

// streamFilter uses the integrated redactor's frame classification. Successful
// frames keep their exact bytes, field order, whitespace and line endings; only
// diagnostic events and leading comments pass through credential filtering.
// Retaining at most one bounded frame also permits scraping final usage after
// arbitrarily long streams, instead of losing it beyond the legacy capture cap.
type streamFilter struct {
	ctx         context.Context
	writer      http.ResponseWriter
	diagnostic  bool
	pending     []byte
	usage       ir.Usage
	responseID  string
	upstreamErr error
	writeErr    error
}

func (f *streamFilter) writeFrame(frame []byte) error {
	err := writeBytes(f.writer, frame)
	if err != nil {
		f.writeErr = err
	}
	return err
}

func (f *streamFilter) write(data []byte) error {
	f.pending = append(f.pending, data...)
	if f.diagnostic {
		body := bytes.TrimSpace(f.pending)
		if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
			// A mislabeled JSON body has no SSE data field. Keep the whole
			// object private, including blank lines, until JSON redaction.
			if len(f.pending) > diagnosticBodyLimit {
				return errors.New("upstream SSE diagnostic frame exceeded limit")
			}
			return nil
		}
	}
	for {
		end := completeSSEFrame(f.pending)
		if end == 0 {
			break
		}
		if err := f.frame(f.pending[:end]); err != nil {
			return err
		}
		if end == len(f.pending) {
			f.pending = f.pending[:0]
		} else {
			f.pending = f.pending[end:]
		}
	}
	if len(f.pending) > diagnosticBodyLimit {
		return errors.New("upstream SSE diagnostic frame exceeded limit")
	}
	return nil
}

// This is the legacy diagnostic boundary convention, with a trailing CR held
// until its following byte arrives so a split CRLF is not mistaken for a blank
// line. Leading keepalive comments can flush without a following blank line.
func completeSSEFrame(data []byte) int {
	lineStart := 0
	for i := 0; i < len(data); {
		if data[i] != '\n' && data[i] != '\r' {
			i++
			continue
		}
		next := i + 1
		if data[i] == '\r' {
			if next == len(data) {
				return 0
			}
			if data[next] == '\n' {
				next++
			}
		}
		if i == lineStart || lineStart == 0 && data[0] == ':' {
			return next
		}
		lineStart, i = next, next
	}
	return 0
}

func (f *streamFilter) finish() error {
	if len(f.pending) == 0 {
		return nil
	}
	return f.frame(f.pending)
}

func (f *streamFilter) frame(frame []byte) error {
	if len(frame) > diagnosticBodyLimit {
		return errors.New("upstream SSE diagnostic frame exceeded limit")
	}
	if len(frame) > 0 && frame[0] == ':' {
		original := string(frame)
		if clean := redact.Default.Text(original); clean != original {
			frame = []byte(clean)
		}
		return f.writeFrame(frame)
	}
	reader := sse.NewReader(bytes.NewReader(frame))
	defer reader.Close()
	event, err := reader.Next(f.ctx)
	if errors.Is(err, io.EOF) || err == nil && f.diagnostic && event.Data == "" {
		if f.diagnostic {
			clean, cleanErr := redact.Default.JSON(frame)
			if cleanErr != nil {
				return cleanErr
			}
			return f.writeFrame(clean)
		}
		return f.writeFrame(frame)
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
	f.usage = scrapeUsageObject([]byte(event.Data), f.usage)
	if event.Type == "response.created" || envelope.Type == "response.created" {
		f.responseID = envelope.Response.ID
	}
	isError := f.diagnostic || event.Type == "error" || event.Type == "response.failed" || parsed == nil &&
		(envelope.Type == "error" || envelope.Type == "response.failed" || nonNull(envelope.Error) || nonNull(envelope.Response.Error))
	if !isError {
		return f.writeFrame(frame)
	}
	f.upstreamErr = &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "upstream sent an SSE error event"}
	clean, err := redact.Default.JSON([]byte(event.Data))
	if err != nil {
		return err
	}
	cleanID, cleanType := redact.Default.Text(event.ID), redact.Default.Text(event.Type)
	if bytes.Equal(clean, []byte(event.Data)) && cleanID == event.ID && cleanType == event.Type && redact.Default.Text(string(frame)) == string(frame) {
		return f.writeFrame(frame)
	}
	event.Data, event.ID, event.Type = string(clean), cleanID, cleanType
	writer, err := sse.NewWriter(f.writer)
	if err != nil {
		f.writeErr = err
		return err
	}
	err = writer.WriteEvent(event)
	if err != nil {
		f.writeErr = err
	}
	return err
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
