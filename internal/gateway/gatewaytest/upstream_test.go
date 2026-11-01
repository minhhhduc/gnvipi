package gatewaytest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// reply is one response from the fake upstream, body fully read and
// closed, so no *http.Response escapes for the caller to leak.
type reply struct {
	status int
	header http.Header
	body   string
}

// post sends one request to the fake upstream and returns the response
// with its fully read body. The call is bounded by a context timeout so
// stalled steps can be exercised without hanging the suite.
func post(t *testing.T, baseURL, path, body string) reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close %s body: %v", path, err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func TestNonStreamChat(t *testing.T) {
	u := New(t)
	u.Script(RouteChat, Step{Body: Load(t, "sample-chat.json")})

	r := post(t, u.URL(), RouteChat, `{"model":"gpt-4o"}`)
	if r.status != http.StatusOK {
		t.Errorf("status = %d, want 200", r.status)
	}
	if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if !strings.Contains(r.body, "Hello from the fake upstream") {
		t.Errorf("body = %s, missing the canned content", r.body)
	}

	reqs := u.RequestsFor(RouteChat)
	if len(reqs) != 1 {
		t.Fatalf("Recorded %d requests for %s, want 1", len(reqs), RouteChat)
	}
	if reqs[0].Method != http.MethodPost {
		t.Errorf("method = %s, want POST", reqs[0].Method)
	}
	if !strings.Contains(reqs[0].Body, "gpt-4o") {
		t.Errorf("captured body = %s, missing the request model", reqs[0].Body)
	}
}

func TestRecorderSeesAuthHeader(t *testing.T) {
	u := New(t)
	u.Script(RouteMessages, Step{Body: `{}`})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+RouteMessages, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}

	auths := u.Authorizations()
	if len(auths) != 1 || auths[0] != "Bearer sk-test-key" {
		t.Fatalf("Authorizations() = %v, want [Bearer sk-test-key]", auths)
	}
	if got := u.Requests()[0].Headers.Get("X-Api-Key"); got != "" {
		t.Errorf("unexpected X-Api-Key %q on a raw request", got)
	}
}

func TestAnthropicStreamFromFixture(t *testing.T) {
	u := New(t)
	u.Script(RouteMessages, Step{Events: SplitEvents(Load(t, "sample-events.sse")), EventDelay: time.Millisecond})

	r := post(t, u.URL(), RouteMessages, `{}`)
	if r.status != http.StatusOK {
		t.Errorf("status = %d, want 200", r.status)
	}
	if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// Each written event is flushed; the read returns the whole stream.
	if !strings.Contains(r.body, "message_start") || !strings.Contains(r.body, "text_delta") || !strings.Contains(r.body, "message_stop") {
		t.Errorf("stream =\n%s\nmissing expected events", r.body)
	}
	if strings.Count(r.body, "\n\n") != 3 {
		t.Errorf("stream has %d dispatched events, want 3", strings.Count(r.body, "\n\n"))
	}
}

func TestResponsesStreamAndModels(t *testing.T) {
	u := New(t)
	u.Script(RouteResponses, Step{
		Events: []string{
			"event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		},
	})
	u.Script(RouteModels, Step{Body: `{"data":[{"id":"gpt-4o"}]}`})

	if r := post(t, u.URL(), RouteResponses, `{"model":"gpt-4o"}`); !strings.Contains(r.body, "response.completed") {
		t.Errorf("responses stream = %s, missing the terminator", r.body)
	}
	if r := post(t, u.URL(), RouteModels, ""); !strings.Contains(r.body, "gpt-4o") {
		t.Errorf("models body = %s", r.body)
	}
}

func TestHTTPErrorWithRetryAfter(t *testing.T) {
	u := New(t)
	u.Script(RouteChat, Step{
		Status:     429,
		RetryAfter: "30",
		Body:       `{"error":{"message":"quota exceeded","type":"rate_limit_error"}}`,
	})
	r := post(t, u.URL(), RouteChat, `{}`)
	if r.status != 429 {
		t.Errorf("status = %d, want 429", r.status)
	}
	if got := r.header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if !strings.Contains(r.body, "rate_limit_error") {
		t.Errorf("body = %s, missing the provider error", r.body)
	}
}

func TestMidStreamErrorEvent(t *testing.T) {
	u := New(t)
	u.Script(RouteResponses, Step{Events: []string{
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"He\"}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"provider exploded\"}\n\n",
	}})
	if r := post(t, u.URL(), RouteResponses, `{}`); !strings.Contains(r.body, "provider exploded") {
		t.Errorf("stream = %s, missing the mid-stream error", r.body)
	}
}

func TestUnscriptedRequestFailsLoudly(t *testing.T) {
	u := New(t)
	r := post(t, u.URL(), RouteChat, `{}`)
	if r.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an empty step queue", r.status)
	}
	if !strings.Contains(r.body, "no scripted step") {
		t.Errorf("body = %s, wants a named failure", r.body)
	}
}

