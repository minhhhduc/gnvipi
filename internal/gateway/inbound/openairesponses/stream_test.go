package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// These fixtures catch missing lifecycle events, lost accumulated content,
// incomplete final responses, and tool IDs conflated with output item IDs.
func TestStreamGolden(t *testing.T) {
	cases := []struct {
		name   string
		events []ir.StreamEvent
	}{
		{"text", []ir.StreamEvent{
			ir.NewBlockStart(7, ir.NewTextBlock("")),
			ir.NewTextDelta(7, "Hello"), ir.NewTextDelta(7, " world"),
			ir.NewBlockStop(7), ir.NewUsageUpdate(ir.Usage{OutputTokens: 3}),
			ir.NewMessageStop(ir.StopEndTurn),
		}},
		{"reasoning-text", []ir.StreamEvent{
			ir.NewBlockStart(4, ir.NewReasoningBlock("", "opaque-signature")),
			ir.NewReasoningDelta(4, "Think first."), ir.NewBlockStop(4),
			ir.NewBlockStart(9, ir.NewTextBlock("")),
			ir.NewTextDelta(9, "42"), ir.NewBlockStop(9),
			ir.NewUsageUpdate(ir.Usage{OutputTokens: 7, ReasoningTokens: 5}),
			ir.NewMessageStop(ir.StopEndTurn),
		}},
		{"function-call", []ir.StreamEvent{
			ir.NewBlockStart(3, ir.NewToolUseBlock("call_weather", "weather", json.RawMessage(`{}`))),
			ir.NewToolArgumentsDelta(3, "call_weather", `{"city":`),
			ir.NewToolArgumentsDelta(3, "call_weather", `"Paris"}`),
			ir.NewBlockStop(3), ir.NewUsageUpdate(ir.Usage{OutputTokens: 6}),
			ir.NewMessageStop(ir.StopToolUse),
		}},
		{"failure", []ir.StreamEvent{
			ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewTextDelta(0, "Partial"),
			ir.NewUsageUpdate(ir.Usage{OutputTokens: 2}),
			ir.NewErrorEvent("rate_limit", "Try later"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := ir.NewMessageStart("resp_fixture", ir.RoleAssistant, "client-alias", ir.Usage{
				InputTokens: 10, CacheReadTokens: 4, CacheWriteTokens: 2,
			})
			stream := &eventStream{events: append([]ir.StreamEvent{start}, tc.events...)}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{Created: 1700000000}).EncodeStream(context.Background(), stream, writer); err != nil {
				t.Fatal(err)
			}
			if stream.closed {
				t.Fatal("encoder closed the caller-owned stream")
			}
			golden, err := os.ReadFile("testdata/" + tc.name + ".golden.sse")
			if err != nil {
				t.Fatal(err)
			}
			got, want := readWire(t, recorder.Body.String()), readWire(t, string(golden))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SSE differs from %s golden\ngot: %#v\nwant: %#v", tc.name, got, want)
			}
		})
	}
}

