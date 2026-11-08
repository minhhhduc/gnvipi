package reliability_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/reliability"
)

func fastTestPolicy() reliability.Policy {
	p := reliability.DefaultPolicy()
	p.InitialBackoff = time.Millisecond
	p.MaxBackoff = 5 * time.Millisecond
	p.MaxRetryAfter = 10 * time.Millisecond
	p.DisableJitter = true
	return p
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp != nil && resp.Body != nil {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		// Retryable statuses
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},

		// Non-retryable 2xx / 3xx
		{http.StatusOK, false},
		{http.StatusCreated, false},
		{http.StatusMovedPermanently, false},

		// Non-retryable 4xx
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusMethodNotAllowed, false},
		{http.StatusConflict, false},
		{http.StatusRequestEntityTooLarge, false},
		{http.StatusUnprocessableEntity, false},

		// Non-retryable 5xx
		{http.StatusInternalServerError, false},
		{http.StatusNotImplemented, false},
		{http.StatusHTTPVersionNotSupported, false},
	}

	for _, tc := range tests {
		got := reliability.IsRetryableStatus(tc.status)
		if got != tc.want {
			t.Errorf("IsRetryableStatus(%d) = %v; want %v", tc.status, got, tc.want)
		}
	}
}

func TestIsRetryableNetworkError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "connection refused syscall",
			err:  &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
			want: true,
		},
		{
			name: "connection refused WSA 10061",
			err:  &os.SyscallError{Syscall: "connectex", Err: syscall.Errno(10061)},
			want: true,
		},
		{
			name: "connection reset syscall",
			err:  &net.OpError{Op: "read", Err: syscall.ECONNRESET},
			want: true,
		},
		{
			name: "connection reset WSA 10054",
			err:  &net.OpError{Op: "wsarecv", Err: syscall.Errno(10054)},
			want: true,
		},
		{
			name: "connection reset error string",
			err:  errors.New("read tcp: connection reset by peer"),
			want: true,
		},
		{
			name: "forcibly closed error string",
			err:  errors.New("wsarecv: An existing connection was forcibly closed by the remote host"),
			want: true,
		},
		{
			name: "broken pipe error string",
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "abrupt EOF before response",
			err:  io.EOF,
			want: true,
		},
		{
			name: "unexpected EOF before response",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "DNS temporary error",
			err:  &net.DNSError{Err: "server failure", IsTemporary: true},
			want: true,
		},
		{
			name: "DNS timeout error",
			err:  &net.DNSError{Err: "i/o timeout", IsTimeout: true},
			want: true,
		},
		{
			name: "DNS SERVFAIL message",
			err:  &net.DNSError{Err: "SERVFAIL on query"},
			want: true,
		},
		{
			name: "DNS not found (NXDOMAIN)",
			err:  &net.DNSError{Err: "no such host", IsNotFound: true},
			want: false,
		},
		{
			name: "context canceled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "wrapped context canceled",
			err:  &url.Error{Op: "Post", URL: "http://example.com", Err: context.Canceled},
			want: false,
		},
		{
			name: "TLS certificate invalid",
			err:  &x509.CertificateInvalidError{},
			want: false,
		},
		{
			name: "TLS unknown authority",
			err:  x509.UnknownAuthorityError{},
			want: false,
		},
		{
			name: "TLS hostname mismatch",
			err:  x509.HostnameError{Certificate: &x509.Certificate{}, Host: "bad.com"},
			want: false,
		},
		{
			name: "arbitrary application error",
			err:  errors.New("something went wrong"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reliability.IsRetryableNetworkError(tc.err)
			if got != tc.want {
				t.Errorf("IsRetryableNetworkError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name:    "integer seconds",
			raw:     "5",
			wantOK:  true,
			wantMin: 5 * time.Second,
			wantMax: 5 * time.Second,
		},
		{
			name:    "decimal seconds",
			raw:     "2.5",
			wantOK:  true,
			wantMin: 2500 * time.Millisecond,
			wantMax: 2500 * time.Millisecond,
		},
		{
			name:    "zero seconds",
			raw:     "0",
			wantOK:  true,
			wantMin: 0,
			wantMax: 0,
		},
		{
			name:    "whitespace trimmed",
			raw:     "  10  ",
			wantOK:  true,
			wantMin: 10 * time.Second,
			wantMax: 10 * time.Second,
		},
		{
			name:   "empty string",
			raw:    "",
			wantOK: false,
		},
		{
			name:   "invalid text",
			raw:    "not-a-number-or-date",
			wantOK: false,
		},
		{
			name:   "negative seconds",
			raw:    "-5",
			wantOK: false,
		},
		{
			name:    "RFC1123 date in future",
			raw:     time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat),
			wantOK:  true,
			wantMin: 8 * time.Second,
			wantMax: 12 * time.Second,
		},
		{
			name:    "RFC1123 date in past",
			raw:     time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat),
			wantOK:  true,
			wantMin: 0,
			wantMax: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reliability.ParseRetryAfter(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ParseRetryAfter(%q) ok = %v; want %v", tc.raw, ok, tc.wantOK)
			}
			if ok {
				if got < tc.wantMin || got > tc.wantMax {
					t.Errorf("ParseRetryAfter(%q) = %v; want between %v and %v", tc.raw, got, tc.wantMin, tc.wantMax)
				}
			}
		})
	}
}

