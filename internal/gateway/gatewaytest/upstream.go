// Package gatewaytest provides fake upstreams for the gateway's tests:
// httptest servers that speak the three provider protocols from scripted
// step queues, and record every request they receive.
//
// Existing tests built ad-hoc roundTripFuncs and inline fixtures
// (cmd/serve/passthrough_test.go, responses_anthropic_test.go); adapters,
// the dispatcher and the e2e matrix all need the same deterministic
// upstreams, so the shapes live here once.
//
// Script a route with Script(path, steps...). Each Step serves one
// response: canned JSON, an SSE event list, an HTTP error with a body and
// Retry-After, a stall that outlasts the client's timeout, or an abrupt
// disconnect. Steps are consumed in order per route; a request that finds
// its queue empty fails loudly with a 500 JSON error rather than a
// mysterious hang, so a miscounted test says which route ran dry.
//
// Every request — method, path, headers, body — is captured for
// assertions, and Authorizations exposes the received auth headers so
// tests can assert key injection. New closes its server via t.Cleanup.
package gatewaytest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// noStepBody is the JSON a request receives when its route's step queue is
// empty. The message is the contract: tests assert "no scripted step" so a
// miscounted queue says what ran dry instead of hanging on a read.
const noStepBody = `{"error":{"message":"gatewaytest: no scripted step for this request","type":"gatewaytest_error"}}`

// Routes the emulators serve.
const (
	RouteChat      = "/v1/chat/completions"
	RouteMessages  = "/v1/messages"
	RouteResponses = "/v1/responses"
	RouteModels    = "/v1/models"
)

// Step is one scripted response to one request on a route.
//
// Set Events to serve an SSE stream: each string is written verbatim and
// flushed, one EventDelay between events. An event string needs its blank
// line appended unless it already ends with "\n\n" — SplitEvents produces
// correctly terminated strings from a raw stream.
//
// Set Body to serve a plain JSON response at Status (200 when zero).
// Headers sets response headers; Content-Type defaults to
// application/json, or text/event-stream when Events is set. RetryAfter
// becomes the Retry-After header. Stall delays the first byte, so a Step
// whose Stall exceeds the client's timeout reads as a timeout. Disconnect
// hijacks and closes the connection without any response.
type Step struct {
	Status     int
	Headers    map[string]string
	Body       string
	Events     []string
	EventDelay time.Duration
	RetryAfter string
	Stall      time.Duration
	Disconnect bool
}

// RecordedRequest is one request the fake upstream received.
type RecordedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    string
}

// Upstream is an httptest server emulating the provider endpoints.
type Upstream struct {
	srv *httptest.Server

	mu    sync.Mutex
	steps map[string][]Step
	log   []RecordedRequest
}

// New starts a fake upstream whose lifetime is tied to t.
func New(t *testing.T) *Upstream {
	t.Helper()
	u := &Upstream{steps: map[string][]Step{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(func() { u.srv.Close() })
	return u
}

// URL is the upstream's base URL, e.g. "http://127.0.0.1:PORT".
func (u *Upstream) URL() string { return u.srv.URL }

// Script appends steps to path's response queue, consumed in order.
func (u *Upstream) Script(path string, steps ...Step) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.steps[path] = append(u.steps[path], steps...)
}

// Requests returns every request received so far, in arrival order.
func (u *Upstream) Requests() []RecordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]RecordedRequest, len(u.log))
	copy(out, u.log)
	return out
}

// RequestsFor returns the requests received on path, in arrival order.
func (u *Upstream) RequestsFor(path string) []RecordedRequest {
	var out []RecordedRequest
	for _, r := range u.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// Authorizations returns the Authorization headers received, in arrival
// order, for asserting that the gateway injected the expected key.
func (u *Upstream) Authorizations() []string {
	var out []string
	for _, r := range u.Requests() {
		if v := r.Headers.Get("Authorization"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (u *Upstream) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "gatewaytest: reading request: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.record(r, body)

	step, ok := u.next(r.URL.Path)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := fmt.Fprint(w, noStepBody); err != nil {
			return
		}
		return
	}

	if step.Disconnect {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				// The error is uninteresting — dropping the client is the
				// step's whole purpose — but the close is still checked so
				// a failing one is not silently swallowed.
				if err := conn.Close(); err != nil {
					return
				}
				return
			}
		}
		// No hijacker available: the closest available abrupt failure.
		http.Error(w, "gatewaytest: disconnect requested", http.StatusBadGateway)
		return
	}

	if step.Stall > 0 {
		time.Sleep(step.Stall)
	}

	h := w.Header()
	for k, v := range step.Headers {
		h.Set(k, v)
	}
	if step.RetryAfter != "" {
		h.Set("Retry-After", step.RetryAfter)
	}
	status := step.Status
	if status == 0 {
		status = http.StatusOK
	}

	if len(step.Events) > 0 {
		h.Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		flusher, _ := w.(http.Flusher)
		for _, ev := range step.Events {
			if _, err := io.WriteString(w, ev); err != nil {
				return
			}
			if !strings.HasSuffix(ev, "\n\n") {
				if _, err := io.WriteString(w, "\n\n"); err != nil {
					return
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
			if step.EventDelay > 0 {
				time.Sleep(step.EventDelay)
			}
		}
		return
	}

	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if _, err := io.WriteString(w, step.Body); err != nil {
		return
	}
}

func (u *Upstream) record(r *http.Request, body []byte) {
	clone := make([]byte, len(body))
	copy(clone, body)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.log = append(u.log, RecordedRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: r.Header.Clone(),
		Body:    string(clone),
	})
}

func (u *Upstream) next(path string) (Step, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.steps[path]) == 0 {
		return Step{}, false
	}
	s := u.steps[path][0]
	u.steps[path] = u.steps[path][1:]
	return s, true
}

// Load reads a fixture from the calling package's testdata directory.
// It fails the test when the fixture is missing, so a typo named in an
// assertion says which file it wanted.
func Load(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("gatewaytest: load fixture %q: %v", name, err)
	}
	return string(b)
}

// SplitEvents chops a raw SSE stream into the per-event strings Step.Events
// wants, each with its terminating blank line. The trailing fragment after
// the last blank line is kept when non-empty, so streams without a final
// terminator still yield their last event. CRLF and lone-CR line endings
// are normalized — the SSE grammar allows all three, and a fixture checked
// out or pasted on Windows arrives as CRLF.
func SplitEvents(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	parts := strings.Split(raw, "\n\n")
	var out []string
	last := len(parts) - 1
	for _, p := range parts {
		if p == "" {
			continue
		}
		if p == parts[last] && !strings.HasSuffix(raw, "\n\n") {
			out = append(out, p)
		} else {
			out = append(out, p+"\n\n")
		}
	}
	return out
}