// Interleaved, non-dense IR indexes must not leak into Responses array indexes.
func TestStreamOutputIdentityAndOrdering(t *testing.T) {
	stream := &eventStream{events: []ir.StreamEvent{
		ir.NewMessageStart("resp_order", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewBlockStart(12, ir.NewTextBlock("")),
		ir.NewBlockStart(2, ir.NewToolUseBlock("call_2", "lookup", nil)),
		ir.NewTextDelta(12, "A"), ir.NewToolArgumentsDelta(2, "call_2", "{}"),
		ir.NewBlockStart(99, ir.NewTextBlock("")), ir.NewTextDelta(99, "B"),
		ir.NewBlockStop(2), ir.NewBlockStop(99), ir.NewBlockStop(12),
		ir.NewMessageStop(ir.StopEndTurn),
		ir.NewTextDelta(12, "must not appear"),
	}}
	wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, stream)
	if err != nil {
		t.Fatal(err)
	}
	items := make(map[int]string)
	finished := make(map[int]bool)
	for i, event := range wire {
		if event["sequence_number"] != float64(i) {
			t.Fatalf("event %d has sequence %v", i, event["sequence_number"])
		}
		typ := event["type"].(string)
		if typ == "response.output_item.added" {
			index := int(event["output_index"].(float64))
			if index != len(items) {
				t.Fatalf("non-dense output index: %d", index)
			}
			items[index] = event["item"].(map[string]any)["id"].(string)
		} else if itemID, ok := event["item_id"].(string); ok {
			index := int(event["output_index"].(float64))
			if items[index] != itemID || finished[index] {
				t.Fatalf("event references wrong or completed item: %v", event)
			}
			if content, ok := event["content_index"]; ok && content != float64(0) {
				t.Fatalf("wrong content index: %v", event)
			}
		} else if typ == "response.output_item.done" {
			index := int(event["output_index"].(float64))
			if items[index] != event["item"].(map[string]any)["id"] {
				t.Fatalf("item ID changed: %v", event)
			}
			finished[index] = true
		}
	}
	final := wire[len(wire)-1]
	if final["type"] != "response.completed" || len(finished) != 3 {
		t.Fatalf("unfinished response: %v", final)
	}
	output := final["response"].(map[string]any)["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output has %d items", len(output))
	}
	for i, value := range output {
		item := value.(map[string]any)
		if item["id"] != items[i] || item["status"] != "completed" {
			t.Fatalf("final identity or status changed: %v", item)
		}
	}
	if output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "A" ||
		output[1].(map[string]any)["arguments"] != "{}" ||
		output[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "B" {
		t.Fatalf("final output lost content: %v", output)
	}
	if stream.closed || stream.position != 11 {
		t.Fatalf("encoder consumed events after terminal or took source ownership: %+v", stream)
	}
}

func TestStreamIncompleteStatus(t *testing.T) {
	for _, tc := range []struct {
		stop   ir.StopReason
		reason string
	}{
		{ir.StopMaxTokens, "max_output_tokens"}, {ir.StopContentFilter, "content_filter"},
	} {
		t.Run(string(tc.stop), func(t *testing.T) {
			stream := &eventStream{events: []ir.StreamEvent{
				ir.NewMessageStart("resp_short", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewTextDelta(0, "partial"),
				ir.NewBlockStop(0), ir.NewMessageStop(tc.stop),
			}}
			wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, stream)
			if err != nil {
				t.Fatal(err)
			}
			final := wire[len(wire)-1]
			response := final["response"].(map[string]any)
			if final["type"] != "response.incomplete" || response["status"] != "incomplete" ||
				response["incomplete_details"].(map[string]any)["reason"] != tc.reason {
				t.Fatalf("incorrect incomplete response: %v", final)
			}
			if response["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "partial" {
				t.Fatal("incomplete response lost partial output")
			}
		})
	}
}