func TestComputeDelay(t *testing.T) {
	p := reliability.Policy{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		MaxRetryAfter:  5 * time.Second,
		BackoffFactor:  2.0,
		DisableJitter:  true,
	}

	// Exponential backoff check
	d0 := p.ComputeDelay(0, nil)
	if d0 != 100*time.Millisecond {
		t.Errorf("attempt 0 backoff = %v; want 100ms", d0)
	}

	d1 := p.ComputeDelay(1, nil)
	if d1 != 200*time.Millisecond {
		t.Errorf("attempt 1 backoff = %v; want 200ms", d1)
	}

	d2 := p.ComputeDelay(2, nil)
	if d2 != 400*time.Millisecond {
		t.Errorf("attempt 2 backoff = %v; want 400ms", d2)
	}

	// Capped at MaxBackoff
	d10 := p.ComputeDelay(10, nil)
	if d10 != 1*time.Second {
		t.Errorf("attempt 10 backoff = %v; want capped at 1s", d10)
	}

	// 429 with Retry-After header
	resp429 := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"3"}},
	}
	d429 := p.ComputeDelay(0, resp429)
	if d429 != 3*time.Second {
		t.Errorf("429 Retry-After 3s = %v; want 3s", d429)
	}

	// 503 with Retry-After header <= MaxRetryAfter
	resp503 := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Retry-After": []string{"2"}},
	}
	d503 := p.ComputeDelay(0, resp503)
	if d503 != 2*time.Second {
		t.Errorf("503 Retry-After 2s = %v; want 2s", d503)
	}

	// 429 with Retry-After exceeding MaxRetryAfter (capped at 5s)
	resp429Large := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"60"}},
	}
	d429Capped := p.ComputeDelay(0, resp429Large)
	if d429Capped != 5*time.Second {
		t.Errorf("429 Retry-After 60s = %v; want capped at 5s", d429Capped)
	}

	// Jitter enabled: should be >= base and <= MaxBackoff
	pJitter := p
	pJitter.DisableJitter = false
	pJitter.JitterFraction = 0.5
	for i := 0; i < 20; i++ {
		dj := pJitter.ComputeDelay(1, nil)
		if dj < 200*time.Millisecond || dj > 300*time.Millisecond {
			t.Errorf("jittered delay = %v; want between 200ms and 300ms", dj)
		}
	}
}

func TestNeverRetryAfterStreamStarted(t *testing.T) {
	p := fastTestPolicy()
	attempts := 0

	// 1. streamStarted explicitly returned as true
	resp, err := p.Execute(context.Background(), func(attempt int) (*http.Response, bool, error) {
		attempts++
		streamStarted := true
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("bad gateway")),
		}, streamStarted, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer closeBody(t, resp)

	if attempts != 1 {
		t.Errorf("attempts = %d; want 1 because streaming had started", attempts)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}

	// 2. core.RequestContext with FirstByte marked
	attempts = 0
	rc := core.NewRequestContext("req-1")
	rc.MarkFirstByte()
	ctx := core.WithRequestContext(context.Background(), rc)

	resp2, err2 := p.Execute(ctx, func(attempt int) (*http.Response, bool, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("service unavailable")),
		}, false, nil
	})

	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	defer closeBody(t, resp2)

	if attempts != 1 {
		t.Errorf("attempts = %d; want 1 because FirstByte was marked", attempts)
	}
}

