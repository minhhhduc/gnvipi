package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

func TestStreamGoldens(t *testing.T) {
	for _, tc := range []struct {
		name         string
		includeUsage bool
	}{
		{name: "text"},
		{name: "include_usage", includeUsage: true},
		{name: "tools"},
		{name: "reasoning"},
		{name: "error", includeUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := os.ReadFile("testdata/stream/" + tc.name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var events []ir.StreamEvent
			if err := json.Unmarshal(input, &events); err != nil {
				t.Fatal(err)
			}
			if err := ir.ValidateEventOrder(events); err != nil {
				t.Fatal(err)
			}
			stream := &streamFixture{events: events}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			encoder := StreamEncoder{Created: 1700000000, IncludeUsage: tc.includeUsage}
			if err := encoder.EncodeStream(context.Background(), stream, writer); err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile("testdata/stream/" + tc.name + ".golden.sse")
			if err != nil {
				t.Fatal(err)
			}
			got, want := streamFrames(t, recorder.Body.Bytes()), streamFrames(t, golden)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SSE frames differ\ngot: %s\nwant: %s", recorder.Body.Bytes(), golden)
			}
			if stream.position != len(events) || stream.closes != 0 {
				t.Fatalf("terminal consumption or caller ownership violated: read=%d/%d closes=%d", stream.position, len(events), stream.closes)
			}
		})
	}
}

func TestStreamTerminalDoesNotConsumeOrWriteLaterEvents(t *testing.T) {
	for _, terminal := range []ir.StreamEvent{
		ir.NewMessageStop(ir.StopEndTurn),
		ir.NewErrorEvent("rate_limit", "capacity exhausted"),
		ir.NewMessageStop(ir.StopError),
	} {
		t.Run(string(terminal.Type)+"/"+string(terminal.StopReason), func(t *testing.T) {
			stream := &streamFixture{events: []ir.StreamEvent{
				ir.NewMessageStart("chat-1", ir.RoleAssistant, "client-alias", ir.Usage{}),
				terminal,
				ir.NewMessageStart("must-not-read", ir.RoleAssistant, "other-model", ir.Usage{}),
			}}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err != nil {
				t.Fatal(err)
			}
			frames := streamFrames(t, recorder.Body.Bytes())
			if stream.position != 2 || strings.Contains(recorder.Body.String(), "must-not-read") {
				t.Fatal("encoder continued after terminal event")
			}
			if terminal.Type == ir.EventMessageStop && terminal.StopReason != ir.StopError {
				if len(frames) != 3 || frames[2] != "[DONE]" {
					t.Fatalf("successful terminal frames = %#v", frames)
				}
			} else {
				if len(frames) != 2 {
					t.Fatalf("error terminal frames = %#v", frames)
				}
				last, ok := frames[1].(map[string]any)
				if !ok || last["error"] == nil {
					t.Fatalf("error turned into a successful finish: %#v", frames)
				}
			}
		})
	}
}

