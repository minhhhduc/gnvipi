package anthropic_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
)

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestStreamContextCancellation(t *testing.T) {
	out := anthropic.New()
	body := strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cancel\",\"model\":\"claude\",\"usage\":{\"input_tokens\":5}}}\n\n")
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}

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

func TestStreamPingIgnoredAndMergedUsage(t *testing.T) {
	out := anthropic.New()
	raw := "event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ping\",\"model\":\"claude\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":5}}}\n\n" +
		": keepalive comment\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

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

	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d: %+v", len(events), events)
	}
	// Verify usage is merged from start and delta
	if events[4].Type != ir.EventUsageUpdate || events[4].Usage == nil {
		t.Fatalf("usage_update mismatch: %+v", events[4])
	}
	usage := events[4].Usage
	if usage.InputTokens != 10 || usage.OutputTokens != 2 || usage.CacheReadTokens != 5 {
		t.Fatalf("merged usage mismatch: %+v", usage)
	}
}

func TestStreamClosesBody(t *testing.T) {
	out := anthropic.New()
	tracked := &trackedBody{Reader: strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
	resp := &http.Response{StatusCode: http.StatusOK, Body: tracked}

	stream, err := out.ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatal(err)
	}
	if tracked.closed {
		t.Fatal("stream closed body prematurely")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !tracked.closed {
		t.Fatal("Stream.Close failed to close underlying response body")
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	out := anthropic.New()
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

func TestStreamToolEmulation(t *testing.T) {
	out := &anthropic.Adapter{ToolEmulation: true}
	rawSSE := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_emul\",\"model\":\"claude\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"person\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"name\\\":\\\"Bob\\\"}\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(rawSSE))}
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
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		events = append(events, ev)
	}

	if len(events) < 5 {
		t.Fatalf("expected at least 5 events, got %d", len(events))
	}
	// Verify block start converted to BlockText
	if events[1].Type != ir.EventBlockStart || events[1].Block.Type != ir.BlockText {
		t.Errorf("block_start event = %+v, want BlockText", events[1])
	}
	// Verify delta converted to TextDelta
	if events[2].Type != ir.EventBlockDelta || events[2].Delta.Type != ir.DeltaText || events[2].Delta.Text != `{"name":"Bob"}` {
		t.Errorf("block_delta event = %+v, want TextDelta with json", events[2])
	}
	var stopEvent *ir.StreamEvent
	for i := range events {
		if events[i].Type == ir.EventMessageStop {
			stopEvent = &events[i]
			break
		}
	}
	if stopEvent == nil || stopEvent.StopReason != ir.StopEndTurn {
		t.Errorf("message_stop event = %+v, want StopEndTurn", stopEvent)
	}
}

func TestStreamParallelToolCalling(t *testing.T) {
	out := anthropic.New()
	rawSSE := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_tool_stream\",\"model\":\"claude\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":1\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"23}\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_2\",\"name\":\"weather\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":15}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(rawSSE))}
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
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		events = append(events, ev)
	}

	if len(events) < 8 {
		t.Fatalf("expected at least 8 events, got %d", len(events))
	}
	// Block 0 start
	if events[1].Type != ir.EventBlockStart || events[1].Block.Type != ir.BlockToolUse || events[1].Block.ToolUse.ID != "call_1" {
		t.Errorf("block 0 start mismatch: %+v", events[1])
	}
	// Block 0 deltas
	if events[2].Type != ir.EventBlockDelta || events[2].Delta.Type != ir.DeltaToolArguments || events[2].Delta.ToolCallID != "call_1" {
		t.Errorf("block 0 delta 1 mismatch: %+v", events[2])
	}
	// Block 1 start
	if events[5].Type != ir.EventBlockStart || events[5].Block.Type != ir.BlockToolUse || events[5].Block.ToolUse.ID != "call_2" {
		t.Errorf("block 1 start mismatch: %+v", events[5])
	}
	// Block 1 delta
	if events[6].Type != ir.EventBlockDelta || events[6].Delta.Type != ir.DeltaToolArguments || events[6].Delta.ToolCallID != "call_2" {
		t.Errorf("block 1 delta mismatch: %+v", events[6])
	}

	var stopEvent *ir.StreamEvent
	for i := range events {
		if events[i].Type == ir.EventMessageStop {
			stopEvent = &events[i]
			break
		}
	}
	if stopEvent == nil || stopEvent.StopReason != ir.StopToolUse {
		t.Errorf("message_stop = %+v, want StopToolUse", stopEvent)
	}
}
