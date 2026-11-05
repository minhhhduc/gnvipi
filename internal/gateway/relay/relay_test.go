package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

type recording struct {
	mu    sync.Mutex
	calls []*core.Completion
}

func (r *recording) Record(c *core.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recording) completions() []*core.Completion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*core.Completion(nil), r.calls...)
}

func (r *recording) wait(t *testing.T, count int, timeout time.Duration) []*core.Completion {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		if len(r.calls) >= count {
			res := append([]*core.Completion(nil), r.calls...)
			r.mu.Unlock()
			return res
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d completion(s)", count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func formatCompletions(calls []*core.Completion) string {
	deref := make([]core.Completion, len(calls))
	for i, c := range calls {
		if c != nil {
			deref[i] = *c
		}
	}
	return fmt.Sprintf("%+v", deref)
}

func newTestRelay(t *testing.T, base string, protocol ir.Protocol, recorder core.Recorder) *Relay {
	t.Helper()
	factory := upstream.NewFactory()
	t.Cleanup(factory.CloseIdleConnections)
	p := &provider.Provider{
		ID: "relay-provider", Name: "Relay provider", BaseURL: base, Protocol: protocol,
		Auth:           provider.Auth{Scheme: provider.AuthBearer, Key: "opaque-relay-provider-credential"},
		Headers:        map[string]string{"X-Static": "configured"},
		ForwardHeaders: []string{"X-Client"},
	}
	if protocol == ir.ProtocolAnthropicMessages {
		p.Auth.Scheme = provider.AuthAPIKey
	}
	h, err := New(p, "upstream-model", &Options{Factory: factory, Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRelayPreservesResponseBytesAndOnlyRewritesModel(t *testing.T) {
	for _, tc := range []struct {
		protocol ir.Protocol
		path     string
	}{
		{ir.ProtocolOpenAIChat, gatewaytest.RouteChat},
		{ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses},
		{ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages},
	} {
		for _, baseSuffix := range []string{"", "/v1/", tc.path + "/"} {
			t.Run(string(tc.protocol)+baseSuffix, func(t *testing.T) {
				u := gatewaytest.New(t)
				const response = " {\"usage\":{\"input_tokens\":11,\"output_tokens\":7},\"content\":\"opaque-relay-provider-credential\"}\n"
				u.Script(tc.path, gatewaytest.Step{Body: response})
				recorder := &recording{}
				h := newTestRelay(t, u.URL()+baseSuffix, tc.protocol, recorder)
				const body = ` { "model" : "client-alias", "temperature":0, "unknown":{"model":"nested","number":9007199254740993}, "messages":[] } `
				r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer client-credential")
				r.Header.Set("Cookie", "session=client-secret")
				r.Header.Set("X-Client", "forwarded")
				r.Header.Set("X-Opencode-Session", "session-id")
				r.Header.Set(core.RequestIDHeader, "client-request-id")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusOK || w.Body.String() != response {
					t.Fatalf("response = %d %q", w.Code, w.Body.String())
				}
				requests := u.RequestsFor(tc.path)
				if len(requests) != 1 {
					t.Fatalf("upstream received %d requests", len(requests))
				}
				got := requests[0]
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(got.Body), &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["model"]) != `"upstream-model"` || string(fields["temperature"]) != "0" || string(fields["unknown"]) != `{"model":"nested","number":9007199254740993}` {
					t.Fatalf("request fields changed: %s", got.Body)
				}
				if got.Headers.Get("X-Client") != "forwarded" || got.Headers.Get("X-Static") != "configured" || got.Headers.Get("X-Opencode-Session") != "session-id" || got.Headers.Get("Cookie") != "" || got.Headers.Get(core.RequestIDHeader) != "client-request-id" {
					t.Fatalf("unsafe or missing upstream headers: %v", got.Headers)
				}
				credential := got.Headers.Get("Authorization")
				if tc.protocol == ir.ProtocolAnthropicMessages {
					credential = got.Headers.Get("X-Api-Key")
					if got.Headers.Get("Anthropic-Version") != "2023-06-01" {
						t.Fatal("missing Anthropic version")
					}
				} else if credential != "Bearer opaque-relay-provider-credential" {
					t.Fatalf("provider authorization = %q", credential)
				}
				if credential == "" || strings.Contains(credential, "client-credential") {
					t.Fatalf("provider credential not applied: %q", credential)
				}
				calls := recorder.completions()
				if len(calls) != 1 || calls[0].Err != nil || calls[0].Usage.InputTokens != 11 || calls[0].Usage.OutputTokens != 7 || calls[0].Request.ClientModel != "client-alias" || calls[0].Request.UpstreamModel != "upstream-model" || calls[0].Request.ResolvedProvider != "relay-provider" {
					t.Fatalf("completion = %+v", calls)
				}
			})
		}
	}
}

func TestRelayUpstreamStatusesAndCredentialSafeDiagnostics(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			u := gatewaytest.New(t)
			const safe = " {\"error\":{\"message\":\"provider detail\"},\"large\":9007199254740993}\n"
			u.Script(gatewaytest.RouteChat,
				gatewaytest.Step{Status: status, Body: safe, RetryAfter: "13"},
				gatewaytest.Step{Status: status, Body: `{"error":{"message":"echo \u006fpaque-relay-provider-credential","headers":{"Authorization":"Bearer unregistered-secret"}}}`},
			)
			recorder := &recording{}
			h := newTestRelay(t, u.URL(), ir.ProtocolOpenAIChat, recorder)
			for i := range 2 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`)))
				if w.Code != status {
					t.Fatalf("status = %d, want %d", w.Code, status)
				}
				if i == 0 && (w.Body.String() != safe || w.Header().Get("Retry-After") != "13") {
					t.Fatalf("transparent error = %q %v", w.Body.String(), w.Header())
				}
				if i == 1 && (strings.Contains(w.Body.String(), "opaque-relay-provider-credential") || strings.Contains(w.Body.String(), "unregistered-secret") || !strings.Contains(w.Body.String(), "[redacted]")) {
					t.Fatalf("unsafe diagnostic: %s", w.Body.String())
				}
			}
			calls := recorder.completions()
			if len(calls) != 2 || calls[0].Err == nil || calls[1].Err == nil {
				t.Fatalf("upstream failures not recorded exactly once: %+v", calls)
			}
		})
	}
}

func TestRelaySafeResponseHeaders(t *testing.T) {
	u := gatewaytest.New(t)
	u.Script(gatewaytest.RouteChat, gatewaytest.Step{Body: `{}`, Headers: map[string]string{
		"Connection": "X-Connection-Secret", "X-Connection-Secret": "drop", "Keep-Alive": "timeout=5",
		"Set-Cookie": "upstream-session=secret", "X-Request-Id": "untrusted-upstream-id",
		"X-Upstream-Diagnostic": "echo opaque-relay-provider-credential", "X-Safe": "keep",
	}})
	h := newTestRelay(t, u.URL(), ir.ProtocolOpenAIChat, &recording{})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`))
	r.Header.Set(core.RequestIDHeader, "canonical-id")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, name := range []string{"Connection", "X-Connection-Secret", "Keep-Alive", "Set-Cookie", "Content-Length", "Transfer-Encoding"} {
		if w.Header().Get(name) != "" {
			t.Fatalf("forwarded unsafe %s: %v", name, w.Header())
		}
	}
	if w.Header().Get(core.RequestIDHeader) != "canonical-id" || w.Header().Get("X-Safe") != "keep" || strings.Contains(w.Header().Get("X-Upstream-Diagnostic"), "opaque-relay-provider-credential") {
		t.Fatalf("response headers = %v", w.Header())
	}
}

func TestRelaySSEFlushesAndPreservesSuccessfulBytes(t *testing.T) {
	first := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"opaque-relay-provider-credential\"}\r\n\r\n"
	last := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":17,\"output_tokens\":9}}}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		if _, err := io.WriteString(w, first); err != nil {
			t.Errorf("write first SSE event: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if _, err := io.WriteString(w, last); err != nil {
			t.Errorf("write final SSE event: %v", err)
		}
	}))
	defer u.Close()
	recorder := &recording{}
	gateway := httptest.NewServer(newTestRelay(t, u.URL, ir.ProtocolOpenAIResponses, recorder))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+gatewaytest.RouteResponses, strings.NewReader(`{"model":"alias","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	prefix := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatal(err)
	}
	if string(prefix) != first {
		t.Fatalf("first event changed: %q", prefix)
	}
	once.Do(func() { close(release) })
	tail, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != last {
		t.Fatalf("remaining stream changed: %q", tail)
	}
	calls := recorder.wait(t, 1, 2*time.Second)
	if len(calls) != 1 || calls[0].Err != nil || !calls[0].Request.Stream ||
		calls[0].Request.FirstByte.IsZero() || calls[0].Request.FirstByte.Before(calls[0].Request.Start) ||
		calls[0].TTFB < 0 || calls[0].Usage.InputTokens != 17 || calls[0].Usage.OutputTokens != 9 {
		t.Fatalf("stream completion = %s", formatCompletions(calls))
	}
}

func TestRelayAnthropicCountTokensUsesFullMessagesEndpoint(t *testing.T) {
	u := gatewaytest.New(t)
	path := gatewaytest.RouteMessages + "/count_tokens"
	u.Script(path, gatewaytest.Step{Body: " {\"input_tokens\":23}\n"})
	h := newTestRelay(t, u.URL()+gatewaytest.RouteMessages, ir.ProtocolAnthropicMessages, &recording{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"model":"alias","messages":[]}`)))
	if w.Code != 200 || w.Body.String() != " {\"input_tokens\":23}\n" || len(u.RequestsFor(path)) != 1 {
		t.Fatalf("count_tokens response = %d %q", w.Code, w.Body.String())
	}
}

