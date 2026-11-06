// Package reliability provides safe retry policies, bounded backoff, and attempt
// execution loops for upstream LLM providers.
package reliability

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
)

// Default retry policy parameters.
const (
	DefaultMaxAttempts    = 2
	DefaultInitialDelay   = 100 * time.Millisecond
	DefaultMaxBackoff     = 3 * time.Second
	DefaultMaxRetryAfter  = 5 * time.Second
	DefaultBackoffFactor  = 2.0
	DefaultJitterFraction = 0.25
)

// Policy configures safe upstream request retries.
type Policy struct {
	// MaxAttempts is the total number of attempts allowed for a request
	// (1 initial attempt + retries). Default is DefaultMaxAttempts (2).
	MaxAttempts int

	// InitialBackoff is the baseline delay before the first retry.
	// Default is DefaultInitialDelay (100ms).
	InitialBackoff time.Duration

	// MaxBackoff is the maximum delay computed by exponential backoff (excluding Retry-After).
	// Default is DefaultMaxBackoff (3s).
	MaxBackoff time.Duration

	// MaxRetryAfter is the upper cap for delays specified by the upstream Retry-After header.
	// Default is DefaultMaxRetryAfter (5s).
	MaxRetryAfter time.Duration

	// BackoffFactor is the exponential multiplier applied between attempts.
	// Default is DefaultBackoffFactor (2.0).
	BackoffFactor float64

	// JitterFraction is the fraction of base backoff added as random jitter (0.0 to 1.0).
	// Default is DefaultJitterFraction (0.25).
	JitterFraction float64

	// DisableJitter disables jitter if true (primarily useful for deterministic testing).
	DisableJitter bool

	// LogRetries controls whether retry events are automatically logged via log.Printf.
	LogRetries bool

	// OnRetry is an optional callback invoked prior to sleeping on each retry attempt.
	// attempt is 0-indexed (0 before retry 1, 1 before retry 2, etc.).
	OnRetry func(ctx context.Context, attempt int, delay time.Duration, resp *http.Response, err error)

	// Sleep waits for duration d respecting context ctx. If nil, defaultSleep is used.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultPolicy returns a Policy with standard gateway defaults:
// 2 max attempts (1 initial + 1 retry), 100ms initial backoff, 3s max backoff,
// 5s max Retry-After cap, factor 2.0, and 25% jitter.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:    DefaultMaxAttempts,
		InitialBackoff: DefaultInitialDelay,
		MaxBackoff:     DefaultMaxBackoff,
		MaxRetryAfter:  DefaultMaxRetryAfter,
		BackoffFactor:  DefaultBackoffFactor,
		JitterFraction: DefaultJitterFraction,
	}
}

