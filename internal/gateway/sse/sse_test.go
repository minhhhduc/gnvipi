package sse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chunkReader releases its data in fixed-size chunks to exercise streams
// split across arbitrary read boundaries.
type chunkReader struct {
	data  string
	chunk int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.data == "" {
		return 0, io.EOF
	}
	n := len(p)
	if n > c.chunk {
		n = c.chunk
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// errReader fails every read with a non-EOF error.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("connection reset") }

// drain reads every event from r, failing the test on unexpected errors.
func drain(t *testing.T, r io.Reader) []Event {
	t.Helper()
	rd := NewReader(r)
	var out []Event
	for {
		ev, err := rd.Next(context.Background())
		if err == nil {
			out = append(out, ev)
			continue
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("unexpected read error: %v", err)
		}
		return out
	}
}

func TestReaderOpenAIChat(t *testing.T) {
	const stream = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":" world"}}]}

data: [DONE]

`
	got := drain(t, strings.NewReader(stream))
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[0].Type != "" || !strings.Contains(got[0].Data, `"content":"Hello"`) {
		t.Errorf("first event wrong: %+v", got[0])
	}
	if !got[2].IsDone() {
		t.Errorf("third event should be the [DONE] sentinel: %+v", got[2])
	}
	if got[0].IsDone() || got[1].IsDone() {
		t.Errorf("non-terminal events reported as done")
	}
}

func TestReaderAnthropicMessages(t *testing.T) {
	const stream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":10}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: message_stop
data: {"type":"message_stop"}

`
	got := drain(t, strings.NewReader(stream))
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	wantTypes := []string{"message_start", "content_block_delta", "message_stop"}
	for i, want := range wantTypes {
		if got[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, got[i].Type, want)
		}
	}
	if !strings.Contains(got[2].Data, "message_stop") {
		t.Errorf("message_stop data wrong: %q", got[2].Data)
	}
}

func TestReaderOpenAIResponses(t *testing.T) {
	const stream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}

event: response.completed
data: {"type":"response.completed"}

`
	got := drain(t, strings.NewReader(stream))
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[1].Type != "response.output_text.delta" || got[1].Data != `{"type":"response.output_text.delta","delta":"Hello"}` {
		t.Errorf("delta event wrong: %+v", got[1])
	}
}

func TestReaderLineEndings(t *testing.T) {
	const wantData = `{"text":"Hello"}`
	tests := []struct {
		name   string
		stream string
	}{
		{"LF", "event: delta\ndata: " + wantData + "\n\n"},
		{"CRLF", "event: delta\r\ndata: " + wantData + "\r\n\r\n"},
		{"CR", "event: delta\rdata: " + wantData + "\r\r"},
		{"mixed", "event: delta\r\ndata: " + wantData + "\r\n\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := drain(t, strings.NewReader(tt.stream))
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1: %+v", len(got), got)
			}
			if got[0].Type != "delta" || got[0].Data != wantData {
				t.Errorf("event = %+v, want type=delta data=%q", got[0], wantData)
			}
		})
	}
}

func TestReaderGrammar(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		want    []Event
		wantEOF bool
	}{
		{
			name:   "multi-line data joins with newline",
			stream: "data: line1\ndata: line2\ndata: line3\n\n",
			want:   []Event{{Data: "line1\nline2\nline3"}},
		},
		{
			name:   "empty first data line is a real line",
			stream: "data:\ndata: hi\n\n",
			want:   []Event{{Data: "\nhi"}},
		},
		{
			name:   "one optional space after colon",
			stream: "event:  ping\ndata:  hi\n\n",
			want:   []Event{{Type: " ping", Data: " hi"}},
		},
		{
			name:   "colon in value",
			stream: "data: {\"a\":1}\n\n",
			want:   []Event{{Data: `{"a":1}`}},
		},
		{
			name:   "id and retry fields",
			stream: "id: 42\nretry: 5000\ndata: hi\n\n",
			want:   []Event{{Data: "hi", ID: "42", Retry: 5000}},
		},
		{
			name:   "invalid retry is ignored",
			stream: "retry: bogus\ndata: hi\n\n",
			want:   []Event{{Data: "hi"}},
		},
		{
			name:   "leading space in data survives a double space",
			stream: "data:  padded\n\n",
			want:   []Event{{Data: " padded"}},
		},
		{
			name:    "comments do not dispatch",
			stream:  ": keepalive\n: another\n\n",
			want:    nil,
			wantEOF: true,
		},
		{
			name:   "comment interleaved with fields",
			stream: ": note\nevent: ping\ndata: hi\n\n",
			want:   []Event{{Type: "ping", Data: "hi"}},
		},
		{
			name:   "trailing event without final blank line still yields",
			stream: "event: ping\ndata: hi",
			want:   []Event{{Type: "ping", Data: "hi"}},
		},
		{
			name:   "blank lines between events are not events",
			stream: "data: a\n\n\n\ndata: b\n\n",
			want:   []Event{{Data: "a"}, {Data: "b"}},
		},
		{
			name:    "empty stream",
			stream:  "",
			want:    nil,
			wantEOF: true,
		},
		{
			name:   "unknown field is ignored but dispatches",
			stream: "foo: bar\ndata: hi\n\n",
			want:   []Event{{Data: "hi"}},
		},
		{
			name:   "event with only an id",
			stream: "id: 7\n\n",
			want:   []Event{{ID: "7"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := drain(t, strings.NewReader(tt.stream))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d events %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReaderByteAtATime(t *testing.T) {
	const stream = "event: message_start\ndata: {\"id\":\"msg_1\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	want := drain(t, strings.NewReader(stream))
	for _, chunk := range []int{1, 2, 3, 5, 7, 64} {
		got := drain(t, &chunkReader{data: stream, chunk: chunk})
		if len(got) != len(want) {
			t.Fatalf("chunk %d: got %d events, want %d", chunk, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("chunk %d: event %d = %+v, want %+v", chunk, i, got[i], want[i])
			}
		}
	}
}

func TestReaderLargeEvent(t *testing.T) {
	payload := strings.Repeat("a", 1<<20) // 1 MiB, well past bufio.Scanner's 64 KiB cap
	got := drain(t, strings.NewReader("data: "+payload+"\n\n"))
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if len(got[0].Data) != len(payload) {
		t.Errorf("data length = %d, want %d", len(got[0].Data), len(payload))
	}
	if got[0].Data != payload {
		t.Errorf("large payload truncated or corrupted")
	}
}

func TestReaderErrorPropagation(t *testing.T) {
	rd := NewReader(errReader{})
	_, err := rd.Next(context.Background())
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("first Next = %v, want the read error", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %v, want connection reset", err)
	}
	// The terminal error must replay, not become io.EOF.
	for i := 0; i < 3; i++ {
		if _, err := rd.Next(context.Background()); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("follow-up Next #%d = %v, want the read error replayed", i, err)
		}
	}
}

func TestReaderContextCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		if err := pr.Close(); err != nil {
			t.Errorf("close pipe reader: %v", err)
		}
		if err := pw.Close(); err != nil {
			t.Errorf("close pipe writer: %v", err)
		}
	})
	rd := NewReader(pr)

	// The pump is blocked reading an empty pipe; cancel before Next blocks.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rd.Next(ctx); err == nil {
		t.Fatal("canceled Next returned nil error")
	}

	// Cancellation does not stop the pump: a later Next still delivers.
	go func() {
		if _, err := fmt.Fprint(pw, "event: ping\ndata: hi\n\n"); err != nil {
			t.Errorf("write to pipe: %v", err)
		}
	}()
	ev, err := rd.Next(context.Background())
	if err != nil {
		t.Fatalf("Next after cancellation: %v", err)
	}
	if ev.Type != "ping" || ev.Data != "hi" {
		t.Errorf("event = %+v", ev)
	}
}

func TestReaderEOFBeforeFirstEvent(t *testing.T) {
	rd := NewReader(strings.NewReader(""))
	if _, err := rd.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: Next = %v, want io.EOF", err)
	}
}

func TestWriterNormalizesCRData(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// CRLF and lone-CR endings inside Data must each split into their own
	// "data:" line rather than carrying a stray \r or staying one line.
	if err := w.WriteData("a\r\nb\rc"); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	want := "data: a\ndata: b\ndata: c\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	// And the reader agrees: any \r is a line terminator per the SSE
	// grammar, so the round trip yields one event with joined lines.
	got := drain(t, strings.NewReader("data: a\r\ndata: b\rdata: c\r\n\r\n"))
	if len(got) != 1 || got[0].Data != "a\nb\nc" {
		t.Errorf("round trip = %+v, want one event with joined lines", got)
	}
}

func TestReaderCloseReleasesPump(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		if err := pr.Close(); err != nil {
			t.Errorf("close pipe reader: %v", err)
		}
		if err := pw.Close(); err != nil {
			t.Errorf("close pipe writer: %v", err)
		}
	})
	rd := NewReader(pr)
	t.Cleanup(func() { rd.Close() })

	go func() {
		if _, err := fmt.Fprint(pw, "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"); err != nil {
			t.Errorf("write to pipe: %v", err)
		}
	}()

	// Take only the first of the two events: the pump parses both from one
	// read and then blocks sending the second, which no one will ask for.
	if _, err := rd.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	rd.Close()

	// The released pump closes the events channel, so a later Next sees
	// the stream as ended instead of hanging on an event never sent.
	nextDone := make(chan error, 1)
	go func() {
		_, err := rd.Next(context.Background())
		nextDone <- err
	}()
	select {
	case err := <-nextDone:
		if !errors.Is(err, io.EOF) {
			t.Errorf("Next after Close = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next after Close hung: Close did not release the pump")
	}
}

func TestReaderCloseIsIdempotent(t *testing.T) {
	rd := NewReader(strings.NewReader(""))
	rd.Close()
	rd.Close()
	if _, err := rd.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("Next after double Close = %v, want io.EOF", err)
	}
}

func TestNewWriter(t *testing.T) {
	t.Run("rejects non-flusher", func(t *testing.T) {
		w, err := NewWriter(nonFlusher{})
		if err == nil {
			t.Fatal("NewWriter on a non-flusher succeeded")
		}
		if !errors.Is(err, ErrNotFlusher) {
			t.Errorf("error = %v, want ErrNotFlusher", err)
		}
		if w != nil {
			t.Errorf("NewWriter returned a writer alongside the error")
		}
	})
	t.Run("accepts http.Flusher", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if _, err := NewWriter(rec); err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
	})
}

// nonFlusher implements http.ResponseWriter without http.Flusher.
type nonFlusher struct{}

func (nonFlusher) Header() http.Header        { return http.Header{} }
func (nonFlusher) Write([]byte) (int, error)  { return 0, nil }
func (nonFlusher) WriteHeader(statusCode int) {}

func TestWriterEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	id := "42"
	if err := w.WriteEvent(Event{Type: "message_start", Data: `{"id":"msg_1"}`, ID: id, Retry: 5000}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if err := w.WriteData("hello\nworld"); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if err := w.Comment("keepalive"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := w.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !rec.Flushed {
		t.Error("writer did not flush after an event")
	}
	want := "event: message_start\ndata: {\"id\":\"msg_1\"}\nid: 42\nretry: 5000\n\n" +
		"data: hello\ndata: world\n\n" +
		": keepalive\n" +
		"data: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestWriterLeadingSpaceRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteData(" padded value"); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	got := drain(t, strings.NewReader(rec.Body.String()))
	if len(got) != 1 || got[0].Data != " padded value" {
		t.Errorf("round trip lost the leading space: %+v", got)
	}
}

func TestWriterSurfacesClientDisconnect(t *testing.T) {
	w, err := NewWriter(&failingResponseWriter{httptest.NewRecorder()})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteEvent(Event{Data: "hi"}); err == nil {
		t.Error("WriteEvent on a dead client returned nil error")
	}
	if err := w.WriteData("hi"); err == nil {
		t.Error("WriteData on a dead client returned nil error")
	}
	if err := w.Comment("ka"); err == nil {
		t.Error("Comment on a dead client returned nil error")
	}
	if err := w.Done(); err == nil {
		t.Error("Done on a dead client returned nil error")
	}
}

// failingResponseWriter fails every write while still flushing.
type failingResponseWriter struct {
	*httptest.ResponseRecorder
}

func (f *failingResponseWriter) Write(b []byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func FuzzReader(f *testing.F) {
	f.Add([]byte("data: hi\n\n"))
	f.Add([]byte("event: a\rdata: b\r\r"))
	f.Add([]byte(": c\nretry: x\ndata:\n\n"))
	f.Add([]byte("data"))
	f.Fuzz(func(t *testing.T, data []byte) {
		rd := NewReader(bytes.NewReader(data))
		for {
			if _, err := rd.Next(context.Background()); err != nil {
				return
			}
		}
	})
}