func TestStallReadsAsClientTimeout(t *testing.T) {
	u := New(t)
	u.Script(RouteChat, Step{Stall: 400 * time.Millisecond, Body: `{}`})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL()+RouteChat, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close body: %v", cerr)
		}
		t.Fatal("stalled request with a 50 ms client timeout unexpectedly succeeded")
	}
	// The request itself was still recorded: the gateway's side of a
	// timeout is a request that left and never came back.
	if len(u.RequestsFor(RouteChat)) != 1 {
		t.Errorf("Recorded %d requests, want 1 (the timed-out one)", len(u.RequestsFor(RouteChat)))
	}
}

func TestDisconnect(t *testing.T) {
	u := New(t)
	u.Script(RouteChat, Step{Disconnect: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL()+RouteChat, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close body: %v", cerr)
		}
		t.Error("disconnected request unexpectedly succeeded")
	}
}

func TestSequentialStepsConsumedInOrder(t *testing.T) {
	u := New(t)
	u.Script(RouteChat,
		Step{Status: 500, Body: `{"error":{"message":"try again"}}`},
		Step{Body: `{"ok":true}`},
	)
	if r := post(t, u.URL(), RouteChat, `{}`); r.status != 500 {
		t.Errorf("first step status = %d, want 500", r.status)
	}
	if r := post(t, u.URL(), RouteChat, `{}`); r.status != 200 || r.body != `{"ok":true}` {
		t.Errorf("second step = %d %s, want 200 {\"ok\":true}", r.status, r.body)
	}
	// Third request drains the queue: the failure is named, not silent.
	if r := post(t, u.URL(), RouteChat, `{}`); r.status != 500 || !strings.Contains(r.body, "no scripted step") {
		t.Errorf("drained queue = %d %s, want a named failure", r.status, r.body)
	}
}

func TestCustomResponseHeaders(t *testing.T) {
	u := New(t)
	u.Script(RouteMessages, Step{Headers: map[string]string{"X-Provider": "acme"}, Body: `{}`})
	r := post(t, u.URL(), RouteMessages, `{}`)
	if got := r.header.Get("X-Provider"); got != "acme" {
		t.Errorf("X-Provider = %q, want acme", got)
	}
}

func TestSplitEvents(t *testing.T) {
	t.Run("terminated stream", func(t *testing.T) {
		got := SplitEvents("event: a\ndata: 1\n\nevent: b\ndata: 2\n\n")
		want := []string{"event: a\ndata: 1\n\n", "event: b\ndata: 2\n\n"}
		if len(got) != len(want) {
			t.Fatalf("SplitEvents() = %#v, want %#v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("event %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	})
	t.Run("unterminated tail keeps its event", func(t *testing.T) {
		got := SplitEvents("event: a\ndata: 1\n\nevent: b\ndata: 2")
		if len(got) != 2 || got[1] != "event: b\ndata: 2" {
			t.Errorf("SplitEvents() = %#v, want the unterminated tail as its own event", got)
		}
	})
	t.Run("CRLF stream", func(t *testing.T) {
		got := SplitEvents("event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n")
		want := []string{"event: a\ndata: 1\n\n", "event: b\ndata: 2\n\n"}
		if len(got) != len(want) {
			t.Fatalf("SplitEvents() = %#v, want %#v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("event %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	})
	t.Run("duplicate events before an unterminated tail", func(t *testing.T) {
		// The tail check is by index, not value: an earlier event whose
		// text equals the unterminated tail must still be terminated.
		got := SplitEvents("event: ping\ndata: 1\n\nevent: ping\ndata: 2")
		want := []string{"event: ping\ndata: 1\n\n", "event: ping\ndata: 2"}
		if len(got) != len(want) {
			t.Fatalf("SplitEvents() = %#v, want %#v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("event %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	})
	t.Run("empty stream", func(t *testing.T) {
		if got := SplitEvents(""); len(got) != 0 {
			t.Errorf("SplitEvents(\"\") = %#v, want no events", got)
		}
	})
}

func TestLoadMissingFixtureFailsTest(t *testing.T) {
	// Load must be fatal to the caller. Feed it a TB that records the
	// fatal instead of exiting, so the harness's own suite stays green.
	ft := &fatalRecorder{T: t}
	Load(ft, "no-such-fixture.json")
	if !ft.failed {
		t.Error("Load on a missing fixture returned instead of failing")
	}
	if !strings.Contains(ft.message, "no-such-fixture.json") {
		t.Errorf("fatal message %q does not name the fixture", ft.message)
	}
}

// fatalRecorder embeds a real *testing.T so it satisfies testing.TB, and
// overrides Fatalf to record the call instead of ending the test.
type fatalRecorder struct {
	*testing.T
	failed  bool
	message string
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.failed = true
	f.message = fmt.Sprintf(format, args...)
}
