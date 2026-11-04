package outbound_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// chatAdapter is a functional OpenAI-shaped example, not a production adapter.
// It copies resolved headers and deliberately knows nothing about provider keys.
type chatAdapter struct{}

func (chatAdapter) Protocol() ir.Protocol              { return ir.ProtocolOpenAIChat }
func (chatAdapter) Capabilities() adapter.Capabilities { return adapter.Capabilities{Streaming: true} }
func (chatAdapter) BuildRequest(ctx context.Context, req *ir.Request, target adapter.Target) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{"model": target.Model, "messages": req.Messages, "stream": req.Stream, "temperature": 0.2})
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header = target.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Content-Type", "application/json")
	return r, nil
}
func (chatAdapter) ParseResponse(resp *http.Response) (*ir.Response, error) {
	var wire struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}
	if len(wire.Choices) != 1 {
		return nil, errors.New("want one chat choice")
	}
	r := ir.NewTextResponse(wire.ID, wire.Model, wire.Choices[0].Message.Content, ir.Usage{})
	return &r, nil
}
func (chatAdapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	return &chatStream{reader: sse.NewReader(resp.Body), body: resp.Body}, nil
}

type chatStream struct {
	reader *sse.Reader
	body   io.Closer
}

func (s *chatStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	event, err := s.reader.Next(ctx)
	if err != nil {
		return ir.StreamEvent{}, err
	}
	if event.Data == "[DONE]" {
		return ir.StreamEvent{}, io.EOF
	}
	return ir.StreamEvent{}, errors.New("unexpected chat stream event")
}
func (s *chatStream) Close() error { s.reader.Close(); return s.body.Close() }

func TestDeclarativeChatReachesUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Vendor-Key") != "Bearer resolved-secret" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong resolved auth: %v", r.Header)
		}
		if r.Header.Get("X-App") != "hook" {
			t.Error("request hook missing")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for key, want := range map[string]string{"model": `"actual-model"`, "stream": "false", "tenant": `"required"`, "temperature": "0.7"} {
			if string(body[key]) != want {
				t.Errorf("%s = %s, want %s", key, body[key], want)
			}
		}
		if r.ContentLength <= 0 {
			t.Error("merged request framing not updated")
		}
		w.Header().Set("X-Result", "upstream")
		if _, err := io.WriteString(w, `{"id":"chat-1","model":"actual-model","choices":[{"message":{"content":"works"}}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	p := provider.Provider{Protocol: ir.ProtocolOpenAIChat, EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/api/chat"}, ExtraBody: json.RawMessage(`{"tenant":"required","temperature":0.7}`), Auth: provider.Auth{Scheme: provider.AuthBearer, Key: "not-the-resolved-secret", HeaderName: "X-Vendor-Key"}}
	a, err := outbound.Configure(chatAdapter{}, &p)
	if err != nil {
		t.Fatal(err)
	}
	a = outbound.Wrap(a, outbound.Hooks{
		Request: func(ctx context.Context, r *http.Request) error { r.Header.Set("X-App", "hook"); return nil },
		Response: func(ctx context.Context, r *http.Response) error {
			if r.Header.Get("X-Result") != "upstream" {
				return errors.New("wrong response")
			}
			return nil
		},
	})
	headers := http.Header{"Authorization": {"Bearer resolved-secret"}, "X-Vendor-Key": {"stale-static"}}
	r, err := a.BuildRequest(t.Context(), &ir.Request{Model: "alias"}, adapter.Target{BaseURL: server.URL, Model: "actual-model", Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Authorization") != "Bearer resolved-secret" || headers.Get("X-Vendor-Key") != "stale-static" {
		t.Fatal("mutated shared target headers")
	}
	// A replay must have the merged body, not the base adapter's old body.
	replay, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	replayBytes, err := io.ReadAll(replay)
	if err := replay.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil || !bytes.Contains(replayBytes, []byte(`"tenant":"required"`)) {
		t.Fatalf("replay body = %s, %v", replayBytes, err)
	}
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "chat-1" || result.Content[0].Text != "works" {
		t.Fatalf("parsed response = %#v", result)
	}
}

func TestCustomSelectedThroughProviderConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/chat" {
			t.Errorf("custom path = %s", r.URL.Path)
		}
		if _, err := io.WriteString(w, `{"id":"custom-1","model":"m","choices":[{"message":{"content":"custom works"}}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	registry := adapter.NewRegistry()
	outbound.RegisterCustom(registry, "vendor", chatAdapter{})
	var p provider.Provider
	if err := json.Unmarshal([]byte(`{"id":"vendor","name":"vendor","base_url":"`+server.URL+`/prefix","protocol":"custom:vendor","endpoint_path":{"openai-chat":"/api/chat"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	a, err := outbound.Select(registry, &p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Protocol() != p.Protocol {
		t.Fatalf("selected protocol = %s", a.Protocol())
	}
	r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: p.BaseURL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := a.ParseResponse(resp)
	if err != nil || result == nil || result.ID != "custom-1" {
		t.Fatalf("custom response = %#v, %v", result, err)
	}
	p.Protocol = "custom:missing"
	if _, err := outbound.Select(registry, &p); err == nil {
		t.Fatal("missing custom silently fell back")
	}
	if _, ok := adapter.NewRegistry().Outbound("custom:vendor"); ok {
		t.Fatal("registration leaked into another registry")
	}
}

func TestCustomRegistrationConcurrentDuplicateSafety(t *testing.T) {
	registry := adapter.NewRegistry()
	var registered, panicked atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panicked.Add(1)
				}
			}()
			outbound.RegisterCustom(registry, "one", chatAdapter{})
			registered.Add(1)
		}()
	}
	wg.Wait()
	if registered.Load() != 1 || panicked.Load() != 15 {
		t.Fatalf("successes=%d duplicates=%d", registered.Load(), panicked.Load())
	}
	if _, ok := registry.Outbound("custom:one"); !ok {
		t.Fatal("winning registration missing")
	}
}

type closeBody struct {
	io.Reader
	closes int
}

func (b *closeBody) Close() error { b.closes++; return nil }

type buildAdapter struct {
	chatAdapter
	body *closeBody
}

func (a buildAdapter) BuildRequest(ctx context.Context, req *ir.Request, target adapter.Target) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+"/v1/chat/completions", a.body)
	return r, err
}

type rejectStream struct{ chatAdapter }

func (rejectStream) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, errors.New("bad stream")
}

func TestHookFailureClosesBodies(t *testing.T) {
	failure := errors.New("hook refused")
	body := &closeBody{Reader: strings.NewReader(`{}`)}
	a := outbound.Wrap(buildAdapter{body: body}, outbound.Hooks{Request: func(context.Context, *http.Request) error { return failure }})
	if r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: "https://up.invalid"}); r != nil || !errors.Is(err, failure) {
		t.Fatalf("request failure = %v, %v", r, err)
	}
	if body.closes != 1 {
		t.Fatalf("failed request body closed %d times", body.closes)
	}
	for _, streaming := range []bool{false, true} {
		body := &closeBody{Reader: strings.NewReader("data: [DONE]\n\n")}
		a := outbound.Wrap(chatAdapter{}, outbound.Hooks{Response: func(context.Context, *http.Response) error { return failure }})
		resp := &http.Response{Body: body}
		var err error
		if streaming {
			_, err = a.ParseStream(t.Context(), resp)
		} else {
			_, err = a.ParseResponse(resp)
		}
		if !errors.Is(err, failure) || body.closes != 1 {
			t.Fatalf("stream=%v error=%v closes=%d", streaming, err, body.closes)
		}
	}
	body = &closeBody{Reader: strings.NewReader("broken")}
	a = outbound.Wrap(rejectStream{}, outbound.Hooks{})
	if _, err := a.ParseStream(t.Context(), &http.Response{Body: body}); err == nil || body.closes != 1 {
		t.Fatalf("parse failure=%v closes=%d", err, body.closes)
	}
}

func TestStreamOwnershipAndCallerContext(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "caller"))
	defer cancel()
	body := &closeBody{Reader: strings.NewReader("data: [DONE]\n\n")}
	a := outbound.Wrap(chatAdapter{}, outbound.Hooks{
		Request: func(hookCtx context.Context, r *http.Request) error {
			if hookCtx.Value(contextKey{}) != "caller" || r.Context() != ctx {
				return errors.New("request lost caller context")
			}
			return nil
		},
		Response: func(hookCtx context.Context, r *http.Response) error {
			if hookCtx != ctx {
				return errors.New("stream lost caller context")
			}
			return nil
		},
	})
	r, err := a.BuildRequest(ctx, &ir.Request{Stream: true}, adapter.Target{BaseURL: "https://up.invalid", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	stream, err := a.ParseStream(ctx, &http.Response{Body: body, Request: r})
	if err != nil {
		t.Fatal(err)
	}
	if body.closes != 0 {
		t.Fatal("decorator closed a live response stream")
	}
	if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("stream terminator = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if body.closes != 1 {
		t.Fatalf("stream body closed %d times", body.closes)
	}
	cancel()
	if !errors.Is(r.Context().Err(), context.Canceled) {
		t.Fatal("upstream request detached from cancellation")
	}
}

func TestConfigureRejectsMaliciousConfigurationAndUnresolvedAuth(t *testing.T) {
	for _, p := range []provider.Provider{
		{ExtraBody: json.RawMessage(`{"model":"hijack"}`)},
		{ExtraBody: json.RawMessage(`{"stream":true}`)},
		{EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "//evil.invalid/chat"}},
		{EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/a\\b"}},
		{Auth: provider.Auth{Scheme: provider.AuthBearer, HeaderName: "Host"}},
	} {
		if _, err := outbound.Configure(chatAdapter{}, &p); err == nil {
			t.Fatalf("accepted %#v", p)
		}
	}
	a, err := outbound.Configure(chatAdapter{}, &provider.Provider{Auth: provider.Auth{Scheme: provider.AuthBearer, Key: "must-not-be-used", HeaderName: "X-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range []http.Header{nil, {"Authorization": {"a", "b"}}, {"Authorization": {"bad\r\nHost: evil"}}, {"Authorization": {"one"}, "authorization": {"two"}}} {
		if r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: "https://up.invalid", Model: "m", Headers: headers}); err == nil {
			if r != nil {
				if err := r.Body.Close(); err != nil {
					t.Error(err)
				}
			}
			t.Fatal("unresolved/ambiguous/unsafe auth was sent")
		}
	}
}

func TestExtraBodyPreservesStreamAndSnapshotsConfiguration(t *testing.T) {
	p := provider.Provider{EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/api/chat"}, ExtraBody: json.RawMessage(`{"nested":{"required":true},"large":9007199254740993}`)}
	a, err := outbound.Configure(chatAdapter{}, &p)
	if err != nil {
		t.Fatal(err)
	}
	p.EndpointPaths[ir.ProtocolOpenAIChat] = "//evil.invalid"
	copy(p.ExtraBody, `{"model":"bad"}`)
	r, err := a.BuildRequest(t.Context(), &ir.Request{Stream: true}, adapter.Target{BaseURL: "https://up.invalid", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if string(body["stream"]) != "true" || string(body["model"]) != `"m"` || string(body["large"]) != "9007199254740993" || r.URL.Path != "/api/chat" {
		t.Fatalf("configuration snapshot/routing fields changed: %v %s", body, r.URL)
	}
}