func TestReplayableRequestBody(t *testing.T) {
	var receivedBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		receivedBodies = append(receivedBodies, string(bodyBytes))
		if len(receivedBodies) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			if _, writeErr := w.Write([]byte(`{"error":"retry me"}`)); writeErr != nil {
				t.Errorf("write error: %v", writeErr)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"result":"success"}`)); writeErr != nil {
			t.Errorf("write error: %v", writeErr)
		}
	}))
	defer srv.Close()

	payload := `{"messages":[{"role":"user","content":"hello"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	p := fastTestPolicy()
	resp, err := p.Do(srv.Client(), req)
	if err != nil {
		t.Fatalf("policy.Do failed: %v", err)
	}
	defer closeBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resp status = %d; want 200", resp.StatusCode)
	}
	if len(receivedBodies) != 2 {
		t.Fatalf("received %d requests; want 2", len(receivedBodies))
	}
	if receivedBodies[0] != payload || receivedBodies[1] != payload {
		t.Errorf("bodies differ:\nattempt 1: %q\nattempt 2: %q\nwant: %q", receivedBodies[0], receivedBodies[1], payload)
	}
}

func TestHarnessRetryableStatusesWithFakeUpstream(t *testing.T) {
	retryableCases := []struct {
		name        string
		firstStep   gatewaytest.Step
		wantRetried bool
	}{
		{
			name:        "502 Bad Gateway retries to 200 OK",
			firstStep:   gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502"}`},
			wantRetried: true,
		},
		{
			name:        "503 Service Unavailable retries to 200 OK",
			firstStep:   gatewaytest.Step{Status: http.StatusServiceUnavailable, Body: `{"error":"503"}`},
			wantRetried: true,
		},
		{
			name:        "504 Gateway Timeout retries to 200 OK",
			firstStep:   gatewaytest.Step{Status: http.StatusGatewayTimeout, Body: `{"error":"504"}`},
			wantRetried: true,
		},
		{
			name:        "429 Too Many Requests with Retry-After seconds",
			firstStep:   gatewaytest.Step{Status: http.StatusTooManyRequests, RetryAfter: "0", Body: `{"error":"429"}`},
			wantRetried: true,
		},
		{
			name:        "429 Too Many Requests with RFC1123 date",
			firstStep:   gatewaytest.Step{Status: http.StatusTooManyRequests, RetryAfter: time.Now().Add(time.Millisecond).UTC().Format(http.TimeFormat), Body: `{"error":"429"}`},
			wantRetried: true,
		},
		{
			name:        "408 Request Timeout retries to 200 OK",
			firstStep:   gatewaytest.Step{Status: http.StatusRequestTimeout, Body: `{"error":"408"}`},
			wantRetried: true,
		},
		{
			name:        "Abrupt Disconnect retries to 200 OK",
			firstStep:   gatewaytest.Step{Disconnect: true},
			wantRetried: true,
		},
	}

	for _, tc := range retryableCases {
		t.Run(tc.name, func(t *testing.T) {
			u := gatewaytest.New(t)
			u.Script(gatewaytest.RouteChat,
				tc.firstStep,
				gatewaytest.Step{Status: http.StatusOK, Body: `{"choices":[{"message":{"content":"ok"}}]}`},
			)

			p := fastTestPolicy()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{"model":"gpt-4"}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := p.Do(http.DefaultClient, req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer closeBody(t, resp)

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d; want 200 OK after retry", resp.StatusCode)
			}

			// Verify fake upstream received 2 requests
			reqs := u.Requests()
			if len(reqs) != 2 {
				t.Errorf("recorded %d requests; want 2", len(reqs))
			}
		})
	}
}

func TestHarnessNonRetryableStatusesWithFakeUpstream(t *testing.T) {
	nonRetryableCases := []struct {
		name       string
		firstStep  gatewaytest.Step
		wantStatus int
	}{
		{
			name:       "400 Bad Request does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusBadRequest, Body: `{"error":"bad request"}`},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "401 Unauthorized does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusUnauthorized, Body: `{"error":"unauthorized"}`},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "403 Forbidden does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusForbidden, Body: `{"error":"forbidden"}`},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "404 Not Found does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusNotFound, Body: `{"error":"not found"}`},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "422 Unprocessable does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusUnprocessableEntity, Body: `{"error":"unprocessable"}`},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "500 Internal Server Error does not retry",
			firstStep:  gatewaytest.Step{Status: http.StatusInternalServerError, Body: `{"error":"internal server error"}`},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range nonRetryableCases {
		t.Run(tc.name, func(t *testing.T) {
			u := gatewaytest.New(t)
			u.Script(gatewaytest.RouteChat,
				tc.firstStep,
				gatewaytest.Step{Status: http.StatusOK, Body: `{"should":"not be called"}`},
			)

			p := fastTestPolicy()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := p.Do(http.DefaultClient, req)
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}
			defer closeBody(t, resp)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d; want %d", resp.StatusCode, tc.wantStatus)
			}

			// Verify only 1 request was sent to the upstream
			reqs := u.Requests()
			if len(reqs) != 1 {
				t.Errorf("recorded %d requests; want exactly 1 (no retries)", len(reqs))
			}
		})
	}
}

