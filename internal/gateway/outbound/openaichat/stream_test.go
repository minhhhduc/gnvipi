package openaichat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	inboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

func TestStreamContextCancellation(t *testing.T) {
	out := openaichat.New()
	body := strings.NewReader("data: {\"id\":\"cancel\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"start\"}}]}\n\n")
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

func TestStreamRobustnessToCommentsAndMissingRole(t *testing.T) {
	out := openaichat.New()
	raw := ": keep-alive comment\n\n" +
		"data: \n\n" +
		"data: {\"choices\":[]}\n\n" +
		"data: {\"id\":\"no-role\",\"model\":\"chat-fast\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: [DONE]\n\n"
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
	if events[0].Type != ir.EventMessageStart || events[0].Message.ID != "no-role" || events[0].Message.Model != "chat-fast" {
		t.Fatalf("message_start mismatch: %+v", events[0])
	}
	if events[1].Type != ir.EventBlockStart || events[1].Block.Type != ir.BlockText {
		t.Fatalf("block_start mismatch: %+v", events[1])
	}
	if events[2].Type != ir.EventBlockDelta || events[2].Delta.Text != "Hello" {
		t.Fatalf("block_delta mismatch: %+v", events[2])
	}
	if events[3].Type != ir.EventBlockStop {
		t.Fatalf("block_stop mismatch: %+v", events[3])
	}
	if events[4].Type != ir.EventMessageStop || events[4].StopReason != ir.StopEndTurn {
		t.Fatalf("message_stop mismatch: %+v", events[4])
	}
}

func TestStreamClosesBody(t *testing.T) {
	out := openaichat.New()
	tracked := &trackedBody{Reader: strings.NewReader("data: [DONE]\n\n")}
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

func TestStreamErrorStatusCode(t *testing.T) {
	out := openaichat.New()
	tracked := &trackedBody{Reader: strings.NewReader(`{"error":{"message":"bad request","type":"invalid_request_error"}}`)}
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: tracked}

	stream, err := out.ParseStream(t.Context(), resp)
	if stream != nil {
		t.Fatal("expected nil stream on 400 status")
	}
	if err == nil {
		t.Fatal("expected error on 400 status")
	}
	if !tracked.closed {
		t.Fatal("ParseStream failed to close body on HTTP error status")
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	out := openaichat.New()
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

func TestStreamToolCallsSingleAndParallel(t *testing.T) {
	out := openaichat.New()
	sseData := "data: {\"id\":\"cmpl_stream_tools\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"cmpl_stream_tools\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_weather\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"\"}},{\"index\":1,\"id\":\"call_clock\",\"type\":\"function\",\"function\":{\"name\":\"clock\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_stream_tools\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"\"}},{\"index\":1,\"function\":{\"arguments\":\"{\\\"format\\\":\\\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_stream_tools\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"Paris\\\"}\"}},{\"index\":1,\"function\":{\"arguments\":\"24h\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_stream_tools\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sseData))}
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

	if len(events) < 7 {
		t.Fatalf("expected at least 7 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != ir.EventMessageStart || events[0].Message.ID != "cmpl_stream_tools" {
		t.Fatalf("event 0 mismatch: %+v", events[0])
	}

	// Find block starts
	var toolStarts []ir.StreamEvent
	var toolDeltas []ir.StreamEvent
	var toolStops []ir.StreamEvent
	var messageStop *ir.StreamEvent
	for _, ev := range events {
		switch ev.Type {
		case ir.EventBlockStart:
			toolStarts = append(toolStarts, ev)
		case ir.EventBlockDelta:
			toolDeltas = append(toolDeltas, ev)
		case ir.EventBlockStop:
			toolStops = append(toolStops, ev)
		case ir.EventMessageStop:
			evCopy := ev
			messageStop = &evCopy
		}
	}

	if len(toolStarts) != 2 {
		t.Fatalf("expected 2 tool block starts, got %d", len(toolStarts))
	}
	if toolStarts[0].Block.ToolUse.ID != "call_weather" || toolStarts[0].Block.ToolUse.Name != "weather" {
		t.Fatalf("tool 0 mismatch: %+v", toolStarts[0].Block.ToolUse)
	}
	if toolStarts[1].Block.ToolUse.ID != "call_clock" || toolStarts[1].Block.ToolUse.Name != "clock" {
		t.Fatalf("tool 1 mismatch: %+v", toolStarts[1].Block.ToolUse)
	}

	if len(toolDeltas) != 4 {
		t.Fatalf("expected 4 tool deltas, got %d", len(toolDeltas))
	}
	// Verify deltas carry the correct tool IDs and concatenated arguments
	argsByBlock := make(map[int]string)
	for _, d := range toolDeltas {
		if d.Delta.Type != ir.DeltaToolArguments {
			t.Fatalf("delta type = %s, want DeltaToolArguments", d.Delta.Type)
		}
		argsByBlock[d.Index] += d.Delta.PartialJSON
	}
	if argsByBlock[toolStarts[0].Index] != `{"city":"Paris"}` {
		t.Fatalf("tool 0 accumulated args = %q, want {\"city\":\"Paris\"}", argsByBlock[toolStarts[0].Index])
	}
	if argsByBlock[toolStarts[1].Index] != `{"format":"24h"}` {
		t.Fatalf("tool 1 accumulated args = %q, want {\"format\":\"24h\"}", argsByBlock[toolStarts[1].Index])
	}

	if len(toolStops) != 2 {
		t.Fatalf("expected 2 tool block stops, got %d", len(toolStops))
	}
	if messageStop == nil || messageStop.StopReason != ir.StopToolUse {
		t.Fatalf("message stop mismatch: %+v", messageStop)
	}
}

func TestStreamInvalidJSONArgumentsPreservedVerbatim(t *testing.T) {
	out := openaichat.New()
	sseData := "data: {\"id\":\"cmpl_inv\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"cmpl_inv\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_inv\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_inv\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{invalid: json\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_inv\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\", unclosed\"}}]}}]}\n\n" +
		"data: {\"id\":\"cmpl_inv\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sseData))}
	stream, err := out.ParseStream(t.Context(), resp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()

	var accumulated string
	for {
		ev, err := stream.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == ir.EventBlockDelta && ev.Delta.Type == ir.DeltaToolArguments {
			accumulated += ev.Delta.PartialJSON
		}
	}

	want := "{invalid: json, unclosed"
	if accumulated != want {
		t.Fatalf("accumulated invalid arguments = %q, want %q", accumulated, want)
	}
}

func TestChatReasoningStreamToResponses(t *testing.T) {
	raw := "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	stream, err := openaichat.New().ParseStream(t.Context(), &http.Response{
		StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close() //nolint:errcheck // test stream cleanup
	w := httptest.NewRecorder()
	writer, err := sse.NewWriter(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := (inboundresponses.StreamEncoder{}).EncodeStream(t.Context(), stream, writer); err != nil {
		t.Fatal(err)
	}
	reader := sse.NewReader(strings.NewReader(w.Body.String()))
	for {
		event, err := reader.Next(t.Context())
		if err != nil {
			t.Fatalf("missing completed response: %v; wire: %s", err, w.Body.String())
		}
		if event.Type != "response.completed" {
			continue
		}
		var result struct {
			Response struct {
				Output []struct {
					Type    string                  `json:"type"`
					Summary []struct{ Text string } `json:"summary"`
					Content []struct{ Text string } `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(event.Data), &result); err != nil {
			t.Fatal(err)
		}
		output := result.Response.Output
		if len(output) != 2 || output[0].Type != "reasoning" || len(output[0].Summary) != 1 || output[0].Summary[0].Text != "plan" ||
			output[1].Type != "message" || len(output[1].Content) != 1 || output[1].Content[0].Text != "ok" {
			t.Fatalf("lost reasoning or answer: %s", event.Data)
		}
		return
	}
}

// #457: a separator-only reasoning delta must not open a thinking block that
// holds nothing the client can replay.
func TestStreamDropsWhitespaceOnlyReasoning(t *testing.T) {
	out := openaichat.New()
	raw := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\" \"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
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

	var starts int
	var deltas []string
	for _, ev := range events {
		if ev.Type == ir.EventBlockStart && ev.Block.Type == ir.BlockReasoning {
			starts++
		}
		if ev.Type == ir.EventBlockDelta && ev.Delta.Text != "" {
			deltas = append(deltas, ev.Delta.Text)
		}
	}
	if starts != 1 {
		t.Fatalf("reasoning block starts = %d, want 1: %+v", starts, events)
	}
	if strings.Join(deltas, "|") != "plan|hi" {
		t.Fatalf("deltas = %v, want [plan hi]", deltas)
	}
}
