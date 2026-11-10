package adapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

const protocolCustom ir.Protocol = "custom:example"

// CustomInbound implements adapter.Inbound for a simple custom protocol.
type CustomInbound struct{}

var _ adapter.Inbound = (*CustomInbound)(nil)

func (CustomInbound) Protocol() ir.Protocol {
	return protocolCustom
}

type customInboundRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

func (CustomInbound) DecodeRequest(req *http.Request) (*ir.Request, error) {
	var body customInboundRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Message: "invalid JSON payload: " + err.Error(),
		}
	}
	if body.Prompt == "" {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Message: "missing required field: prompt",
		}
	}
	return &ir.Request{
		Model: body.Model,
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock(body.Prompt)),
		},
	}, nil
}

type customInboundResponse struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Output       string `json:"output"`
	FinishReason string `json:"finish_reason"`
	Tokens       int    `json:"tokens"`
}

func (CustomInbound) EncodeResponse(resp *ir.Response) ([]byte, error) {
	text := ""
	if len(resp.Content) > 0 {
		text = resp.Content[0].Text
	}
	wire := customInboundResponse{
		ID:           resp.ID,
		Model:        resp.Model,
		Output:       text,
		FinishReason: string(resp.StopReason),
		Tokens:       resp.Usage.OutputTokens,
	}
	return json.Marshal(wire)
}

func (CustomInbound) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	for {
		ev, err := events.Next(ctx)
		if err != nil {
			return err
		}

		var frame sse.Event
		switch ev.Type {
		case ir.EventMessageStart:
			data, err := json.Marshal(map[string]string{
				"id":    ev.Message.ID,
				"model": ev.Message.Model,
			})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "start", Data: string(data)}
		case ir.EventBlockDelta:
			data, err := json.Marshal(map[string]string{
				"text": ev.Delta.Text,
			})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "delta", Data: string(data)}
		case ir.EventUsageUpdate:
			data, err := json.Marshal(map[string]int{
				"output_tokens": ev.Usage.OutputTokens,
			})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "usage", Data: string(data)}
		case ir.EventMessageStop:
			frame = sse.Event{Type: "stop", Data: fmt.Sprintf(`{"reason":%q}`, ev.StopReason)}
		case ir.EventError:
			data, err := json.Marshal(map[string]string{
				"kind":    ev.Error.Kind,
				"message": ev.Error.Message,
			})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "error", Data: string(data)}
		default:
			continue
		}

		if err := w.WriteEvent(frame); err != nil {
			return err
		}

		if ev.Type == ir.EventMessageStop || ev.Type == ir.EventError {
			return nil
		}
	}
}

// CustomOutbound implements adapter.Outbound for a simple custom upstream.
type CustomOutbound struct{}

var _ adapter.Outbound = (*CustomOutbound)(nil)

func (CustomOutbound) Protocol() ir.Protocol {
	return protocolCustom
}

func (CustomOutbound) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming: true,
	}
}