func TestStreamReasoningCanBeDisabled(t *testing.T) {
	stream := &streamFixture{events: []ir.StreamEvent{
		ir.NewMessageStart("chat-1", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewBlockStart(0, ir.NewReasoningBlock("initial secret", "signature")),
		ir.NewReasoningDelta(0, "more secret"),
		ir.NewBlockStop(0),
		ir.NewBlockStart(1, ir.NewTextBlock("")),
		ir.NewTextDelta(1, "visible"),
		ir.NewBlockStop(1),
		ir.NewMessageStop(ir.StopEndTurn),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{Created: 7, DisableReasoning: true}).EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "reasoning_content") || !strings.Contains(recorder.Body.String(), "visible") {
		t.Fatalf("reasoning opt-out output: %s", recorder.Body.String())
	}
}

func TestStreamCurrentCreatedIsStable(t *testing.T) {
	stream := &streamFixture{events: []ir.StreamEvent{
		ir.NewMessageStart("chat-1", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewMessageStop(ir.StopEndTurn),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	if err := (StreamEncoder{IncludeUsage: true}).EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()
	frames := streamFrames(t, recorder.Body.Bytes())
	created := frames[0].(map[string]any)["created"].(float64)
	if int64(created) < before || int64(created) > after {
		t.Fatalf("created %v outside [%d,%d]", created, before, after)
	}
	for _, frame := range frames[:len(frames)-1] {
		chunk := frame.(map[string]any)
		if chunk["created"] != created || chunk["id"] != "chat-1" || chunk["model"] != "alias" {
			t.Fatalf("unstable stream metadata: %#v", chunk)
		}
	}
}

func TestStreamReadFailuresEmitErrorAfterPartialOutput(t *testing.T) {
	transportErr := errors.New("upstream connection lost")
	parseErr := &json.SyntaxError{Offset: 9}
	for _, tc := range []struct {
		name    string
		readErr error
		want    error
	}{
		{name: "truncated", want: io.ErrUnexpectedEOF},
		{name: "wrapped EOF", readErr: fmt.Errorf("upstream: %w", io.EOF), want: io.ErrUnexpectedEOF},
		{name: "transport", readErr: transportErr, want: transportErr},
		{name: "parse", readErr: parseErr, want: parseErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &streamFixture{events: []ir.StreamEvent{
				ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(0, ir.NewTextBlock("")),
				ir.NewTextDelta(0, "partial answer"),
			}, readErr: tc.readErr}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			err = (StreamEncoder{Created: 7, IncludeUsage: true}).EncodeStream(context.Background(), stream, writer)
			if !errors.Is(err, tc.want) || strings.Contains(recorder.Body.String(), "[DONE]") || strings.Contains(recorder.Body.String(), `"finish_reason":"stop"`) {
				t.Fatalf("failure=%v output=%s", err, recorder.Body.String())
			}
			frames := streamFrames(t, recorder.Body.Bytes())
			partial := frames[1].(map[string]any)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			last := frames[len(frames)-1].(map[string]any)
			if partial["content"] != "partial answer" || last["error"] == nil || last["choices"] != nil {
				t.Fatalf("partial output failure was not consumer-visible: %#v", frames)
			}
		})
	}
}

func TestStreamReadFailureKeepsCauseWhenErrorWriteFails(t *testing.T) {
	readErr := errors.New("upstream connection lost")
	stream := &streamFixture{events: []ir.StreamEvent{
		ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
	}, readErr: readErr}
	output := &streamFailWriter{failAt: 2, err: errors.New("client disconnected")}
	writer, err := sse.NewWriter(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); !errors.Is(err, readErr) {
		t.Fatalf("recovery write replaced upstream cause: %v", err)
	}
}

func TestStreamWriterFailureStopsImmediately(t *testing.T) {
	writeErr := errors.New("client disconnected")
	for failAt := 1; failAt <= 4; failAt++ {
		stream := &streamFixture{events: []ir.StreamEvent{
			ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
			ir.NewMessageStop(ir.StopEndTurn),
			ir.NewErrorEvent("internal", "must not read"),
		}}
		output := &streamFailWriter{failAt: failAt, err: writeErr}
		writer, err := sse.NewWriter(output)
		if err != nil {
			t.Fatal(err)
		}
		err = (StreamEncoder{Created: 7, IncludeUsage: true}).EncodeStream(context.Background(), stream, writer)
		if !errors.Is(err, writeErr) || output.writes != failAt || stream.position > 2 || stream.closes != 0 {
			t.Fatalf("failure at %d: error=%v writes=%d reads=%d closes=%d", failAt, err, output.writes, stream.position, stream.closes)
		}
	}
}

func TestStreamReadCancellationDoesNotWriteRecovery(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(fmt.Sprintf("caller canceled=%t", cancelCaller), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &streamFixture{events: []ir.StreamEvent{
				ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(0, ir.NewTextBlock("")),
				ir.NewTextDelta(0, "partial answer"),
			}, readErr: context.Canceled}
			if cancelCaller {
				stream.onEnd = cancel
				stream.readErr = errors.New("read interrupted by disconnect")
			}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			err = (StreamEncoder{Created: 7}).EncodeStream(ctx, stream, writer)
			if !errors.Is(err, stream.readErr) || strings.Contains(recorder.Body.String(), `"error"`) || strings.Contains(recorder.Body.String(), "[DONE]") {
				t.Fatalf("canceled source received recovery writes: error=%v output=%s", err, recorder.Body.String())
			}
		})
	}
}

func TestStreamCanceledBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &streamFixture{events: []ir.StreamEvent{ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{})}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	err = (StreamEncoder{}).EncodeStream(ctx, stream, writer)
	if !errors.Is(err, context.Canceled) || stream.position != 0 || recorder.Body.Len() != 0 || stream.closes != 0 {
		t.Fatalf("canceled stream: error=%v reads=%d output=%q closes=%d", err, stream.position, recorder.Body.String(), stream.closes)
	}
}

func TestStreamClientDisconnectCancelsUpstreamRead(t *testing.T) {
	results := make(chan error, 1)
	waiting := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writer, err := sse.NewWriter(w)
		if err != nil {
			results <- err
			return
		}
		stream := &streamDisconnect{waiting: waiting}
		results <- (StreamEncoder{Created: 7}).EncodeStream(r.Context(), stream, writer)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-ctx.Done():
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("upstream read did not start")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("disconnect returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("client disconnect did not cancel the upstream read")
	}
}

