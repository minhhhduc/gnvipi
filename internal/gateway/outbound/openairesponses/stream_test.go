package openairesponses_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
)

func TestStreamContextCancellation(t *testing.T) {
	out := openairesponses.New()
	raw := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancel\",\"model\":\"gpt-4o\"}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := out.ParseStream(ctx, resp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	ev, err := stream.Next(ctx)
	if err != nil || ev.Type != ir.EventMessageStart {
		t.Fatalf("first event = %+v, %v", ev, err)
	}

	cancel()
	_, err = stream.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestStreamUnknownEventsIgnored(t *testing.T) {
	out := openairesponses.New()
	raw := "event: unknown.custom_event\ndata: {\"type\":\"unknown.custom_event\"}\n\n" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_0\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"Hi\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
	stream, err := out.ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var events []ir.StreamEvent
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != ir.EventMessageStart {
		t.Errorf("expected message_start, got %s", events[0].Type)
	}
	if events[4].Type != ir.EventMessageStop {
		t.Errorf("expected message_stop, got %s", events[4].Type)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	out := openairesponses.New()
	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	stream, err := out.ParseStream(ctx, resp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pw.Close(); err != nil {
			t.Error(err)
		}
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	_, err = stream.Next(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestStreamSingleAndParallelFunctionCalls(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_parallel\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"city\\\":\\\"\"}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"Paris\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"time\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"tz\\\":\\\"CET\\\"}\"}\n\n" +
		"event: response.function_call_arguments.done\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":1,\"arguments\":\"{\\\"tz\\\":\\\"CET\\\"}\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_parallel\",\"model\":\"gpt-4o\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
	stream, err := openairesponses.New().ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatalf("ParseStream error: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var events []ir.StreamEvent
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next error: %v", err)
		}
		events = append(events, ev)
	}

	// Verify sequence:
	// MessageStart
	// BlockStart 0 (weather)
	// BlockDelta 0 (city)
	// BlockDelta 0 (Paris)
	// BlockStop 0
	// BlockStart 1 (time)
	// BlockDelta 1 (tz)
	// BlockStop 1
	// UsageUpdate
	// MessageStop (ir.StopToolUse)
	if len(events) != 10 {
		t.Fatalf("expected 10 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != ir.EventMessageStart {
		t.Errorf("events[0] = %s", events[0].Type)
	}

	// Block 0
	if events[1].Type != ir.EventBlockStart || events[1].Index != 0 || events[1].Block.ToolUse.ID != "call_1" {
		t.Errorf("events[1] = %+v", events[1])
	}
	if events[2].Type != ir.EventBlockDelta || events[2].Index != 0 || events[2].Delta.PartialJSON != `{"city":"` {
		t.Errorf("events[2] = %+v", events[2])
	}
	if events[3].Type != ir.EventBlockDelta || events[3].Index != 0 || events[3].Delta.PartialJSON != `Paris"}` {
		t.Errorf("events[3] = %+v", events[3])
	}
	if events[4].Type != ir.EventBlockStop || events[4].Index != 0 {
		t.Errorf("events[4] = %+v", events[4])
	}

	// Block 1
	if events[5].Type != ir.EventBlockStart || events[5].Index != 1 || events[5].Block.ToolUse.ID != "call_2" {
		t.Errorf("events[5] = %+v", events[5])
	}
	if events[6].Type != ir.EventBlockDelta || events[6].Index != 1 || events[6].Delta.PartialJSON != `{"tz":"CET"}` {
		t.Errorf("events[6] = %+v", events[6])
	}
	if events[7].Type != ir.EventBlockStop || events[7].Index != 1 {
		t.Errorf("events[7] = %+v", events[7])
	}

	// Usage update and stop
	if events[8].Type != ir.EventUsageUpdate || events[8].Usage.InputTokens != 10 || events[8].Usage.OutputTokens != 20 {
		t.Errorf("events[8] = %+v", events[8])
	}
	if events[9].Type != ir.EventMessageStop || events[9].StopReason != ir.StopToolUse {
		t.Errorf("events[9] = %+v", events[9])
	}
}

func TestStreamFunctionCallArgumentsDoneWithoutDeltas(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_direct_args\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_direct\",\"name\":\"lookup\"}}\n\n" +
		"event: response.function_call_arguments.done\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"arguments\":\"{\\\"query\\\":\\\"golang\\\"}\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_direct_args\",\"status\":\"completed\"}}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
	stream, err := openairesponses.New().ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatalf("ParseStream error: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var events []ir.StreamEvent
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next error: %v", err)
		}
		events = append(events, ev)
	}

	var deltas []string
	for _, ev := range events {
		if ev.Type == ir.EventBlockDelta && ev.Delta.Type == ir.DeltaToolArguments {
			deltas = append(deltas, ev.Delta.PartialJSON)
		}
	}

	if len(deltas) != 1 || deltas[0] != `{"query":"golang"}` {
		t.Fatalf("expected 1 delta with arguments, got: %+v", deltas)
	}
}

func TestStreamOutputItemDoneToolArgumentsFallback(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fallback\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_fallback\",\"name\":\"search\"}}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_fallback\",\"name\":\"search\",\"arguments\":\"{\\\"query\\\":\\\"golang\\\"}\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fallback\",\"status\":\"completed\"}}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
	stream, err := openairesponses.New().ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatalf("ParseStream error: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var events []ir.StreamEvent
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != ir.EventMessageStart {
		t.Errorf("events[0] want message_start, got %s", events[0].Type)
	}
	if events[1].Type != ir.EventBlockStart || events[1].Index != 0 || events[1].Block.ToolUse == nil || events[1].Block.ToolUse.ID != "call_fallback" {
		t.Errorf("events[1] want block_start for call_fallback, got %+v", events[1])
	}
	if events[2].Type != ir.EventBlockDelta || events[2].Index != 0 || events[2].Delta.Type != ir.DeltaToolArguments || events[2].Delta.PartialJSON != `{"query":"golang"}` {
		t.Errorf("events[2] want tool argument delta, got %+v", events[2])
	}
	if events[3].Type != ir.EventBlockStop || events[3].Index != 0 {
		t.Errorf("events[3] want block_stop at index 0, got %+v", events[3])
	}
	if events[4].Type != ir.EventMessageStop || events[4].StopReason != ir.StopToolUse {
		t.Errorf("events[4] want message_stop with tool_use, got %+v", events[4])
	}
}

func TestStreamOutputItemDoneDoesNotDuplicateArgumentsWhenDeltasReceived(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_no_dup\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_dup\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"query\\\":\\\"golang\\\"}\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_0\",\"type\":\"function_call\",\"call_id\":\"call_dup\",\"name\":\"search\",\"arguments\":\"{\\\"query\\\":\\\"golang\\\"}\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_no_dup\",\"status\":\"completed\"}}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
	stream, err := openairesponses.New().ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatalf("ParseStream error: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var deltas []string
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next error: %v", err)
		}
		if ev.Type == ir.EventBlockDelta && ev.Delta.Type == ir.DeltaToolArguments {
			deltas = append(deltas, ev.Delta.PartialJSON)
		}
	}

	if len(deltas) != 1 || deltas[0] != `{"query":"golang"}` {
		t.Fatalf("expected exactly 1 argument delta, got: %+v", deltas)
	}
}

func TestStreamDeterministicBlockStopOnCompletion(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_multi\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_0\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_multi\",\"status\":\"completed\"}}\n\n"

	for iter := 0; iter < 20; iter++ {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
		stream, err := openairesponses.New().ParseStream(t.Context(), resp)
		if err != nil {
			t.Fatalf("ParseStream error: %v", err)
		}

		var stoppedIndices []int
		for {
			ev, err := stream.Next(t.Context())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("stream.Next error: %v", err)
			}
			if ev.Type == ir.EventBlockStop {
				stoppedIndices = append(stoppedIndices, ev.Index)
			}
		}
		if err := stream.Close(); err != nil {
			t.Error(err)
		}

		wantIndices := []int{0, 1, 2}
		if !slices.Equal(stoppedIndices, wantIndices) {
			t.Fatalf("iter %d: stoppedIndices = %+v, want %+v", iter, stoppedIndices, wantIndices)
		}
	}
}

func TestStreamDeterministicBlockStopOnEOF(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof\",\"model\":\"gpt-4o\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":3,\"item\":{\"id\":\"msg_3\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_0\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n"

	for iter := 0; iter < 20; iter++ {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
		stream, err := openairesponses.New().ParseStream(t.Context(), resp)
		if err != nil {
			t.Fatalf("ParseStream error: %v", err)
		}

		var stoppedIndices []int
		for {
			ev, err := stream.Next(t.Context())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("stream.Next error: %v", err)
			}
			if ev.Type == ir.EventBlockStop {
				stoppedIndices = append(stoppedIndices, ev.Index)
			}
		}
		if err := stream.Close(); err != nil {
			t.Error(err)
		}

		wantIndices := []int{0, 2, 3}
		if !slices.Equal(stoppedIndices, wantIndices) {
			t.Fatalf("iter %d: stoppedIndices = %+v, want %+v", iter, stoppedIndices, wantIndices)
		}
	}
}