func TestRetryAfterExceedingMaxDelayNotRetried(t *testing.T) {
	t.Run("429 with Retry-After > MaxRetryAfter returns immediately", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusTooManyRequests, RetryAfter: "10", Body: `{"error":"rate limited"}`},
			gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
		)

		p := fastTestPolicy()
		p.MaxRetryAfter = 2 * time.Second

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("status = %d; want 429 returned immediately", resp.StatusCode)
		}
		if len(u.Requests()) != 1 {
			t.Errorf("requests = %d; want 1 (must not retry when Retry-After > MaxRetryAfter)", len(u.Requests()))
		}
	})

	t.Run("503 with Retry-After > MaxRetryAfter returns immediately", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusServiceUnavailable, RetryAfter: "10", Body: `{"error":"service unavailable"}`},
			gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
		)

		p := fastTestPolicy()
		p.MaxRetryAfter = 2 * time.Second

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d; want 503 returned immediately", resp.StatusCode)
		}
		if len(u.Requests()) != 1 {
			t.Errorf("requests = %d; want 1 (must not retry when Retry-After > MaxRetryAfter)", len(u.Requests()))
		}
	})

	t.Run("503 with Retry-After <= MaxRetryAfter is honored for delay", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusServiceUnavailable, RetryAfter: "1", Body: `{"error":"service unavailable"}`},
			gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
		)

		var capturedDelay time.Duration
		p := fastTestPolicy()
		p.MaxRetryAfter = 5 * time.Second
		p.Sleep = func(ctx context.Context, d time.Duration) error {
			return nil
		}
		p.OnRetry = func(ctx context.Context, attempt int, delay time.Duration, resp *http.Response, err error) {
			capturedDelay = delay
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d; want 200 after retry", resp.StatusCode)
		}
		if len(u.Requests()) != 2 {
			t.Errorf("requests = %d; want 2 (retried)", len(u.Requests()))
		}
		if capturedDelay != 1*time.Second {
			t.Errorf("captured delay = %v; want 1s", capturedDelay)
		}
	})
}

func TestPermanentRateLimitNotRetried(t *testing.T) {
	quotaErrorBody := `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`

	u := gatewaytest.New(t)
	u.Script(gatewaytest.RouteChat,
		gatewaytest.Step{Status: http.StatusTooManyRequests, Body: quotaErrorBody},
		gatewaytest.Step{Status: http.StatusOK, Body: `{"should":"not be called"}`},
	)

	p := fastTestPolicy()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := p.Do(http.DefaultClient, req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer closeBody(t, resp)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d; want 429", resp.StatusCode)
	}
	if len(u.Requests()) != 1 {
		t.Errorf("requests = %d; want 1 (insufficient_quota must not be retried)", len(u.Requests()))
	}

	// Verify that the caller can read the full response body intact
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	if string(bodyBytes) != quotaErrorBody {
		t.Errorf("body = %q; want %q", string(bodyBytes), quotaErrorBody)
	}
}