func (CustomOutbound) BuildRequest(ctx context.Context, req *ir.Request, t adapter.Target) (*http.Request, error) {
	prompt := ""
	if len(req.Messages) > 0 && len(req.Messages[0].Content) > 0 {
		prompt = req.Messages[0].Content[0].Text
	}

	payload, err := json.Marshal(map[string]string{
		"model":  t.Model,
		"prompt": prompt,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		t.BaseURL+"/v1/generate",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}

	httpReq.Header = t.Headers.Clone()
	httpReq.Header.Set("Content-Type", "application/json")
	return httpReq, nil
}

func (CustomOutbound) ParseResponse(resp *http.Response) (out *ir.Response, err error) {
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	if resp.StatusCode >= 400 {
		var errPayload struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&errPayload); err != nil {
			return nil, &gwerr.Error{
				Kind:           gwerr.KindFromHTTPStatus(resp.StatusCode),
				Status:         resp.StatusCode,
				UpstreamStatus: resp.StatusCode,
				Message:        "upstream error with status " + resp.Status,
			}
		}
		return nil, &gwerr.Error{
			Kind:           gwerr.KindFromHTTPStatus(resp.StatusCode),
			Status:         resp.StatusCode,
			UpstreamStatus: resp.StatusCode,
			Message:        errPayload.Message,
		}
	}

	var data struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Result string `json:"result"`
		Tokens int    `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	res := ir.NewTextResponse(data.ID, data.Model, data.Result, ir.Usage{
		OutputTokens: data.Tokens,
	})
	return &res, nil
}

type customStream struct {
	reader    *sse.Reader
	body      io.Closer
	pending   []ir.StreamEvent
	blockOpen bool
}

func (s *customStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	for len(s.pending) == 0 {
		event, err := s.reader.Next(ctx)
		if err != nil {
			return ir.StreamEvent{}, err
		}

		switch event.Type {
		case "start":
			var p struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(event.Data), &p); err != nil {
				return ir.StreamEvent{}, err
			}
			s.pending = append(s.pending, ir.NewMessageStart(p.ID, ir.RoleAssistant, p.Model, ir.Usage{}))
		case "token":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(event.Data), &p); err != nil {
				return ir.StreamEvent{}, err
			}
			if !s.blockOpen {
				s.pending = append(s.pending, ir.NewBlockStart(0, ir.NewTextBlock("")))
				s.blockOpen = true
			}
			s.pending = append(s.pending, ir.NewTextDelta(0, p.Text))
		case "stop":
			if s.blockOpen {
				s.pending = append(s.pending, ir.NewBlockStop(0))
				s.blockOpen = false
			}
			s.pending = append(s.pending, ir.NewMessageStop(ir.StopEndTurn))
		case "error":
			var p struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(event.Data), &p); err != nil {
				return ir.StreamEvent{}, err
			}
			s.pending = append(s.pending, ir.NewErrorEvent(p.Kind, p.Message))
		}
	}

	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

func (s *customStream) Close() error {
	if s.reader != nil {
		s.reader.Close()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func (CustomOutbound) ParseStream(_ context.Context, resp *http.Response) (adapter.Stream, error) {
	reader := sse.NewReader(resp.Body)
	return &customStream{reader: reader, body: resp.Body}, nil
}

func TestExampleAdapter(t *testing.T) {
	t.Parallel()

	// 1. Registry verification
	reg := adapter.NewRegistry()
	in := CustomInbound{}
	out := CustomOutbound{}

	reg.RegisterInbound(in)
	reg.RegisterOutbound(out)

	inFound, ok := reg.Inbound(protocolCustom)
	if !ok || inFound == nil {
		t.Fatalf("expected inbound adapter to be registered")
	}

	outFound, ok := reg.Outbound(protocolCustom)
	if !ok || outFound == nil {
		t.Fatalf("expected outbound adapter to be registered")
	}

	// 2. Inbound Decode
	reqJSON := `{"model":"custom-test","prompt":"Hello world"}`
	httpReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat", strings.NewReader(reqJSON))
	irReq, err := in.DecodeRequest(httpReq)
	if err != nil {
		t.Fatalf("DecodeRequest failed: %v", err)
	}
	if irReq.Model != "custom-test" {
		t.Errorf("got model %q, want custom-test", irReq.Model)
	}
	if len(irReq.Messages) != 1 || irReq.Messages[0].Content[0].Text != "Hello world" {
		t.Errorf("unexpected messages: %+v", irReq.Messages)
	}

	// 3. Outbound BuildRequest
	target := adapter.Target{
		BaseURL: "https://api.example.com",
		Model:   "custom-test-v1",
		Headers: http.Header{"Authorization": []string{"Bearer test-key"}},
	}
	outReq, err := out.BuildRequest(context.Background(), irReq, target)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if outReq.URL.String() != "https://api.example.com/v1/generate" {
		t.Errorf("unexpected URL: %s", outReq.URL.String())
	}
	if outReq.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("target headers not preserved")
	}

	// 4. Outbound ParseResponse
	respJSON := `{"id":"resp-123","model":"custom-test-v1","result":"Completed text","tokens":15}`
	httpResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(respJSON)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	irResp, err := out.ParseResponse(httpResp)
	if err != nil {
		t.Fatalf("ParseResponse failed: %v", err)
	}
	if irResp.ID != "resp-123" || irResp.Content[0].Text != "Completed text" {
		t.Errorf("unexpected IR response: %+v", irResp)
	}

	// 5. Inbound EncodeResponse
	wireBytes, err := in.EncodeResponse(irResp)
	if err != nil {
		t.Fatalf("EncodeResponse failed: %v", err)
	}
	var encodedResp customInboundResponse
	if err := json.Unmarshal(wireBytes, &encodedResp); err != nil {
		t.Fatalf("unmarshal encoded response failed: %v", err)
	}
	if encodedResp.Output != "Completed text" || encodedResp.Tokens != 15 {
		t.Errorf("unexpected encoded response: %+v", encodedResp)
	}
}

type memoryStream struct {
	events []ir.StreamEvent
	index  int
}

func (s *memoryStream) Next(_ context.Context) (ir.StreamEvent, error) {
	if s.index >= len(s.events) {
		return ir.StreamEvent{}, io.EOF
	}
	ev := s.events[s.index]
	s.index++
	return ev, nil
}

func (s *memoryStream) Close() error {
	return nil
}

func TestExampleStreamSequencing(t *testing.T) {
	t.Parallel()

	// 1. Verify outbound ParseStream decodes SSE chunks and satisfies event ordering
	const ssePayload = "event: start\ndata: {\"id\":\"msg-1\",\"model\":\"custom-test\"}\n\n" +
		"event: token\ndata: {\"text\":\"chunk 1\"}\n\n" +
		"event: token\ndata: {\"text\":\"chunk 2\"}\n\n" +
		"event: stop\ndata: {}\n\n"

	out := CustomOutbound{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(ssePayload)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	parsedStream, err := out.ParseStream(context.Background(), resp)
	if err != nil {
		t.Fatalf("ParseStream failed: %v", err)
	}
	defer func() {
		if closeErr := parsedStream.Close(); closeErr != nil {
			t.Errorf("parsedStream.Close failed: %v", closeErr)
		}
	}()

	var parsedEvents []ir.StreamEvent
	for {
		ev, err := parsedStream.Next(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("parsedStream.Next failed: %v", err)
		}
		parsedEvents = append(parsedEvents, ev)
	}

	if err := ir.ValidateEventOrder(parsedEvents); err != nil {
		t.Fatalf("ValidateEventOrder rejected parsed stream: %v", err)
	}

	if len(parsedEvents) != 6 {
		t.Fatalf("got %d events, want 6", len(parsedEvents))
	}
	if parsedEvents[0].Type != ir.EventMessageStart || parsedEvents[0].Message.ID != "msg-1" {
		t.Errorf("unexpected event 0: %+v", parsedEvents[0])
	}
	if parsedEvents[1].Type != ir.EventBlockStart || parsedEvents[1].Index != 0 {
		t.Errorf("unexpected event 1: %+v", parsedEvents[1])
	}
	if parsedEvents[2].Type != ir.EventBlockDelta || parsedEvents[2].Delta.Text != "chunk 1" {
		t.Errorf("unexpected event 2: %+v", parsedEvents[2])
	}
	if parsedEvents[3].Type != ir.EventBlockDelta || parsedEvents[3].Delta.Text != "chunk 2" {
		t.Errorf("unexpected event 3: %+v", parsedEvents[3])
	}
	if parsedEvents[4].Type != ir.EventBlockStop || parsedEvents[4].Index != 0 {
		t.Errorf("unexpected event 4: %+v", parsedEvents[4])
	}
	if parsedEvents[5].Type != ir.EventMessageStop || parsedEvents[5].StopReason != ir.StopEndTurn {
		t.Errorf("unexpected event 5: %+v", parsedEvents[5])
	}

	// 2. Inbound EncodeStream verification
	events := []ir.StreamEvent{
		ir.NewMessageStart("msg-1", ir.RoleAssistant, "custom-test", ir.Usage{}),
		ir.NewBlockStart(0, ir.NewTextBlock("")),
		ir.NewTextDelta(0, "chunk 1"),
		ir.NewTextDelta(0, "chunk 2"),
		ir.NewBlockStop(0),
		ir.NewUsageUpdate(ir.Usage{OutputTokens: 10}),
		ir.NewMessageStop(ir.StopEndTurn),
	}

	// Invariant check
	if err := ir.ValidateEventOrder(events); err != nil {
		t.Fatalf("ValidateEventOrder rejected valid sequence: %v", err)
	}

	in := CustomInbound{}
	rec := httptest.NewRecorder()
	writer, err := sse.NewWriter(rec)
	if err != nil {
		t.Fatalf("sse.NewWriter failed: %v", err)
	}

	stream := &memoryStream{events: events}
	if err := in.EncodeStream(context.Background(), stream, writer); err != nil {
		t.Fatalf("EncodeStream failed: %v", err)
	}

	output := rec.Body.String()
	if !strings.Contains(output, "event: start") ||
		!strings.Contains(output, "event: delta") ||
		!strings.Contains(output, "event: usage") ||
		!strings.Contains(output, "event: stop") {
		t.Errorf("SSE stream output missing expected events: %s", output)
	}
}

func Example() {
	reg := adapter.NewRegistry()
	in := CustomInbound{}
	out := CustomOutbound{}

	reg.RegisterInbound(in)
	reg.RegisterOutbound(out)

	inAdapter, _ := reg.Inbound(protocolCustom)
	outAdapter, _ := reg.Outbound(protocolCustom)

	fmt.Printf("Inbound: %s\n", inAdapter.Protocol())
	fmt.Printf("Outbound: %s (streaming=%v)\n", outAdapter.Protocol(), outAdapter.Capabilities().Streaming)
	// Output:
	// Inbound: custom:example
	// Outbound: custom:example (streaming=true)
}