func TestRelayDisconnectCancelsUpstreamAndRecordsOnce(t *testing.T) {
	canceled := make(chan struct{})
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: {\"choices\":[]}\n\n"); err != nil {
			t.Errorf("write initial SSE event: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer u.Close()
	recorder := &recording{}
	gateway := httptest.NewServer(newTestRelay(t, u.URL, ir.ProtocolOpenAIChat, recorder))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+gatewaytest.RouteChat, strings.NewReader(`{"model":"alias","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	line := make([]byte, len("data: {\"choices\":[]}\n\n"))
	if _, err := io.ReadFull(resp.Body, line); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("upstream not canceled on client disconnect")
	}
	deadline := time.Now().Add(time.Second)
	for len(recorder.completions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	calls := recorder.completions()
	if len(calls) != 1 || !errors.Is(calls[0].Err, context.Canceled) {
		t.Fatalf("cancellation completion = %+v", calls)
	}
}

func TestRelayTransportErrorsUseInboundProtocolShape(t *testing.T) {
	for _, tc := range []struct {
		protocol ir.Protocol
		path     string
	}{
		{ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages},
		{ir.ProtocolOpenAIChat, gatewaytest.RouteChat},
		{ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			u := gatewaytest.New(t)
			// A disconnected upstream must not trigger retries.
			u.Script(tc.path, gatewaytest.Step{Disconnect: true})
			recorder := &recording{}
			h := newTestRelay(t, u.URL(), tc.protocol, recorder)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(`{"model":"alias"}`)))
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadGateway || body.Error.Message == "" || body.Error.Type == "" || tc.protocol == ir.ProtocolAnthropicMessages && body.Type != "error" {
				t.Fatalf("transport error = %d %s", w.Code, w.Body.String())
			}
			if len(u.RequestsFor(tc.path)) != 1 || len(recorder.completions()) != 1 || recorder.completions()[0].Err == nil {
				t.Fatal("transport failure retried or accounted incorrectly")
			}
		})
	}
}

func TestRelayBlocksCrossOriginRedirectBeforeCredentialDisclosure(t *testing.T) {
	var received atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Errorf("write redirect destination response: %v", err)
		}
	}))
	defer destination.Close()
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer u.Close()
	h := newTestRelay(t, u.URL, ir.ProtocolOpenAIChat, &recording{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`)))
	if w.Code != http.StatusBadGateway || received.Load() != 0 || bytes.Contains(w.Body.Bytes(), []byte("opaque-relay-provider-credential")) {
		t.Fatalf("redirect policy = %d %s; destination requests %d", w.Code, w.Body.String(), received.Load())
	}
}