func (p Policy) withDefaults() Policy {
	out := p
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = DefaultMaxAttempts
	}
	if out.InitialBackoff <= 0 {
		out.InitialBackoff = DefaultInitialDelay
	}
	if out.MaxBackoff <= 0 {
		out.MaxBackoff = DefaultMaxBackoff
	}
	if out.MaxRetryAfter <= 0 {
		out.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if out.BackoffFactor <= 0 {
		out.BackoffFactor = DefaultBackoffFactor
	}
	if out.JitterFraction <= 0 && !out.DisableJitter {
		out.JitterFraction = DefaultJitterFraction
	}
	if out.Sleep == nil {
		out.Sleep = defaultSleep
	}
	return out
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ForProvider returns a copy of p customized for the given provider.
// When LogRetries is enabled, it ensures provider ID is included in logs and callbacks.
func (p Policy) ForProvider(prov *provider.Provider) Policy {
	out := p
	if prov == nil {
		return out
	}
	prevOnRetry := out.OnRetry
	out.OnRetry = func(ctx context.Context, attempt int, delay time.Duration, resp *http.Response, err error) {
		if out.LogRetries {
			logRetry(ctx, prov.ID, attempt, delay, resp, err)
		}
		if prevOnRetry != nil {
			prevOnRetry(ctx, attempt, delay, resp, err)
		}
	}
	return out
}

func logRetry(ctx context.Context, providerID string, attempt int, delay time.Duration, resp *http.Response, err error) {
	reqID := "-"
	if rc := core.RequestContextFrom(ctx); rc != nil {
		if rc.ID != "" {
			reqID = rc.ID
		}
		if providerID == "" && rc.ResolvedProvider != "" {
			providerID = rc.ResolvedProvider
		}
	}
	var cause string
	if resp != nil {
		cause = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else if err != nil {
		cause = redact.Default.Text(err.Error())
	}
	log.Printf("gateway retry: request_id=%s provider=%s attempt=%d delay=%v cause=%s",
		reqID, providerID, attempt+1, delay, cause)
}

// IsRetryableStatus reports whether an HTTP status code is safe to retry.
// Retryable statuses are:
//   - 408 Request Timeout
//   - 429 Too Many Requests
//   - 502 Bad Gateway
//   - 503 Service Unavailable
//   - 504 Gateway Timeout
//
// All other 4xx statuses (e.g. 400, 401, 403, 404, 409, 422) and 5xx statuses
// (e.g. 500 Internal Server Error) are strictly NOT retryable.
func IsRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// IsRetryableNetworkError reports whether a network/transport error that occurred
// before the response headers were received is safe to retry.
// Retryable errors include:
//   - Connection refused
//   - Connection reset by peer / forcibly closed
//   - DNS temporary errors (server failure, temporary timeout)
//   - Abrupt connection disconnects before response headers (io.EOF, io.ErrUnexpectedEOF)
//
// Non-retryable errors include:
//   - Context cancellation or deadline exceeded (client aborts/timeouts)
//   - TLS certificate errors (trust, validity, hostname mismatch)
//   - DNS host not found (NXDOMAIN)
func IsRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}

	// Respect client context: cancellation and timeouts originating from context are NEVER retried.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// TLS certificate errors indicate trust/configuration issues and must never be retried.
	var (
		unknownAuthErr  x509.UnknownAuthorityError
		unknownAuthPErr *x509.UnknownAuthorityError
		certInvalidErr  x509.CertificateInvalidError
		certInvalidPErr *x509.CertificateInvalidError
		hostnameErr     x509.HostnameError
		hostnamePErr    *x509.HostnameError
	)
	if errors.As(err, &unknownAuthErr) || errors.As(err, &unknownAuthPErr) ||
		errors.As(err, &certInvalidErr) || errors.As(err, &certInvalidPErr) ||
		errors.As(err, &hostnameErr) || errors.As(err, &hostnamePErr) {
		return false
	}

	// DNS errors: temporary errors and timeouts are retryable; not-found (NXDOMAIN) is not.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return false
		}
		if dnsErr.IsTemporary || dnsErr.IsTimeout {
			return true
		}
		msg := strings.ToLower(dnsErr.Err)
		if strings.Contains(msg, "server failure") || strings.Contains(msg, "servfail") || strings.Contains(msg, "timeout") {
			return true
		}
		return false
	}

	// Abrupt disconnect before response headers.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Syscall error inspection.
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) && isRetryableSyscallErrno(syscallErr.Err) {
		return true
	}

	// Net op error inspection.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if isRetryableSyscallErrno(opErr.Err) {
			return true
		}
		// A timeout on the dial or socket before response headers is retryable,
		// provided it is not caused by caller context cancellation.
		if opErr.Timeout() && !errors.Is(opErr.Err, context.Canceled) && !errors.Is(opErr.Err, context.DeadlineExceeded) {
			return true
		}
	}

	// Direct syscall errno match.
	if isRetryableSyscallErrno(err) {
		return true
	}

	// String fallback for OS-specific, runtime, or wrapped network error strings.
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "forcibly closed") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connect: connection refused") ||
		strings.Contains(errStr, "connectex: no connection could be made") {
		return true
	}

	return false
}

func isRetryableSyscallErrno(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED:
			return true
		// Windows WSA error numbers:
		// WSAECONNREFUSED = 10061, WSAECONNRESET = 10054, WSAECONNABORTED = 10053,
		// WSAETIMEDOUT = 10060, WSAENETRESET = 10052
		case 10061, 10054, 10053, 10060, 10052:
			return true
		}
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED)
}

// IsRetryable reports whether a call outcome is safe to retry.
// It returns false if response streaming has started (first byte written to client),
// if the error is non-retryable, or if the HTTP status is not retryable.
func IsRetryable(resp *http.Response, err error, streamStarted bool) bool {
	if streamStarted {
		return false
	}
	if err != nil {
		return IsRetryableNetworkError(err)
	}
	if resp != nil {
		return IsRetryableStatus(resp.StatusCode)
	}
	return false
}

// ParseRetryAfter parses a Retry-After header string.
// It supports integer or decimal seconds (e.g. "5", "2.5") and HTTP dates
// (RFC1123, RFC850, ANSI C). Returns (0, false) if empty, invalid, or negative.
func ParseRetryAfter(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}

	// Seconds format (integer or float)
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}

	// HTTP-date format (RFC1123, RFC850, ANSI C)
	if t, err := http.ParseTime(raw); err == nil {
		wait := time.Until(t)
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}

	return 0, false
}

// ComputeDelay calculates the backoff wait duration for the next retry attempt.
// For HTTP 429 with a Retry-After header, the parsed duration is honored up to MaxRetryAfter.
// Otherwise, exponential backoff with jitter is applied up to MaxBackoff.
func (p Policy) ComputeDelay(attempt int, resp *http.Response) time.Duration {
	policy := p.withDefaults()

	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if d, ok := ParseRetryAfter(resp.Header.Get("Retry-After")); ok {
			if d > policy.MaxRetryAfter {
				return policy.MaxRetryAfter
			}
			return d
		}
	}

	multiplier := math.Pow(policy.BackoffFactor, float64(attempt))
	base := time.Duration(float64(policy.InitialBackoff) * multiplier)
	if base > policy.MaxBackoff {
		base = policy.MaxBackoff
	}

	if policy.DisableJitter || policy.JitterFraction <= 0 {
		return base
	}

	jitterMax := float64(base) * policy.JitterFraction
	jitter := time.Duration(rand.Float64() * jitterMax)
	delay := base + jitter
	if delay > policy.MaxBackoff {
		delay = policy.MaxBackoff
	}
	return delay
}

