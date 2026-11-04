package anthropic

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
	"sync"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// These goldens catch missing lifecycle events, payload loss and incorrect
// delta/usage mapping. The wire expectations are authored independently.
func TestStreamGoldens(t *testing.T) {
	for _, name := range []string{"text", "thinking-text", "tool-arguments", "error"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/sse/" + name + ".ir.json")
			if err != nil {
				t.Fatal(err)
			}
			var events []ir.StreamEvent
			if err := json.Unmarshal(data, &events); err != nil {
				t.Fatal(err)
			}
			stream := &streamTestSource{events: events}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			err = (StreamEncoder{}).EncodeStream(context.Background(), stream, writer)
			if err != nil {
				t.Fatal(err)
			}
			streamAssertGolden(t, recorder.Body.String(), name)
			if stream.next != len(events) || stream.closed != 0 {
				t.Fatalf("consumed %d/%d, closes %d", stream.next, len(events), stream.closed)
			}
		})
	}
}

// Sparse IR indexes and concurrently open blocks must become distinct,
// contiguous wire indexes, and omitted stops must not leave an invalid message.
func TestStreamClosesOpenBlocksBeforeTerminal(t *testing.T) {
	source := &streamTestSource{events: []ir.StreamEvent{
		ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
		ir.NewBlockStart(8, ir.NewTextBlock("")),
		ir.NewBlockStart(3, ir.NewToolUseBlock("call", "run", nil)),
		ir.NewTextDelta(8, "answer"),
		ir.NewToolArgumentsDelta(3, "call", "{}"),
		ir.NewMessageStop(ir.StopToolUse),
		ir.NewTextDelta(8, "must not be read"),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err != nil {
		t.Fatal(err)
	}
	frames := streamTestFrames(t, recorder.Body.String())
	var got []string
	for _, frame := range frames {
		got = append(got, frame.Type)
	}
	want := []string{"message_start", "content_block_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_stop", "message_delta", "message_stop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v", got)
	}
	for _, pair := range [][2]int{{1, 0}, {2, 1}, {3, 0}, {4, 1}, {5, 0}, {6, 1}} {
		var payload struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(frames[pair[0]].Data), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Index != pair[1] {
			t.Fatalf("frame %d index = %d", pair[0], payload.Index)
		}
	}
	if source.next != 6 || source.closed != 0 {
		t.Fatalf("reads %d, closes %d", source.next, source.closed)
	}
}

// Invalid ordering must be terminal rather than produce unpaired deltas or
// silently reopen a block the client already completed.
func TestStreamRejectsInvalidBlockTransitions(t *testing.T) {
	start := ir.NewBlockStart(0, ir.NewTextBlock(""))
	for _, tc := range []struct {
		name   string
		events []ir.StreamEvent
	}{
		{"unopened delta", []ir.StreamEvent{ir.NewTextDelta(0, "x")}},
		{"duplicate start", []ir.StreamEvent{start, start}},
		{"reopened block", []ir.StreamEvent{start, ir.NewBlockStop(0), start}},
		{"duplicate stop", []ir.StreamEvent{start, ir.NewBlockStop(0), ir.NewBlockStop(0)}},
		{"wrong delta kind", []ir.StreamEvent{start, ir.NewReasoningDelta(0, "x")}},
		{"wrong tool identity", []ir.StreamEvent{ir.NewBlockStart(0, ir.NewToolUseBlock("one", "run", nil)), ir.NewToolArgumentsDelta(0, "two", "{}")}},
		{"duplicate message", []ir.StreamEvent{ir.NewMessageStart("second", ir.RoleAssistant, "model", ir.Usage{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []ir.StreamEvent{ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{})}
			events = append(events, tc.events...)
			events = append(events, ir.NewMessageStop(ir.StopEndTurn))
			source := &streamTestSource{events: events}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err == nil {
				t.Fatal("invalid sequence accepted")
			}
			frames := streamTestFrames(t, recorder.Body.String())
			if frames[len(frames)-1].Type != "error" || strings.Contains(recorder.Body.String(), "event: message_stop") {
				t.Fatalf("nonterminal failure: %s", recorder.Body.String())
			}
			if source.next != len(events)-1 || source.closed != 0 {
				t.Fatalf("reads %d, closes %d", source.next, source.closed)
			}
		})
	}
}

// Client write failure must be returned unchanged, never followed by another
// event or an extra upstream pull (including when writing a terminal error).
func TestStreamDisconnectStopsImmediately(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		for _, failAfter := range []int{0, 2, 3} {
			source := &streamTestSource{events: []ir.StreamEvent{
				ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
				ir.NewBlockStart(0, ir.NewTextBlock("")), ir.NewTextDelta(0, "x"),
				ir.NewErrorEvent("overloaded", "busy"), ir.NewMessageStop(ir.StopEndTurn),
			}}
			failure := errors.New("client disconnected")
			sink := &streamTestSink{ResponseRecorder: httptest.NewRecorder(), failAfter: failAfter, failure: failure}
			writer, err := sse.NewWriter(sink)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{KeepaliveInterval: interval}).EncodeStream(context.Background(), source, writer); !errors.Is(err, failure) {
				t.Fatalf("write error = %v", err)
			}
			if sink.writes != failAfter+1 || source.next != failAfter+1 || source.closed != 0 {
				t.Fatalf("writes %d, reads %d, closes %d", sink.writes, source.next, source.closed)
			}
		}
	}
}

