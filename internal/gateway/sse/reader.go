package sse

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
)

// Event is one dispatched SSE event. Type is the "event:" field value
// (empty when the event is unnamed), Data is the "data:" lines joined
// with "\n", ID is the "id:" field, and Retry is the "retry:" field in
// milliseconds.
type Event struct {
	Type  string
	Data  string
	ID    string
	Retry int
}

// IsDone reports whether the event is the OpenAI stream terminator: the
// unnamed event whose data is the literal "[DONE]".
func (e Event) IsDone() bool {
	return e.Type == "" && e.Data == "[DONE]"
}

// Reader parses an SSE event stream. Call Next to receive each event in
// order; Next returns io.EOF once the stream has ended, and replays the
// underlying read error on every subsequent call if the stream failed.
//
// Next honors the caller's context: when ctx is done while an event is
// pending, Next returns ctx.Err() promptly. Canceling the context does
// not stop the internal pump goroutine — it keeps reading the underlying
// stream until that stream ends or errors. Closing the stream (for an
// HTTP response, the body) is the supported way to release it.
type Reader struct {
	events chan Event
	term   chan error // buffered 1: the pump's terminal error, if any
	err    error      // sticky, set from Next once the stream terminates
}

// NewReader returns a Reader reading events from r.
func NewReader(r io.Reader) *Reader {
	rd := &Reader{
		events: make(chan Event),
		term:   make(chan error, 1),
	}
	go rd.pump(r)
	return rd
}

func (rd *Reader) pump(r io.Reader) {
	defer close(rd.events)
	br := bufio.NewReader(r)
	for {
		ev, err := readEvent(br)
		if err != nil {
			rd.term <- err
			return
		}
		rd.events <- ev
	}
}

// Next returns the next event in the stream, or io.EOF at the end.
func (rd *Reader) Next(ctx context.Context) (Event, error) {
	if rd.err != nil {
		return Event{}, rd.err
	}
	select {
	case ev, ok := <-rd.events:
		if ok {
			return ev, nil
		}
		select {
		case err := <-rd.term:
			rd.err = err
		default:
			rd.err = io.EOF
		}
		if rd.err == nil {
			rd.err = io.EOF
		}
		return Event{}, rd.err
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

// readEvent parses one dispatched event from br. It returns io.EOF when
// the stream has ended with nothing left to dispatch.
func readEvent(br *bufio.Reader) (Event, error) {
	var eb eventBuilder
	for {
		line, eof, err := nextLine(br)
		if err != nil {
			return Event{}, err
		}
		if line != "" {
			eb.apply(line)
		}
		if eof {
			// The stream ended mid-event; flush what was buffered so a
			// truncated stream still yields its final content.
			if eb.pending {
				return eb.ev, nil
			}
			return Event{}, io.EOF
		}
		if line == "" && eb.pending {
			return eb.ev, nil
		}
	}
}

// eventBuilder accumulates the field lines of one event.
type eventBuilder struct {
	ev                Event
	pending, dataSeen bool
}

// apply merges one line into the event being built.
func (eb *eventBuilder) apply(line string) {
	if strings.HasPrefix(line, ":") {
		return // comment line: ignored, and does not open an event
	}
	field, value := line, ""
	if i := strings.IndexByte(line, ':'); i >= 0 {
		field, value = line[:i], line[i+1:]
	}
	// The spec strips exactly one leading space after the colon.
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		eb.ev.Type = value
	case "data":
		if eb.dataSeen {
			eb.ev.Data += "\n"
		}
		eb.ev.Data += value
		eb.dataSeen = true
	case "id":
		eb.ev.ID = value
	case "retry":
		if n, err := strconv.Atoi(value); err == nil {
			eb.ev.Retry = n
		}
	}
	eb.pending = true
}

// nextLine reads one line, handling LF, CRLF and lone-CR endings. The
// returned line has its terminator removed. eof is true when the
// underlying reader reached its end (and br said so) before any further
// bytes, in which case line may still carry the trailing unterminated
// content.
func nextLine(br *bufio.Reader) (line string, eof bool, err error) {
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err == io.EOF {
			return b.String(), true, nil
		}
		if err != nil {
			return b.String(), false, err
		}
		switch c {
		case '\n':
			return strings.TrimSuffix(b.String(), "\r"), false, nil
		case '\r':
			next, err := br.ReadByte()
			if err == nil && next != '\n' {
				// Lone CR: the byte we peeked starts the next line.
				if err := br.UnreadByte(); err != nil {
					return b.String(), false, err
				}
			}
			return b.String(), err == io.EOF, nil
		default:
			b.WriteByte(c)
		}
	}
}
