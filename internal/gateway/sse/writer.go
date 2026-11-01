package sse

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Writer emits SSE events to an HTTP response, flushing after every event
// so streaming clients receive them as they are produced. All methods
// return write errors so callers detect a client disconnect instead of
// writing into a dead connection unnoticed.
type Writer struct {
	w http.ResponseWriter
	f http.Flusher
}

// ErrNotFlusher is returned by NewWriter when the response writer does not
// implement http.Flusher, and so cannot stream.
var ErrNotFlusher = errors.New("sse: response writer does not implement http.Flusher")

// NewWriter wraps a response writer. It fails when w cannot flush, since
// buffering the whole stream would defeat the purpose.
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrNotFlusher
	}
	return &Writer{w: w, f: f}, nil
}

// WriteEvent writes one complete event: the "event:" field when Type is
// set, one "data:" line per line of Data, the "id:" and "retry:" fields
// when set, a blank line to dispatch, then flushes.
func (w *Writer) WriteEvent(e Event) error {
	var b strings.Builder
	if e.Type != "" {
		writeField(&b, "event", e.Type)
	}
	for _, line := range strings.Split(e.Data, "\n") {
		writeField(&b, "data", line)
	}
	if e.ID != "" {
		writeField(&b, "id", e.ID)
	}
	if e.Retry > 0 {
		fmt.Fprintf(&b, "retry: %d\n", e.Retry)
	}
	b.WriteByte('\n')
	if _, err := w.w.Write([]byte(b.String())); err != nil {
		return err
	}
	w.f.Flush()
	return nil
}

// WriteData writes one unnamed event carrying data.
func (w *Writer) WriteData(data string) error {
	return w.WriteEvent(Event{Data: data})
}

// Comment writes a comment line (the SSE keepalive form) and flushes.
// Comments carry no data and are never dispatched as events.
func (w *Writer) Comment(text string) error {
	if _, err := fmt.Fprintf(w.w, ": %s\n", text); err != nil {
		return err
	}
	w.f.Flush()
	return nil
}

// Done writes the OpenAI stream terminator, the unnamed "[DONE]" event.
func (w *Writer) Done() error {
	return w.WriteData("[DONE]")
}

// Flush pushes any buffered bytes to the client.
func (w *Writer) Flush() {
	w.f.Flush()
}

func writeField(b *strings.Builder, field, value string) {
	fmt.Fprintf(b, "%s: %s\n", field, value)
}