func TestStreamPrototypeContentAndStableCreation(t *testing.T) {
	before := time.Now().Unix()
	stream := &eventStream{events: []ir.StreamEvent{
		ir.NewMessageStart("", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewBlockStart(0, ir.NewTextBlock("seed")), ir.NewTextDelta(0, "ed"), ir.NewBlockStop(0),
		ir.NewBlockStart(1, ir.NewToolUseBlock("call_seed", "seed", json.RawMessage(`{"x":0}`))), ir.NewBlockStop(1),
		ir.NewMessageStop(ir.StopEndTurn),
	}}
	wire, err := encodeWire(t, context.Background(), StreamEncoder{}, stream)
	if err != nil {
		t.Fatal(err)
	}
	created := wire[0]["response"].(map[string]any)
	final := wire[len(wire)-1]["response"].(map[string]any)
	stamp := int64(created["created_at"].(float64))
	if stamp < before || stamp > time.Now().Unix() || created["id"] == "" || created["id"] != final["id"] || created["created_at"] != final["created_at"] {
		t.Fatalf("creation metadata not stable: %v / %v", created, final)
	}
	output := final["output"].([]any)
	if output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "seeded" || output[1].(map[string]any)["arguments"] != `{"x":0}` {
		t.Fatalf("prototype content lost: %v", output)
	}
}

func TestStreamInitialMessageContent(t *testing.T) {
	for _, tc := range []struct {
		stop   ir.StopReason
		status string
		reason string
	}{
		{ir.StopEndTurn, "completed", ""},
		{ir.StopMaxTokens, "incomplete", "max_output_tokens"},
		{ir.StopContentFilter, "incomplete", "content_filter"},
	} {
		t.Run(string(tc.stop), func(t *testing.T) {
			start := ir.NewMessageStart("resp_initial", ir.RoleAssistant, "alias", ir.Usage{
				InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4, CacheWriteTokens: 2, ReasoningTokens: 1,
			})
			text := ir.NewTextBlock("initial\ntext \"雪\"")
			text.Index = 6
			tool := ir.NewToolUseBlock("call_initial", "lookup", json.RawMessage(` { "n": 9007199254740993, "text": "雪" } `))
			tool.Index = 2
			start.Message.Content = []ir.Block{text, tool}
			stream := &eventStream{events: []ir.StreamEvent{start, ir.NewMessageStop(tc.stop)}}
			wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, stream)
			if err != nil {
				t.Fatal(err)
			}
			final := wire[len(wire)-1]
			response := final["response"].(map[string]any)
			if final["type"] != "response."+tc.status || response["status"] != tc.status {
				t.Fatalf("initial content prevented terminal response: %v", final)
			}
			if tc.reason != "" && response["incomplete_details"].(map[string]any)["reason"] != tc.reason {
				t.Fatalf("incorrect incomplete reason: %v", response)
			}
			output := response["output"].([]any)
			if len(output) != 2 {
				t.Fatalf("initial output lost items: %v", output)
			}
			message, call := output[0].(map[string]any), output[1].(map[string]any)
			if message["content"].([]any)[0].(map[string]any)["text"] != "initial\ntext \"雪\"" ||
				call["arguments"] != ` { "n": 9007199254740993, "text": "雪" } ` ||
				call["call_id"] != "call_initial" || call["name"] != "lookup" {
				t.Fatalf("initial content changed: %v", output)
			}
			usage := response["usage"].(map[string]any)
			if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(3) || usage["total_tokens"] != float64(13) ||
				usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(4) ||
				usage["input_tokens_details"].(map[string]any)["cache_write_tokens"] != float64(2) ||
				usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(1) {
				t.Fatalf("terminal response lost initial usage: %v", usage)
			}
			done := 0
			for i, event := range wire {
				if event["sequence_number"] != float64(i) {
					t.Fatalf("event sequence changed: %v", event)
				}
				if event["type"] == "response.output_item.done" {
					if done >= len(output) || event["output_index"] != float64(done) || !reflect.DeepEqual(event["item"], output[done]) ||
						event["item"].(map[string]any)["status"] != "completed" {
						t.Fatalf("initial item identity, content or completion order changed: %v", event)
					}
					done++
				}
			}
			if done != 2 || stream.closed {
				t.Fatalf("initial items not finalized or source ownership changed: %v / %+v", wire, stream)
			}
		})
	}
}

func TestStreamToolArgumentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     json.RawMessage
		fragments []string
		want      string
		wantDelta []string
		object    bool
	}{
		{"argument-less", nil, nil, `{}`, []string{`{}`}, true},
		{"raw object", json.RawMessage(` { "n": 9007199254740993 } `), nil, ` { "n": 9007199254740993 } `, []string{` { "n": 9007199254740993 } `}, true},
		{"wrapped object", json.RawMessage(`"{\"n\":9007199254740993}"`), nil, `{"n":9007199254740993}`, []string{`{"n":9007199254740993}`}, true},
		{"whitespace wrapped object", json.RawMessage(" \n\t" + `"{\"n\":9007199254740993}"` + " \r"), nil, `{"n":9007199254740993}`, []string{`{"n":9007199254740993}`}, true},
		{"stream replaces prototype", json.RawMessage(`{}`), []string{` { "n":`, `9007199254740993 } `}, ` { "n":9007199254740993 } `, []string{` { "n":`, `9007199254740993 } `}, true},
		{"explicit empty fragment", nil, []string{""}, "", []string{""}, false},
		{"explicit empty wrapper", json.RawMessage(`""`), nil, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []ir.StreamEvent{
				ir.NewMessageStart("resp_arguments", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(4, ir.NewToolUseBlock("call_arguments", "lookup", tc.input)),
			}
			for _, fragment := range tc.fragments {
				events = append(events, ir.NewToolArgumentsDelta(4, "call_arguments", fragment))
			}
			events = append(events, ir.NewBlockStop(4), ir.NewMessageStop(ir.StopToolUse))
			wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, &eventStream{events: events})
			if err != nil {
				t.Fatal(err)
			}
			var deltas []string
			var done string
			doneCount := 0
			for _, event := range wire {
				switch event["type"] {
				case "response.function_call_arguments.delta":
					deltas = append(deltas, event["delta"].(string))
				case "response.function_call_arguments.done":
					done = event["arguments"].(string)
					doneCount++
				}
			}
			final := wire[len(wire)-1]
			if final["type"] != "response.completed" {
				t.Fatalf("tool call did not complete: %v", final)
			}
			call := final["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
			if call["arguments"] != tc.want || doneCount != 1 || done != tc.want || !reflect.DeepEqual(deltas, tc.wantDelta) {
				t.Fatalf("tool payload changed: call=%v, done=%q, deltas=%q", call, done, deltas)
			}
			if tc.object {
				var object map[string]json.RawMessage
				if err := json.Unmarshal([]byte(call["arguments"].(string)), &object); err != nil || object == nil {
					t.Fatalf("consumer cannot parse tool object: %v / %v", object, err)
				}
				if tc.name == "argument-less" && len(object) != 0 {
					t.Fatalf("argument-less call gained arguments: %v", object)
				}
			}
		})
	}
}

func TestStreamInitialContentDoesNotCloseStreamedGroups(t *testing.T) {
	start := ir.NewMessageStart("resp_unclosed", ir.RoleAssistant, "alias", ir.Usage{})
	text := ir.NewTextBlock("complete")
	text.Index = 6
	start.Message.Content = []ir.Block{text}
	wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, &eventStream{events: []ir.StreamEvent{
		start, ir.NewBlockStart(9, ir.NewTextBlock("")), ir.NewTextDelta(9, "partial"), ir.NewMessageStop(ir.StopEndTurn),
	}})
	if err == nil || wire[len(wire)-1]["type"] != "response.failed" {
		t.Fatalf("unclosed streamed group was accepted: %v / %v", err, wire)
	}
	output := wire[len(wire)-1]["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["status"] != "completed" || output[1].(map[string]any)["status"] != "incomplete" ||
		output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "partial" {
		t.Fatalf("closure failure changed complete content or lost partial output: %v", output)
	}
}

func TestStreamTransportFailureRetainsPartialOutput(t *testing.T) {
	failure := errors.New("connection reset")
	stream := &eventStream{events: []ir.StreamEvent{
		ir.NewMessageStart("resp_broken", ir.RoleAssistant, "alias", ir.Usage{InputTokens: 3}),
		ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewTextDelta(0, "partial"),
	}, failure: failure}
	wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, stream)
	if !errors.Is(err, failure) || stream.closed {
		t.Fatalf("transport failure not returned or source ownership changed: %v", err)
	}
	final := wire[len(wire)-1]
	response := final["response"].(map[string]any)
	if final["type"] != "response.failed" || response["error"].(map[string]any)["code"] != "server_error" {
		t.Fatalf("missing failed terminal event: %v", final)
	}
	item := response["output"].([]any)[0].(map[string]any)
	if item["status"] != "incomplete" || item["content"].([]any)[0].(map[string]any)["text"] != "partial" {
		t.Fatalf("failed response lost partial item: %v", item)
	}
}