// An idle ping must not cancel/restart an in-flight Next. Returning after the
// terminal event must join the optional pump and never read the queued tail.
func TestStreamIdleKeepaliveGolden(t *testing.T) {
	source := &streamTestSource{events: []ir.StreamEvent{
		ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}), ir.NewMessageStop(ir.StopEndTurn), ir.NewErrorEvent("overloaded", "tail"),
	}, waitAt: 1, ready: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	sink := &streamTestSink{ResponseRecorder: httptest.NewRecorder(), failAfter: -1, onWrite: func(data []byte) {
		if strings.Contains(string(data), "event: ping") {
			once.Do(func() { close(source.release) })
		}
	}}
	writer, err := sse.NewWriter(sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := (StreamEncoder{KeepaliveInterval: 20 * time.Millisecond}).EncodeStream(ctx, source, writer); err != nil {
		t.Fatal(err)
	}
	streamAssertGolden(t, sink.Body.String(), "keepalive")
	if source.next != 2 || source.closed != 0 {
		t.Fatalf("reads %d, closes %d", source.next, source.closed)
	}
}

// Cancellation during a blocked Next must release the encoder's pending read
// without manufacturing a terminal frame for an already disconnected client.
func TestStreamCancellationReleasesPendingNext(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		source := &streamTestSource{waitAt: 0, ready: make(chan struct{}), release: make(chan struct{})}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- (StreamEncoder{KeepaliveInterval: interval}).EncodeStream(ctx, source, writer) }()
		select {
		case <-source.ready:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("Next not started")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled encoder did not return")
		}
		if recorder.Body.Len() != 0 || source.closed != 0 {
			t.Fatalf("body %q, closes %d", recorder.Body.String(), source.closed)
		}
	}
}

// A truncated upstream cannot be presented as a successful end_turn.
func TestStreamReadFailureIsTerminal(t *testing.T) {
	for _, failure := range []error{io.EOF, errors.New("broken upstream")} {
		source := &streamTestSource{events: []ir.StreamEvent{ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{})}, readErr: failure}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		err = (StreamEncoder{}).EncodeStream(context.Background(), source, writer)
		want := failure
		if errors.Is(failure, io.EOF) {
			want = io.ErrUnexpectedEOF
		}
		if !errors.Is(err, want) {
			t.Fatalf("read error = %v", err)
		}
		frames := streamTestFrames(t, recorder.Body.String())
		if len(frames) != 2 || frames[1].Type != "error" || source.closed != 0 {
			t.Fatalf("frames %v, closes %d", frames, source.closed)
		}
	}
}

func TestStreamClientModelAlias(t *testing.T) {
	ctx := core.WithRequestContext(context.Background(), &core.RequestContext{ClientModel: "client-alias", UpstreamModel: "provider-model"})
	source := &streamTestSource{events: []ir.StreamEvent{ir.NewMessageStart("m", ir.RoleAssistant, "provider-model", ir.Usage{}), ir.NewMessageStop(ir.StopEndTurn)}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{}).EncodeStream(ctx, source, writer); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(streamTestFrames(t, recorder.Body.String())[0].Data), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message.Model != "client-alias" || source.events[0].Message.Model != "provider-model" {
		t.Fatalf("client model %q, upstream mutated %q", payload.Message.Model, source.events[0].Message.Model)
	}
}