func TestRelayStreamFinalUsageSurvivesLongOutput(t *testing.T) {
	for _, tc := range []struct {
		protocol ir.Protocol
		path     string
		start    string
		final    string
	}{
		{ir.ProtocolOpenAIChat, gatewaytest.RouteChat, "", "data: {\"usage\":{\"prompt_tokens\":\"31\",\"completion_tokens\":19,\"prompt_tokens_details\":{\"cached_tokens\":5},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n"},
		{ir.ProtocolOpenAIResponses, gatewaytest.RouteResponses, "", "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":31,\"output_tokens\":19,\"input_tokens_details\":{\"cached_tokens\":5},\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\n\n"},
		{ir.ProtocolAnthropicMessages, gatewaytest.RouteMessages, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":31,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":7}}}\n\n", "event: message_delta\ndata: {\"usage\":{\"output_tokens\":19}}\n\n"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			u := gatewaytest.New(t)
			output := "data: {\"delta\":\"" + strings.Repeat("x", 8192) + "\"}\n\n"
			var events []string
			if tc.start != "" {
				events = append(events, tc.start)
			}
			for range 40 {
				events = append(events, output)
			}
			events = append(events, tc.final)
			u.Script(tc.path, gatewaytest.Step{Events: events})
			recorder := &recording{}
			h := newTestRelay(t, u.URL(), tc.protocol, recorder)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(`{"model":"alias","stream":true}`)))
			want := strings.Join(events, "")
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatal("successful long stream was changed")
			}
			calls := recorder.completions()
			if len(calls) != 1 || calls[0].Err != nil || calls[0].Usage.InputTokens != 31 || calls[0].Usage.OutputTokens != 19 || calls[0].Usage.CacheReadTokens != 5 {
				t.Fatalf("final usage = %+v", calls)
			}
			if tc.protocol == ir.ProtocolAnthropicMessages {
				if calls[0].Usage.CacheWriteTokens != 7 {
					t.Fatal("lost start-event cache usage")
				}
			} else if calls[0].Usage.ReasoningTokens != 3 {
				t.Fatal("lost reasoning usage")
			}
		})
	}
}