func TestConfigurableMaxAttempts(t *testing.T) {
	t.Run("default max attempts = 2 exhausts after 2 tries", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusServiceUnavailable, Body: `{"error":"503-1"}`},
			gatewaytest.Step{Status: http.StatusServiceUnavailable, Body: `{"error":"503-2"}`},
		)

		p := fastTestPolicy()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d; want 503", resp.StatusCode)
		}
		if len(u.Requests()) != 2 {
			t.Errorf("recorded %d requests; want 2", len(u.Requests()))
		}
	})

	t.Run("configured max attempts = 3 makes 3 tries", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502-1"}`},
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502-2"}`},
			gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
		)

		p := fastTestPolicy()
		p.MaxAttempts = 3

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d; want 200 OK on attempt 3", resp.StatusCode)
		}
		if len(u.Requests()) != 3 {
			t.Errorf("recorded %d requests; want 3", len(u.Requests()))
		}
	})

	t.Run("configured max attempts = 1 does not retry", func(t *testing.T) {
		u := gatewaytest.New(t)
		u.Script(gatewaytest.RouteChat,
			gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502"}`},
		)

		p := fastTestPolicy()
		p.MaxAttempts = 1

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := p.Do(http.DefaultClient, req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d; want 502", resp.StatusCode)
		}
		if len(u.Requests()) != 1 {
			t.Errorf("recorded %d requests; want 1", len(u.Requests()))
		}
	})
}

func TestContextCancellationRespected(t *testing.T) {
	t.Run("canceled before execution", func(t *testing.T) {
		p := fastTestPolicy()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		resp, err := p.Execute(ctx, func(attempt int) (*http.Response, bool, error) {
			t.Error("attempt called despite canceled context")
			return nil, false, nil
		})
		defer closeBody(t, resp)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("got error %v; want context.Canceled", err)
		}
	})

	t.Run("canceled during backoff sleep", func(t *testing.T) {
		p := reliability.DefaultPolicy()
		p.InitialBackoff = 5 * time.Second

		ctx, cancel := context.WithCancel(context.Background())

		p.OnRetry = func(c context.Context, attempt int, delay time.Duration, resp *http.Response, err error) {
			cancel()
		}

		start := time.Now()
		resp, err := p.Execute(ctx, func(attempt int) (*http.Response, bool, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
			}, false, nil
		})
		defer closeBody(t, resp)

		elapsed := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got error %v; want context.Canceled", err)
		}
		if elapsed > 1*time.Second {
			t.Errorf("took %v to cancel; sleep should have aborted immediately", elapsed)
		}
	})
}