func TestStreamStopReasonMapping(t *testing.T) {
	for _, reason := range []ir.StopReason{ir.StopEndTurn, ir.StopMaxTokens, ir.StopSequence, ir.StopToolUse, ir.StopContentFilter, ir.StopError} {
		source := &streamTestSource{events: []ir.StreamEvent{ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}), ir.NewMessageStop(reason)}}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		err = (StreamEncoder{}).EncodeStream(context.Background(), source, writer)
		frames := streamTestFrames(t, recorder.Body.String())
		if reason == ir.StopError {
			if err != nil || frames[len(frames)-1].Type != "error" {
				t.Fatalf("error stop not delivered: %v, %v", err, frames)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(frames[len(frames)-2].Data), &payload); err != nil {
			t.Fatal(err)
		}
		want := string(reason)
		if reason == ir.StopContentFilter {
			want = "refusal"
		}
		if payload.Delta.StopReason != want {
			t.Fatalf("%s mapped to %s", reason, payload.Delta.StopReason)
		}
	}
}

// A delivered provider error terminates encoding successfully, without
// pulling a queued tail or closing the caller's stream.
func TestStreamReportedErrorIsTerminal(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		source := &streamTestSource{events: []ir.StreamEvent{
			ir.NewErrorEvent(string(gwerr.KindOverloaded), "busy"),
			ir.NewMessageStart("tail", ir.RoleAssistant, "model", ir.Usage{}),
		}}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		if err := (StreamEncoder{KeepaliveInterval: interval}).EncodeStream(context.Background(), source, writer); err != nil {
			t.Fatal(err)
		}
		frames := streamTestFrames(t, recorder.Body.String())
		if len(frames) != 1 || frames[0].Type != "error" || source.next != 1 || source.closed != 0 {
			t.Fatalf("frames %v, reads %d, closes %d", frames, source.next, source.closed)
		}
		var payload struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(frames[0].Data), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Error.Type != "overloaded_error" {
			t.Fatalf("error type = %s", payload.Error.Type)
		}
	}
}

