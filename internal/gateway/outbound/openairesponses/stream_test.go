package openairesponses_test

import (
	"context"
	"errors"
	"io"
	"net/http"
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