func TestStreamCancellationAndWriteFailure(t *testing.T) {
	t.Run("cancellation while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		stream := &eventStream{events: []ir.StreamEvent{ir.NewMessageStart("resp_cancel", ir.RoleAssistant, "alias", ir.Usage{})}, cancel: cancel}
		wire, err := encodeWire(t, ctx, StreamEncoder{Created: 1}, stream)
		if !errors.Is(err, context.Canceled) || stream.closed || len(wire) != 2 {
			t.Fatalf("cancellation emitted failure or took source ownership: %v / %v", err, wire)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		failure := errors.New("client disconnected")
		rw := &failingResponseWriter{failure: failure}
		writer, err := sse.NewWriter(rw)
		if err != nil {
			t.Fatal(err)
		}
		stream := &eventStream{events: []ir.StreamEvent{ir.NewMessageStart("resp_disconnect", ir.RoleAssistant, "alias", ir.Usage{}), ir.NewMessageStop(ir.StopEndTurn)}}
		err = (StreamEncoder{Created: 1}).EncodeStream(context.Background(), stream, writer)
		if !errors.Is(err, failure) || stream.closed || stream.position != 1 || rw.writes != 1 {
			t.Fatalf("encoder continued after disconnect: %v, %+v, %+v", err, stream, rw)
		}
	})
	t.Run("disconnect during delta", func(t *testing.T) {
		failure := errors.New("client disconnected")
		rw := &failingResponseWriter{failure: failure, accept: 4}
		writer, err := sse.NewWriter(rw)
		if err != nil {
			t.Fatal(err)
		}
		stream := &eventStream{events: []ir.StreamEvent{
			ir.NewMessageStart("resp_disconnect", ir.RoleAssistant, "alias", ir.Usage{}),
			ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewTextDelta(0, "lost"),
			ir.NewBlockStop(0), ir.NewMessageStop(ir.StopEndTurn),
		}}
		err = (StreamEncoder{Created: 1}).EncodeStream(context.Background(), stream, writer)
		if !errors.Is(err, failure) || stream.position != 3 || rw.writes != 5 {
			t.Fatalf("encoder wrote recovery events after disconnect: %v, %+v, %+v", err, stream, rw)
		}
	})
	t.Run("EOF before terminal", func(t *testing.T) {
		stream := &eventStream{events: []ir.StreamEvent{ir.NewMessageStart("resp_eof", ir.RoleAssistant, "alias", ir.Usage{})}}
		wire, err := encodeWire(t, context.Background(), StreamEncoder{Created: 1}, stream)
		if !errors.Is(err, io.ErrUnexpectedEOF) || stream.closed || wire[len(wire)-1]["type"] != "response.failed" {
			t.Fatalf("truncated stream accepted as success: %v / %v", err, wire)
		}
	})
}

var _ adapter.Stream = (*eventStream)(nil)

type eventStream struct {
	events   []ir.StreamEvent
	position int
	closed   bool
	failure  error
	cancel   context.CancelFunc
}

func (s *eventStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if s.position < len(s.events) {
		event := s.events[s.position]
		s.position++
		return event, nil
	}
	if s.cancel != nil {
		s.cancel()
		return ir.StreamEvent{}, ctx.Err()
	}
	if s.failure != nil {
		return ir.StreamEvent{}, s.failure
	}
	return ir.StreamEvent{}, io.EOF
}
func (s *eventStream) Close() error { s.closed = true; return nil }

type failingResponseWriter struct {
	failure error
	writes  int
	accept  int
}

func (*failingResponseWriter) Header() http.Header { return make(http.Header) }
func (*failingResponseWriter) WriteHeader(int)     {}
func (w *failingResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes <= w.accept {
		return len(p), nil
	}
	return 0, w.failure
}
func (*failingResponseWriter) Flush() {}

func encodeWire(t *testing.T, ctx context.Context, encoder StreamEncoder, stream adapter.Stream) ([]map[string]any, error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	err = encoder.EncodeStream(ctx, stream, writer)
	return readWire(t, recorder.Body.String()), err
}

func readWire(t *testing.T, body string) []map[string]any {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var events []map[string]any
	for {
		event, err := reader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(event.Data), &value); err != nil {
			t.Fatal(err)
		}
		if value["type"] != event.Type {
			t.Fatalf("SSE name and JSON type differ: %v", event)
		}
		events = append(events, value)
	}
}