// A ping write failure must cancel/join the same pending Next, not wait for
// a provider event or attempt to append a second terminal error frame.
func TestStreamPingDisconnectCancelsPendingRead(t *testing.T) {
	source := &streamTestSource{waitAt: 0, ready: make(chan struct{}), release: make(chan struct{})}
	failure := errors.New("ping disconnected")
	sink := &streamTestSink{ResponseRecorder: httptest.NewRecorder(), failure: failure}
	writer, err := sse.NewWriter(sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (StreamEncoder{KeepaliveInterval: 10 * time.Millisecond}).EncodeStream(ctx, source, writer); !errors.Is(err, failure) {
		t.Fatalf("ping error = %v", err)
	}
	if sink.writes != 1 || sink.Body.Len() != 0 || source.closed != 0 {
		t.Fatalf("writes %d, body %q, closes %d", sink.writes, sink.Body.String(), source.closed)
	}
}

// Initial tool arguments must survive as raw JSON, including unknown fields
// and integers too large for float64, rather than being dropped or reparsed.
func TestStreamInitialToolPayloadPreserved(t *testing.T) {
	input := `{"large":9007199254740993,"unknown":{"flag":false},"empty":null}`
	message := ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{})
	block := ir.NewToolUseBlock("opaque", "run", json.RawMessage(input))
	block.Index = 7
	message.Message.Content = []ir.Block{block}
	source := &streamTestSource{events: []ir.StreamEvent{message, ir.NewMessageStop(ir.StopToolUse)}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err != nil {
		t.Fatal(err)
	}
	frames := streamTestFrames(t, recorder.Body.String())
	var payload struct {
		Delta struct {
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(frames[2].Data), &payload); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 6 || payload.Delta.PartialJSON != input || frames[3].Type != "content_block_stop" {
		t.Fatalf("initial content lost: %v", frames)
	}
}

// A tool consumer replaces its initial input object with concatenated argument
// fragments. Prototypes must be deferred and discarded when real fragments arrive.
func TestStreamToolArgumentsReconstructObject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     json.RawMessage
		fragments []string
		want      string
		autoClose bool
	}{
		{"streamed arguments", json.RawMessage(`{}`), []string{`{"city":`, `"Paris","large":9007199254740993}`}, `{"city":"Paris","large":9007199254740993}`, false},
		{"streamed arguments auto-close", json.RawMessage(`{"stale":true}`), []string{`{"city":`, `"Paris"}`}, `{"city":"Paris"}`, true},
		{"raw prototype", json.RawMessage(" \n{ \"large\":9007199254740993, \"unknown\":{\"flag\":false} }\t"), nil, " \n{ \"large\":9007199254740993, \"unknown\":{\"flag\":false} }\t", false},
		{"wrapped prototype", json.RawMessage(`"{\"large\":9007199254740993, \"unknown\":null}"`), nil, `{"large":9007199254740993, "unknown":null}`, false},
		{"whitespace wrapped prototype auto-close", json.RawMessage(" \n\t\"{\\\"large\\\":9007199254740993, \\\"unknown\\\":null}\"\r "), nil, `{"large":9007199254740993, "unknown":null}`, true},
		{"argument-less", nil, nil, `{}`, false},
		{"argument-less auto-close", nil, nil, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []ir.StreamEvent{
				ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
				ir.NewBlockStart(7, ir.NewToolUseBlock("call", "run", tc.input)),
			}
			for _, fragment := range tc.fragments {
				events = append(events, ir.NewToolArgumentsDelta(7, "call", fragment))
			}
			if !tc.autoClose {
				events = append(events, ir.NewBlockStop(7))
			}
			completionRead := len(events)
			events = append(events, ir.NewMessageStop(ir.StopToolUse))
			if tc.autoClose {
				completionRead++
			}
			terminalRead := len(events)
			events = append(events, ir.NewToolArgumentsDelta(7, "call", `{"tail":true}`))
			source := &streamTestSource{events: events}
			sink := &streamTestSink{ResponseRecorder: httptest.NewRecorder(), failAfter: -1}
			sink.onWrite = func(data []byte) {
				if tc.fragments == nil && strings.Contains(string(data), `"type":"input_json_delta"`) && source.next < completionRead {
					t.Error("prototype arguments emitted before block completion")
				}
			}
			writer, err := sse.NewWriter(sink)
			if err != nil {
				t.Fatal(err)
			}
			if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err != nil {
				t.Fatal(err)
			}
			frames := streamTestFrames(t, sink.Body.String())
			var input json.RawMessage
			var fragments []string
			stopped := false
			for _, frame := range frames {
				var payload struct {
					ContentBlock struct {
						Input json.RawMessage `json:"input"`
					} `json:"content_block"`
					Delta struct {
						Type        string `json:"type"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
					t.Fatal(err)
				}
				switch frame.Type {
				case "content_block_start":
					input = payload.ContentBlock.Input
					if string(input) != `{}` {
						t.Fatalf("initial consumer input = %s, want {}", input)
					}
				case "content_block_delta":
					if stopped || payload.Delta.Type != "input_json_delta" {
						t.Fatalf("unexpected argument delta: %s", frame.Data)
					}
					fragments = append(fragments, payload.Delta.PartialJSON)
				case "content_block_stop":
					stopped = true
				}
			}
			if len(fragments) != 0 {
				input = json.RawMessage(strings.Join(fragments, ""))
			}
			if string(input) != tc.want {
				t.Fatalf("consumer arguments = %q, want %q", input, tc.want)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(input, &object); err != nil || object == nil {
				t.Fatalf("consumer input is not a valid object: %s (%v)", input, err)
			}
			if !stopped || frames[len(frames)-1].Type != "message_stop" || source.next != terminalRead || source.closed != 0 {
				t.Fatalf("invalid completion: frames %v, reads %d, closes %d", frames, source.next, source.closed)
			}
		})
	}
}

// Even an empty explicit fragment supersedes a prototype. Do not silently
// replace the provider's argument bytes with a fabricated fallback object.
func TestStreamExplicitEmptyToolFragmentPreserved(t *testing.T) {
	source := &streamTestSource{events: []ir.StreamEvent{
		ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
		ir.NewBlockStart(0, ir.NewToolUseBlock("call", "run", json.RawMessage(`{"stale":true}`))),
		ir.NewToolArgumentsDelta(0, "call", ""),
		ir.NewBlockStop(0), ir.NewMessageStop(ir.StopToolUse),
	}}
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err != nil {
		t.Fatal(err)
	}
	var fragments []string
	for _, frame := range streamTestFrames(t, recorder.Body.String()) {
		if frame.Type != "content_block_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
			t.Fatal(err)
		}
		fragments = append(fragments, payload.Delta.PartialJSON)
	}
	if !reflect.DeepEqual(fragments, []string{""}) {
		t.Fatalf("explicit fragment rewritten: %q", fragments)
	}
}

func TestStreamDeferredToolInputDisconnectStopsImmediately(t *testing.T) {
	source := &streamTestSource{events: []ir.StreamEvent{
		ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
		ir.NewBlockStart(0, ir.NewToolUseBlock("call", "run", json.RawMessage(`{"large":9007199254740993}`))),
		ir.NewMessageStop(ir.StopToolUse),
		ir.NewToolArgumentsDelta(0, "call", "must not be read"),
	}}
	failure := errors.New("client disconnected")
	sink := &streamTestSink{ResponseRecorder: httptest.NewRecorder(), failAfter: 2, failure: failure}
	writer, err := sse.NewWriter(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); !errors.Is(err, failure) {
		t.Fatalf("write error = %v", err)
	}
	if sink.writes != 3 || source.next != 3 || source.closed != 0 {
		t.Fatalf("writes %d, reads %d, closes %d", sink.writes, source.next, source.closed)
	}
	frames := streamTestFrames(t, sink.Body.String())
	if len(frames) != 2 || frames[1].Type != "content_block_start" {
		t.Fatalf("events written after failed input: %v", frames)
	}
}

// Error terminals must not complete or fabricate pending prototype arguments.
func TestStreamPendingToolInputIsNotCompletedOnError(t *testing.T) {
	for _, terminal := range []ir.StreamEvent{
		ir.NewErrorEvent("overloaded", "busy"),
		ir.NewMessageStop(ir.StopError),
	} {
		source := &streamTestSource{events: []ir.StreamEvent{
			ir.NewMessageStart("m", ir.RoleAssistant, "model", ir.Usage{}),
			ir.NewBlockStart(0, ir.NewToolUseBlock("call", "run", json.RawMessage(`{"pending":true}`))),
			terminal,
			ir.NewMessageStop(ir.StopToolUse),
		}}
		recorder := httptest.NewRecorder()
		writer, err := sse.NewWriter(recorder)
		if err != nil {
			t.Fatal(err)
		}
		if err := (StreamEncoder{}).EncodeStream(context.Background(), source, writer); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, frame := range streamTestFrames(t, recorder.Body.String()) {
			got = append(got, frame.Type)
		}
		want := []string{"message_start", "content_block_start", "error"}
		if !reflect.DeepEqual(got, want) || source.next != 3 || source.closed != 0 {
			t.Fatalf("error completion: events %v, reads %d, closes %d", got, source.next, source.closed)
		}
	}
}

func streamAssertGolden(t *testing.T, body, name string) {
	t.Helper()
	golden, err := os.ReadFile("testdata/sse/" + name + ".sse")
	if err != nil {
		t.Fatal(err)
	}
	got, want := streamTestFrames(t, body), streamTestFrames(t, string(golden))
	if len(got) != len(want) {
		t.Fatalf("frames %d, want %d\n%s", len(got), len(want), body)
	}
	for i := range got {
		var gotJSON, wantJSON any
		if err := json.Unmarshal([]byte(got[i].Data), &gotJSON); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(want[i].Data), &wantJSON); err != nil {
			t.Fatal(err)
		}
		if got[i].Type != want[i].Type || !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Fatalf("frame %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func streamTestFrames(t *testing.T, body string) []sse.Event {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	defer reader.Close()
	var events []sse.Event
	for {
		event, err := reader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

type streamTestSource struct {
	events         []ir.StreamEvent
	next, closed   int
	readErr        error
	waitAt         int
	ready, release chan struct{}
}

func (s *streamTestSource) Next(ctx context.Context) (ir.StreamEvent, error) {
	if s.ready != nil && s.next == s.waitAt {
		close(s.ready)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ir.StreamEvent{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if s.next == len(s.events) {
		if s.readErr != nil {
			return ir.StreamEvent{}, s.readErr
		}
		return ir.StreamEvent{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (s *streamTestSource) Close() error { s.closed++; return nil }

type streamTestSink struct {
	*httptest.ResponseRecorder
	failAfter, writes int
	failure           error
	onWrite           func([]byte)
}

func (w *streamTestSink) Write(data []byte) (int, error) {
	w.writes++
	if w.failAfter >= 0 && w.writes > w.failAfter {
		return 0, w.failure
	}
	if w.onWrite != nil {
		w.onWrite(data)
	}
	return w.ResponseRecorder.Write(data)
}

var _ http.ResponseWriter = (*streamTestSink)(nil)