func TestStreamRejectsMalformedOrder(t *testing.T) {
	start := ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{})
	for _, events := range [][]ir.StreamEvent{
		{ir.NewMessageStop(ir.StopEndTurn)},
		{start, start},
		{start, ir.NewTextDelta(0, "unopened")},
		{start, ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewMessageStop(ir.StopEndTurn)},
		{start, ir.NewMessageStop("")},
		{start, ir.NewUsageUpdate(ir.Usage{}), ir.NewUsageUpdate(ir.Usage{})},
	} {
		stream := &streamFixture{events: events}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err == nil {
			t.Fatalf("malformed stream accepted: %#v", events)
		}
		if strings.Contains(recorder.Body.String(), "[DONE]") {
			t.Fatal("malformed stream reported successful completion")
		}
	}
}

func TestStreamInitialContentAndToolPayloadRemainIntact(t *testing.T) {
	start := ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{})
	start.Message.Content = []ir.Block{
		ir.NewTextBlock("initial"),
		ir.NewToolUseBlock("call-1", "lookup", json.RawMessage(`{"unknown":{"limit":0}}`)),
		ir.NewToolUseBlock("call-2", "empty", nil),
	}
	stream := &streamFixture{events: []ir.StreamEvent{start, ir.NewMessageStop(ir.StopToolUse)}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatal(err)
	}
	frames := streamFrames(t, recorder.Body.Bytes())
	initial := frames[1].(map[string]any)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if initial["content"] != "initial" {
		t.Fatalf("initial text lost: %#v", initial)
	}
	calls := streamToolCalls(t, frames)
	want := map[int]streamAccumulatedTool{
		0: {id: "call-1", name: "lookup", arguments: `{"unknown":{"limit":0}}`},
		1: {id: "call-2", name: "empty", arguments: `{}`},
	}
	if !reflect.DeepEqual(calls, want) || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("initial tool content was not completed: %#v", calls)
	}
}

func TestStreamToolArgumentsAccumulateAsOnePayload(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     json.RawMessage
		fragments []string
		want      string
	}{
		{name: "stream replaces prototype", input: json.RawMessage(`{}`), fragments: []string{`{"city":`, `"Paris"}`}, want: `{"city":"Paris"}`},
		{name: "opaque prototype", input: json.RawMessage(" \n{\"n\":9007199254740993, \"vendor\":{\"enabled\":false}}\t"), want: " \n{\"n\":9007199254740993, \"vendor\":{\"enabled\":false}}\t"},
		{name: "argument-less", want: `{}`},
		{name: "quoted object", input: json.RawMessage(`"{\"n\":9007199254740993}"`), want: `{"n":9007199254740993}`},
		{name: "whitespace quoted object", input: json.RawMessage(" \n\t\"{\\\"n\\\":9007199254740993}\"\r"), want: `{"n":9007199254740993}`},
		{name: "explicit empty fragment", input: json.RawMessage(`{}`), fragments: []string{""}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []ir.StreamEvent{
				ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(4, ir.NewToolUseBlock("call-A", "weather", tc.input)),
			}
			for _, fragment := range tc.fragments {
				events = append(events, ir.NewToolArgumentsDelta(4, "call-A", fragment))
			}
			events = append(events, ir.NewBlockStop(4), ir.NewMessageStop(ir.StopToolUse))
			stream := &streamFixture{events: events}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err != nil {
				t.Fatal(err)
			}
			frames := streamFrames(t, recorder.Body.Bytes())
			calls := streamToolCalls(t, frames)
			call := calls[0]
			if len(calls) != 1 || call.id != "call-A" || call.name != "weather" || call.arguments != tc.want || frames[len(frames)-1] != "[DONE]" {
				t.Fatalf("consumer tool call = %#v, want arguments %q", calls, tc.want)
			}
			if tc.want != "" {
				decoder := json.NewDecoder(strings.NewReader(call.arguments))
				decoder.UseNumber()
				var object map[string]any
				if err := decoder.Decode(&object); err != nil || object == nil {
					t.Fatalf("arguments do not decode as an object: %q: %v", call.arguments, err)
				}
				if n, ok := object["n"]; ok && n != json.Number("9007199254740993") {
					t.Fatalf("integer precision lost: %#v", n)
				}
			}
		})
	}
}

func TestStreamRejectsConflictingToolIdentityBeforeArguments(t *testing.T) {
	for _, tc := range []struct {
		name, id, toolName string
	}{
		{name: "ID", id: "call-B", toolName: "weather"},
		{name: "name", id: "call-A", toolName: "clock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conflict := ir.NewToolArgumentsDelta(4, tc.id, `"misattributed"}`)
			conflict.Delta.ToolCallName = tc.toolName
			stream := &streamFixture{events: []ir.StreamEvent{
				ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
				ir.NewBlockStart(4, ir.NewToolUseBlock("call-A", "weather", nil)),
				ir.NewToolArgumentsDelta(4, "call-A", `{"city":`),
				conflict,
				ir.NewBlockStop(4),
				ir.NewMessageStop(ir.StopToolUse),
			}}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err == nil {
				t.Fatal("conflicting tool identity accepted")
			}
			frames := streamFrames(t, recorder.Body.Bytes())
			last := frames[len(frames)-1].(map[string]any)
			calls := streamToolCalls(t, frames)
			if last["error"] == nil || calls[0].arguments != `{"city":` || strings.Contains(recorder.Body.String(), "misattributed") || strings.Contains(recorder.Body.String(), "[DONE]") {
				t.Fatalf("conflict forwarded or silently completed: %s", recorder.Body.String())
			}
		})
	}
}