func TestRelaySSEFiltersSplitDiagnosticWithoutChangingModelOutput(t *testing.T) {
	const success = "data: {\"choices\":[{\"delta\":{\"content\":\"opaque-relay-provider-credential\"}}]}\n\n"
	const diagnostic = "id: opaque-relay-provider-credential\r\nevent: error\r\ndata: {\"error\":{\"message\":\"echo \\u006fpaque-relay-provider-credential\",\"headers\":{\"x-api-key\":\"unregistered-stream-key\"}}}\r\n\r\n"
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, success); err != nil {
			t.Errorf("write successful SSE event: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		for i := range len(diagnostic) {
			if _, err := io.WriteString(w, diagnostic[i:i+1]); err != nil {
				t.Errorf("write diagnostic SSE byte: %v", err)
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer u.Close()
	recorder := &recording{}
	h := newTestRelay(t, u.URL, ir.ProtocolOpenAIChat, recorder)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias","stream":true}`)))
	if !strings.HasPrefix(w.Body.String(), success) {
		t.Fatal("redactor altered successful model content")
	}
	tail := strings.TrimPrefix(w.Body.String(), success)
	if strings.Contains(tail, "opaque-relay-provider-credential") || strings.Contains(tail, "unregistered-stream-key") || !strings.Contains(tail, "[redacted]") {
		t.Fatalf("unsafe error frame: %s", tail)
	}
	if len(recorder.completions()) != 1 || recorder.completions()[0].Err == nil {
		t.Fatal("stream error was not counted exactly once")
	}
}

func TestRelayFiltersErrorBodyMislabeledAsSSE(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		secret bool
	}{
		{"escaped", `{"error":{"message":"echo \u006fpaque-relay-provider-credential"}}`, true},
		{"blank-lines", " \n{\n\n\"error\": {\n\"message\": \"echo \\u006fpaque-relay-provider-credential\"\n}\n}\n", true},
		{"safe", " \n{\n\n\"error\": {\"message\": \"provider detail\"},\"large\":9007199254740993\n}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := gatewaytest.New(t)
			u.Script(gatewaytest.RouteChat, gatewaytest.Step{Status: http.StatusUnauthorized,
				Headers: map[string]string{"Content-Type": "text/event-stream"}, Body: tc.body,
			})
			recorder := &recording{}
			h := newTestRelay(t, u.URL(), ir.ProtocolOpenAIChat, recorder)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`)))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("mislabeled diagnostic status = %d", w.Code)
			}
			if tc.secret {
				var diagnostic struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &diagnostic); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(diagnostic.Error.Message, "opaque-relay-provider-credential") || !strings.Contains(diagnostic.Error.Message, "[redacted]") {
					t.Fatalf("unsafe mislabeled diagnostic = %s", w.Body.String())
				}
			} else if w.Body.String() != tc.body {
				t.Fatalf("safe diagnostic bytes changed: %q", w.Body.String())
			}
			if len(recorder.completions()) != 1 || recorder.completions()[0].Err == nil {
				t.Fatal("mislabeled error was not counted once")
			}
		})
	}
}

func TestRelayConnectionHeaderCannotEnableTransparentGzip(t *testing.T) {
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(zip, " {\"content\":\"opaque-relay-provider-credential\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		if _, err := w.Write(compressed.Bytes()); err != nil {
			t.Errorf("write compressed response: %v", err)
		}
	}))
	defer u.Close()
	h := newTestRelay(t, u.URL, ir.ProtocolOpenAIChat, &recording{})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`))
	r.Header.Set("Connection", "Accept-Encoding")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(w.Body.Bytes(), compressed.Bytes()) {
		t.Fatalf("gzip response was transparently decoded: %d %v %q", w.Code, w.Header(), w.Body.Bytes())
	}
}

func TestRelayConfiguredConnectTimeoutBoundsProxyNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect time.Duration
		delay   time.Duration
		status  int
	}{
		{"above-ten-seconds", 12 * time.Second, 10200 * time.Millisecond, http.StatusOK},
		{"provider-shorter-than-options", 40 * time.Millisecond, time.Second, http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Error("proxy expected CONNECT")
					return
				}
				conn, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() {
					if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
						t.Errorf("close proxy connection: %v", closeErr)
					}
				}()
				timer := time.NewTimer(tc.delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-done:
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				request, err := http.ReadRequest(buffered.Reader)
				if err != nil {
					return
				}
				defer func() {
					if closeErr := request.Body.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
						t.Errorf("close proxy request body: %v", closeErr)
					}
				}()
				if _, err := io.Copy(io.Discard, request.Body); err != nil {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"); err != nil && tc.status == http.StatusOK && !errors.Is(err, net.ErrClosed) {
					t.Errorf("write proxy response: %v", err)
				}
			}))
			defer proxy.Close()
			defer close(done)
			factory := upstream.NewFactory()
			defer factory.CloseIdleConnections()
			policy := provider.URLPolicy{}
			p := &provider.Provider{ID: "relay-connect", Name: "Relay connect", BaseURL: "http://127.0.0.1:1", Protocol: ir.ProtocolOpenAIChat,
				Timeouts: provider.Timeouts{Connect: tc.connect}, ProxyURL: proxy.URL}
			h, err := New(p, "upstream-model", &Options{Factory: factory, Policy: &policy, Recorder: &recording{}, Timeouts: provider.Timeouts{Connect: 15 * time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 14*time.Second)
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, gatewaytest.RouteChat, strings.NewReader(`{"model":"alias"}`))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("proxy negotiation response = %d %s", w.Code, w.Body.String())
			}
			if tc.status == http.StatusOK && w.Body.String() != "{}" {
				t.Fatalf("proxy response changed: %q", w.Body.String())
			}
		})
	}
}

func TestRelayGuardedDialerHonorsCallerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer proxy.Close()
	defer close(release)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := (provider.URLPolicy{}).Dialer(proxyURL, nil, nil)(ctx, "tcp", "127.0.0.1:1")
		if conn != nil {
			if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = closeErr
			}
		}
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("guarded dial ended before proxy negotiation: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("guarded dial did not start proxy negotiation")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("guarded dial cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not interrupt proxy negotiation")
	}
}