func TestOnRetryCallbackAndMetrics(t *testing.T) {
	u := gatewaytest.New(t)
	u.Script(gatewaytest.RouteChat,
		gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502"}`},
		gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
	)

	var retryCalls int32
	var capturedAttempt int
	var capturedDelay time.Duration

	p := fastTestPolicy()
	p.LogRetries = true
	p.OnRetry = func(ctx context.Context, attempt int, delay time.Duration, resp *http.Response, err error) {
		atomic.AddInt32(&retryCalls, 1)
		capturedAttempt = attempt
		capturedDelay = delay
	}

	prov := &provider.Provider{ID: "custom-provider"}
	providerPolicy := p.ForProvider(prov)

	rc := core.NewRequestContext("req-test-123")
	rc.ResolvedProvider = prov.ID
	ctx := core.WithRequestContext(context.Background(), rc)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := providerPolicy.Do(http.DefaultClient, req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer closeBody(t, resp)

	if atomic.LoadInt32(&retryCalls) != 1 {
		t.Errorf("OnRetry called %d times; want 1", atomic.LoadInt32(&retryCalls))
	}
	if capturedAttempt != 0 {
		t.Errorf("captured attempt = %d; want 0", capturedAttempt)
	}
	if capturedDelay <= 0 {
		t.Errorf("captured delay = %v; want > 0", capturedDelay)
	}
}

func TestForProviderOverrides(t *testing.T) {
	basePolicy := reliability.DefaultPolicy()

	if !basePolicy.LogRetries {
		t.Fatal("DefaultPolicy().LogRetries = false; want true")
	}

	t.Run("default ForProvider has LogRetries true", func(t *testing.T) {
		prov := &provider.Provider{ID: "logging-prov"}
		p := basePolicy.ForProvider(prov)
		if !p.LogRetries {
			t.Errorf("p.LogRetries = false; want true")
		}

		tr := reliability.ForProvider(prov, nil)
		if !tr.Policy.LogRetries {
			t.Errorf("ForProvider transport Policy.LogRetries = false; want true")
		}
	})

	t.Run("override via Extensions retry", func(t *testing.T) {
		prov := &provider.Provider{
			ID: "ext-retry-prov",
			Extensions: map[string]json.RawMessage{
				"retry": json.RawMessage(`{"max_attempts":4,"initial_backoff":"25ms","max_backoff":"500ms","max_retry_after":"2s","disable_jitter":true}`),
			},
		}

		p := basePolicy.ForProvider(prov)
		if p.MaxAttempts != 4 {
			t.Errorf("MaxAttempts = %d; want 4", p.MaxAttempts)
		}
		if p.InitialBackoff != 25*time.Millisecond {
			t.Errorf("InitialBackoff = %v; want 25ms", p.InitialBackoff)
		}
		if p.MaxBackoff != 500*time.Millisecond {
			t.Errorf("MaxBackoff = %v; want 500ms", p.MaxBackoff)
		}
		if p.MaxRetryAfter != 2*time.Second {
			t.Errorf("MaxRetryAfter = %v; want 2s", p.MaxRetryAfter)
		}
		if !p.DisableJitter {
			t.Error("DisableJitter = false; want true")
		}
		if !p.LogRetries {
			t.Error("LogRetries = false; want true (default preserved)")
		}
	})

	t.Run("override log_retries via Extensions retry", func(t *testing.T) {
		prov := &provider.Provider{
			ID: "disable-log-prov",
			Extensions: map[string]json.RawMessage{
				"retry": json.RawMessage(`{"log_retries":false}`),
			},
		}

		p := basePolicy.ForProvider(prov)
		if p.LogRetries {
			t.Errorf("LogRetries = true; want false")
		}
	})

	t.Run("Extensions retry takes precedence over ExtraBody", func(t *testing.T) {
		prov := &provider.Provider{
			ID:        "precedence-prov",
			ExtraBody: json.RawMessage(`{"retry":{"max_attempts":3,"initial_backoff":"10ms"}}`),
			Extensions: map[string]json.RawMessage{
				"retry": json.RawMessage(`{"max_attempts":5,"initial_backoff":"30ms"}`),
			},
		}

		p := basePolicy.ForProvider(prov)
		if p.MaxAttempts != 5 {
			t.Errorf("MaxAttempts = %d; want 5", p.MaxAttempts)
		}
		if p.InitialBackoff != 30*time.Millisecond {
			t.Errorf("InitialBackoff = %v; want 30ms", p.InitialBackoff)
		}
	})

	t.Run("override via ExtraBody", func(t *testing.T) {
		prov := &provider.Provider{
			ID:        "extra-body-prov",
			ExtraBody: json.RawMessage(`{"retry":{"max_attempts":4,"initial_backoff":"25ms","max_backoff":"500ms","max_retry_after":"2s","disable_jitter":true}}`),
		}

		p := basePolicy.ForProvider(prov)
		if p.MaxAttempts != 4 {
			t.Errorf("MaxAttempts = %d; want 4", p.MaxAttempts)
		}
		if p.InitialBackoff != 25*time.Millisecond {
			t.Errorf("InitialBackoff = %v; want 25ms", p.InitialBackoff)
		}
		if p.MaxBackoff != 500*time.Millisecond {
			t.Errorf("MaxBackoff = %v; want 500ms", p.MaxBackoff)
		}
		if p.MaxRetryAfter != 2*time.Second {
			t.Errorf("MaxRetryAfter = %v; want 2s", p.MaxRetryAfter)
		}
		if !p.DisableJitter {
			t.Error("DisableJitter = false; want true")
		}
	})

	t.Run("override via RegisterProviderRetry", func(t *testing.T) {
		maxAttempts := 5
		initialBackoff := 50 * time.Millisecond
		maxBackoff := 2 * time.Second
		maxRetryAfter := 1 * time.Second

		reliability.RegisterProviderRetry("reg-prov", reliability.ProviderRetryConfig{
			MaxAttempts:    &maxAttempts,
			InitialBackoff: &initialBackoff,
			MaxBackoff:     &maxBackoff,
			MaxRetryAfter:  &maxRetryAfter,
		})
		defer reliability.ClearProviderRetries()

		prov := &provider.Provider{ID: "reg-prov"}
		p := basePolicy.ForProvider(prov)

		if p.MaxAttempts != 5 {
			t.Errorf("MaxAttempts = %d; want 5", p.MaxAttempts)
		}
		if p.InitialBackoff != 50*time.Millisecond {
			t.Errorf("InitialBackoff = %v; want 50ms", p.InitialBackoff)
		}
		if p.MaxBackoff != 2*time.Second {
			t.Errorf("MaxBackoff = %v; want 2s", p.MaxBackoff)
		}
		if p.MaxRetryAfter != 1*time.Second {
			t.Errorf("MaxRetryAfter = %v; want 1s", p.MaxRetryAfter)
		}
	})
}

func TestForProviderRetryLogging(t *testing.T) {
	u := gatewaytest.New(t)
	u.Script(gatewaytest.RouteChat,
		gatewaytest.Step{Status: http.StatusBadGateway, Body: `{"error":"502"}`},
		gatewaytest.Step{Status: http.StatusOK, Body: `{"ok":true}`},
	)

	var logBuf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	defer func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	}()

	prov := &provider.Provider{
		ID: "logged-prov",
		Extensions: map[string]json.RawMessage{
			"retry": json.RawMessage(`{"initial_backoff":"1ms","max_backoff":"5ms","disable_jitter":true}`),
		},
	}

	tr := reliability.ForProvider(prov, http.DefaultTransport)
	if !tr.Policy.LogRetries {
		t.Fatalf("tr.Policy.LogRetries = false; want true")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.URL()+gatewaytest.RouteChat, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer closeBody(t, resp)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "gateway retry:") {
		t.Errorf("expected 'gateway retry:' in log output, got: %q", logOutput)
	}
	if !strings.Contains(logOutput, "provider=logged-prov") {
		t.Errorf("expected 'provider=logged-prov' in log output, got: %q", logOutput)
	}
	if !strings.Contains(logOutput, "cause=HTTP 502") {
		t.Errorf("expected 'cause=HTTP 502' in log output, got: %q", logOutput)
	}
}