// MakeReplayable ensures that req.GetBody is populated and rewinds req.Body.
// If req.GetBody is already set, it calls GetBody to reset req.Body.
// If req.GetBody is nil and req.Body is present, it buffers the body into memory,
// sets req.GetBody, and assigns req.Body.
func MakeReplayable(req *http.Request) error {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if req.GetBody != nil {
		newBody, err := req.GetBody()
		if err != nil {
			return fmt.Errorf("reliability: reset request body: %w", err)
		}
		req.Body = newBody
		return nil
	}

	data, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("reliability: buffer request body: %w", err)
	}
	if closeErr := req.Body.Close(); closeErr != nil {
		return fmt.Errorf("reliability: close request body: %w", closeErr)
	}

	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	newBody, getErr := req.GetBody()
	if getErr != nil {
		return fmt.Errorf("reliability: get request body: %w", getErr)
	}
	req.Body = newBody
	return nil
}

// RewindRequestBody resets req.Body to the beginning using req.GetBody.
// It returns an error if req.GetBody is nil or fails.
func RewindRequestBody(req *http.Request) error {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if req.GetBody == nil {
		return errors.New("reliability: request body cannot be rewound without GetBody")
	}
	newBody, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("reliability: rewind request body: %w", err)
	}
	req.Body = newBody
	return nil
}

// AttemptFunc executes one attempt of an upstream request.
// It returns:
//   - resp: the *http.Response if headers were received
//   - streamStarted: true if response streaming has started (first byte written to client)
//   - err: transport error if any occurred
type AttemptFunc func(attempt int) (resp *http.Response, streamStarted bool, err error)

// Execute runs fn up to p.MaxAttempts times with safe retry semantics.
func (p Policy) Execute(ctx context.Context, fn AttemptFunc) (*http.Response, error) {
	policy := p.withDefaults()

	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		// Stop immediately if context is done before starting attempt.
		if err := ctx.Err(); err != nil {
			if lastResp != nil && lastResp.Body != nil {
				if closeErr := lastResp.Body.Close(); closeErr != nil {
					err = errors.Join(err, closeErr)
				}
			}
			return nil, err
		}

		resp, streamStarted, err := fn(attempt)

		// Context cancellation during the attempt is never retried.
		if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			if resp != nil && resp.Body != nil {
				if closeErr := resp.Body.Close(); closeErr != nil {
					return nil, closeErr
				}
			}
			return nil, ctxErr
		}

		// Also check core.RequestContext if attached to context.
		if rc := core.RequestContextFrom(ctx); rc != nil && !rc.FirstByte.IsZero() {
			streamStarted = true
		}

		// If streaming has started to the client, NEVER retry.
		if streamStarted {
			return resp, err
		}

		// Check if the outcome is retryable.
		if !IsRetryable(resp, err, streamStarted) {
			return resp, err
		}

		// If this was the last allowed attempt, do not retry.
		if attempt >= policy.MaxAttempts-1 {
			return resp, err
		}

		// Compute delay honoring Retry-After or exponential backoff with jitter.
		delay := policy.ComputeDelay(attempt, resp)

		if policy.OnRetry != nil {
			policy.OnRetry(ctx, attempt, delay, resp, err)
		} else if policy.LogRetries {
			logRetry(ctx, "", attempt, delay, resp, err)
		}

		// Close response body of the failed attempt before sleeping.
		if resp != nil && resp.Body != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				return nil, closeErr
			}
		}

		// Sleep respecting ctx.Done().
		if sleepErr := policy.Sleep(ctx, delay); sleepErr != nil {
			return nil, sleepErr
		}

		lastResp = resp
		lastErr = err
	}

	return lastResp, lastErr
}

// Do executes an HTTP request using client and applying safe retries according to policy.
func (p Policy) Do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if req == nil {
		return nil, errors.New("reliability: nil http.Request")
	}

	if err := MakeReplayable(req); err != nil {
		return nil, err
	}

	return p.Execute(req.Context(), func(attempt int) (*http.Response, bool, error) {
		if attempt > 0 {
			if err := RewindRequestBody(req); err != nil {
				return nil, false, err
			}
		}
		resp, err := client.Do(req)
		return resp, false, err
	})
}

// Transport wraps an underlying http.RoundTripper with safe retry logic.
type Transport struct {
	Base   http.RoundTripper
	Policy Policy
}

// RoundTrip executes the request using Base and Policy.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	client := &http.Client{Transport: base}
	return t.Policy.Do(client, req)
}
