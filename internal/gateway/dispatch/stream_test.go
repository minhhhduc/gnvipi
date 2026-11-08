package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

type fakeStreamInbound struct {
	protocol ir.Protocol
}

func (f fakeStreamInbound) Protocol() ir.Protocol { return f.protocol }

func (fakeStreamInbound) DecodeRequest(r *http.Request) (*ir.Request, error) {
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "decode failure"}
	}
	return &ir.Request{Model: body.Model, Stream: body.Stream, Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"))}}, nil
}

func (fakeStreamInbound) EncodeResponse(*ir.Response) ([]byte, error) {
	return []byte(`{"response":"non-stream"}`), nil
}

func (fakeStreamInbound) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := events.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ev.Type == ir.EventError {
			if err := w.WriteEvent(sse.Event{Type: "error", Data: ev.Error.Message}); err != nil {
				return err
			}
			return errors.New(ev.Error.Message)
		}
		data := string(ev.Type)
		if ev.Delta != nil {
			data = ev.Delta.Text
		}
		if err := w.WriteEvent(sse.Event{Type: string(ev.Type), Data: data}); err != nil {
			return err
		}
		if ev.Type == ir.EventMessageStop {
			return nil
		}
	}
}

type fakeStreamOutbound struct {
	protocol      ir.Protocol
	events        []ir.StreamEvent
	streamErr     error
	onParseStream func(*http.Response)
}

func (f fakeStreamOutbound) Protocol() ir.Protocol { return f.protocol }

func (fakeStreamOutbound) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Streaming: true}
}

func (fakeStreamOutbound) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+"/stream", strings.NewReader(`{"stream":true}`))
	if err != nil {
		return nil, err
	}
	req.Header = target.Headers.Clone()
	return req, nil
}

func (fakeStreamOutbound) ParseResponse(*http.Response) (*ir.Response, error) {
	return &ir.Response{ID: "resp_out"}, nil
}

func (f fakeStreamOutbound) ParseStream(_ context.Context, resp *http.Response) (adapter.Stream, error) {
	if f.onParseStream != nil {
		f.onParseStream(resp)
	}
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return &fakeStreamIterator{events: f.events, body: resp.Body}, nil
}

type fakeStreamIterator struct {
	events []ir.StreamEvent
	body   io.Closer
	idx    int
}

func (s *fakeStreamIterator) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if s.idx < len(s.events) {
		ev := s.events[s.idx]
		s.idx++
		return ev, nil
	}
	if reader, ok := s.body.(io.Reader); ok {
		buf := make([]byte, 1)
		if _, err := reader.Read(buf); err != nil {
			return ir.StreamEvent{}, err
		}
	}
	return ir.StreamEvent{}, io.EOF
}

func (s *fakeStreamIterator) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func TestStreamTranslationNormalFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: chunk\n\n")); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	prov := translationProvider(t, server.URL)
	events := []ir.StreamEvent{
		ir.NewMessageStart("msg_1", ir.RoleAssistant, "upstream-model", ir.Usage{InputTokens: 10}),
		ir.NewBlockStart(0, ir.Block{Type: ir.BlockText, Index: 0}),
		ir.NewTextDelta(0, "Hello "),
		ir.NewTextDelta(0, "world"),
		ir.NewBlockStop(0),
		ir.NewUsageUpdate(ir.Usage{InputTokens: 10, OutputTokens: 5}),
		ir.NewMessageStop(ir.StopEndTurn),
	}

	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{protocol: prov.Protocol, events: events}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	engine.ServeHTTP(out, req)

	if out.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", out.Code, out.Body.String())
	}
	if ct := out.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body := out.Body.String()
	if !strings.Contains(body, "Hello ") || !strings.Contains(body, "world") {
		t.Fatalf("stream body missing content: %s", body)
	}

	completion := receiveTranslationCompletion(t, recorder)
	if completion.Err != nil {
		t.Fatalf("unexpected completion error: %v", completion.Err)
	}
	if completion.Usage.InputTokens != 10 || completion.Usage.OutputTokens != 5 {
		t.Fatalf("completion usage = %+v, want input 10, output 5", completion.Usage)
	}
	if completion.Request.FirstByte.IsZero() || completion.Request.FirstByte.Before(completion.Request.Start) {
		t.Fatalf("invalid FirstByte timestamp: start=%v firstByte=%v", completion.Request.Start, completion.Request.FirstByte)
	}
	if completion.TTFB < 0 {
		t.Fatalf("TTFB = %v, want >= 0", completion.TTFB)
	}
}

func TestStreamTranslationPreStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	prov := translationProvider(t, server.URL)
	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{protocol: prov.Protocol}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	engine.ServeHTTP(out, req)

	if out.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body: %s", out.Code, out.Body.String())
	}
	completion := receiveTranslationCompletion(t, recorder)
	if completion.Err == nil {
		t.Fatal("expected completion error for 429 response")
	}
}

func TestStreamTranslationMidStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: start\n\n")); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	prov := translationProvider(t, server.URL)
	events := []ir.StreamEvent{
		ir.NewMessageStart("msg_err", ir.RoleAssistant, "upstream", ir.Usage{}),
		ir.NewTextDelta(0, "partial answer"),
		ir.NewErrorEvent("overloaded", "server overloaded"),
	}

	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{protocol: prov.Protocol, events: events}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	engine.ServeHTTP(out, req)

	if !strings.Contains(out.Body.String(), "server overloaded") {
		t.Fatalf("expected error event in body, got: %s", out.Body.String())
	}

	completion := receiveTranslationCompletion(t, recorder)
	if completion.Err == nil {
		t.Fatal("expected completion error on mid-stream failure")
	}
}

func TestStreamTranslationClientCancellationNoGoroutineLeaks(t *testing.T) {
	// Baseline goroutine count
	runtime.GC()
	baseline := runtime.NumGoroutine()

	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer server.Close()

	prov := translationProvider(t, server.URL)
	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{protocol: prov.Protocol}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(out, req)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never started")
	}

	// Cancel client request
	cancel()

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not canceled")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after cancellation")
	}

	_ = receiveTranslationCompletion(t, recorder)

	// Verify no goroutine leak (allow up to 2 for background runtime noise)
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
	}
	if cur := runtime.NumGoroutine(); cur > baseline+3 {
		t.Fatalf("goroutine leak: baseline=%d current=%d", baseline, cur)
	}
}

func TestStreamTranslationIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Hang until context canceled
		<-r.Context().Done()
	}))
	defer server.Close()

	prov := translationProvider(t, server.URL)
	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{protocol: prov.Protocol}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	engine.ServeHTTP(out, req)

	completion := receiveTranslationCompletion(t, recorder)
	if !errors.Is(completion.Err, context.DeadlineExceeded) && !errors.Is(completion.Err, context.Canceled) {
		t.Fatalf("expected timeout error in completion, got: %v", completion.Err)
	}
}

type closeTrackingBody struct {
	io.ReadCloser
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return b.ReadCloser.Close()
}

func TestStreamTranslationParseStreamErrorClosesResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("not-sse\n\n")); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer server.Close()

	var trackedBody *closeTrackingBody
	prov := translationProvider(t, server.URL)
	inbound := fakeStreamInbound{protocol: ir.ProtocolOpenAIChat}
	outbound := fakeStreamOutbound{
		protocol:  prov.Protocol,
		streamErr: &gwerr.Error{Kind: gwerr.KindInternal, Message: "invalid stream header"},
		onParseStream: func(resp *http.Response) {
			trackedBody = &closeTrackingBody{ReadCloser: resp.Body}
			resp.Body = trackedBody
		},
	}

	registry := adapter.NewRegistry()
	registry.RegisterInbound(inbound)
	registry.RegisterOutbound(outbound)

	recorder := newTranslationRecorder()
	dispatcher := translationDispatcher(t, &prov, registry, recorder)

	engine := gin.New()
	engine.Use(dispatcher.Handle)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-alias","stream":true}`))
	out := httptest.NewRecorder()

	engine.ServeHTTP(out, req)

	if trackedBody == nil || !trackedBody.closed {
		t.Fatal("expected response.Body to be closed when ParseStream returns an error")
	}
	completion := receiveTranslationCompletion(t, recorder)
	if completion.Err == nil {
		t.Fatal("expected completion error when ParseStream fails")
	}
}