func TestTransportRoundTripContract(t *testing.T) {
	t.Run("retries transient failure without following redirects", func(t *testing.T) {
		redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/destination")
			w.WriteHeader(http.StatusFound) // 302
		}))
		defer redirectServer.Close()

		tr := &reliability.Transport{
			Policy: fastTestPolicy(),
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, redirectServer.URL, http.NoBody)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}

		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip failed: %v", err)
		}
		defer closeBody(t, resp)

		// Must return the 302 Found response directly without following the redirect
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d; want 302 (must not follow redirects)", resp.StatusCode)
		}
	})

	t.Run("does not mutate caller request and preserves body replayability", func(t *testing.T) {
		var receivedBodies []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			receivedBodies = append(receivedBodies, string(bodyBytes))
			if len(receivedBodies) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		payload := `{"test":"immutable"}`
		originalBody := io.NopCloser(bytes.NewReader([]byte(payload)))
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, originalBody)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}

		tr := &reliability.Transport{
			Policy: fastTestPolicy(),
		}

		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip failed: %v", err)
		}
		defer closeBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d; want 200", resp.StatusCode)
		}
		if len(receivedBodies) != 2 {
			t.Fatalf("received %d requests; want 2", len(receivedBodies))
		}
		if receivedBodies[0] != payload || receivedBodies[1] != payload {
			t.Errorf("received bodies mismatch: %v", receivedBodies)
		}

		// Verify caller's req was not mutated to a different URL or context
		if req.URL.String() != srv.URL {
			t.Errorf("req.URL mutated: %s", req.URL.String())
		}
	})
}

func TestConnectionRefusedRetries(t *testing.T) {
	// Point to an unused closed local port to get connection refused.
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	var attempts int
	p := fastTestPolicy()
	p.MaxAttempts = 2

	var dialer net.Dialer
	dialer.Timeout = 100 * time.Millisecond

	resp, err := p.Execute(context.Background(), func(attempt int) (*http.Response, bool, error) {
		attempts++
		conn, dialErr := dialer.DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Errorf("close conn: %v", closeErr)
			}
		}
		return nil, false, dialErr
	})
	defer closeBody(t, resp)

	if err == nil {
		t.Fatal("expected connection refused error, got nil")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d; want 2 (initial + 1 retry on connection refused)", attempts)
	}
}