func TestStreamToolIdentityCanArriveOnMatchingDeltas(t *testing.T) {
	first := ir.NewToolArgumentsDelta(4, "call-A", `{"city":`)
	first.Delta.ToolCallName = "weather"
	last := ir.NewToolArgumentsDelta(4, "call-A", `"Paris"}`)
	last.Delta.ToolCallName = "weather"
	stream := &streamFixture{events: []ir.StreamEvent{
		ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewBlockStart(4, ir.NewToolUseBlock("", "", nil)),
		first,
		last,
		ir.NewBlockStop(4),
		ir.NewMessageStop(ir.StopToolUse),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatal(err)
	}
	frames := streamFrames(t, recorder.Body.Bytes())
	calls := streamToolCalls(t, frames)
	if calls[0] != (streamAccumulatedTool{id: "call-A", name: "weather", arguments: `{"city":"Paris"}`}) || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("late or repeated matching metadata damaged call: %#v", calls)
	}
}

type streamAccumulatedTool struct {
	id, name, arguments string
}

func streamToolCalls(t *testing.T, frames []any) map[int]streamAccumulatedTool {
	t.Helper()
	calls := make(map[int]streamAccumulatedTool)
	for _, frame := range frames {
		chunk, ok := frame.(map[string]any)
		if !ok {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, choice := range choices {
			delta := choice.(map[string]any)["delta"].(map[string]any)
			tools, _ := delta["tool_calls"].([]any)
			for _, item := range tools {
				tool := item.(map[string]any)
				index := int(tool["index"].(float64))
				call := calls[index]
				if id, ok := tool["id"].(string); ok {
					call.id += id
				}
				function := tool["function"].(map[string]any)
				if name, ok := function["name"].(string); ok {
					call.name += name
				}
				call.arguments += function["arguments"].(string)
				calls[index] = call
			}
		}
	}
	return calls
}

func TestStreamContentFilterRemainsDistinctFromNormalStop(t *testing.T) {
	stream := &streamFixture{events: []ir.StreamEvent{
		ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}),
		ir.NewMessageStop(ir.StopContentFilter),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{Created: 7}).EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatal(err)
	}
	frames := streamFrames(t, recorder.Body.Bytes())
	finish := frames[1].(map[string]any)["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if finish != "content_filter" || frames[2] != "[DONE]" {
		t.Fatalf("content filtering mislabeled: %#v", frames)
	}
}

func streamFrames(t *testing.T, body []byte) []any {
	t.Helper()
	reader := sse.NewReader(bytes.NewReader(body))
	defer reader.Close()
	var frames []any
	for {
		event, err := reader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type != "" || event.ID != "" {
			t.Fatalf("unexpected named SSE event: %#v", event)
		}
		if event.Data == "[DONE]" {
			frames = append(frames, event.Data)
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(event.Data), &value); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, value)
	}
}

type streamFixture struct {
	events   []ir.StreamEvent
	position int
	closes   int
	readErr  error
	onEnd    func()
}

func (s *streamFixture) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if s.position == len(s.events) {
		if s.onEnd != nil {
			s.onEnd()
		}
		if s.readErr != nil {
			return ir.StreamEvent{}, s.readErr
		}
		return ir.StreamEvent{}, io.EOF
	}
	event := s.events[s.position]
	s.position++
	return event, nil
}

func (s *streamFixture) Close() error { s.closes++; return nil }

type streamFailWriter struct {
	writes int
	failAt int
	err    error
}

func (*streamFailWriter) Header() http.Header { return make(http.Header) }
func (*streamFailWriter) WriteHeader(int)     {}
func (*streamFailWriter) Flush()              {}
func (w *streamFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return len(p), nil
}

type streamDisconnect struct {
	started bool
	waiting chan struct{}
}

func (s *streamDisconnect) Next(ctx context.Context) (ir.StreamEvent, error) {
	if !s.started {
		s.started = true
		return ir.NewMessageStart("id", ir.RoleAssistant, "alias", ir.Usage{}), nil
	}
	close(s.waiting)
	<-ctx.Done()
	return ir.StreamEvent{}, ctx.Err()
}
func (*streamDisconnect) Close() error { return nil }
