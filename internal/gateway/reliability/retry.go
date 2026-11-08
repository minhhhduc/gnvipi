// Package reliability provides safe retry policies, bounded backoff, and attempt
// execution loops for upstream LLM providers.
package reliability

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
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
	"sync"
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

// ProviderRetryConfig specifies per-provider overrides for retry policies.
type ProviderRetryConfig struct {
	MaxAttempts    *int           `json:"max_attempts,omitempty"`
	InitialBackoff *time.Duration `json:"initial_backoff,omitempty"`
	MaxBackoff     *time.Duration `json:"max_backoff,omitempty"`
	MaxRetryAfter  *time.Duration `json:"max_retry_after,omitempty"`
	DisableJitter  *bool          `json:"disable_jitter,omitempty"`
}

var (
	providerOverridesMu sync.RWMutex
	providerOverrides   = make(map[string]ProviderRetryConfig)
)

// RegisterProviderRetry registers per-provider retry policy overrides.
func RegisterProviderRetry(providerID string, cfg ProviderRetryConfig) {
	providerOverridesMu.Lock()
	defer providerOverridesMu.Unlock()
	providerOverrides[providerID] = cfg
}

// ClearProviderRetries removes all registered per-provider retry policy overrides.
func ClearProviderRetries() {
	providerOverridesMu.Lock()
	defer providerOverridesMu.Unlock()
	providerOverrides = make(map[string]ProviderRetryConfig)
}

func getProviderOverride(providerID string) (ProviderRetryConfig, bool) {
	providerOverridesMu.RLock()
	defer providerOverridesMu.RUnlock()
	cfg, ok := providerOverrides[providerID]
	return cfg, ok
}

func (p *Policy) applyOverride(cfg ProviderRetryConfig) {
	if cfg.MaxAttempts != nil && *cfg.MaxAttempts > 0 {
		p.MaxAttempts = *cfg.MaxAttempts
	}
	if cfg.InitialBackoff != nil && *cfg.InitialBackoff > 0 {
		p.InitialBackoff = *cfg.InitialBackoff
	}
	if cfg.MaxBackoff != nil && *cfg.MaxBackoff > 0 {
		p.MaxBackoff = *cfg.MaxBackoff
	}
	if cfg.MaxRetryAfter != nil && *cfg.MaxRetryAfter > 0 {
		p.MaxRetryAfter = *cfg.MaxRetryAfter
	}
	if cfg.DisableJitter != nil {
		p.DisableJitter = *cfg.DisableJitter
	}
}

func parseExtraBodyRetry(raw json.RawMessage) (ProviderRetryConfig, bool) {
	if len(raw) == 0 {
		return ProviderRetryConfig{}, false
	}
	var wrapper struct {
		Retry *json.RawMessage `json:"retry"`
	}
	target := raw
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Retry != nil && len(*wrapper.Retry) > 0 {
		target = *wrapper.Retry
	}

	var parsed struct {
		MaxAttempts    *int        `json:"max_attempts"`
		InitialBackoff interface{} `json:"initial_backoff"`
		MaxBackoff     interface{} `json:"max_backoff"`
		MaxRetryAfter  interface{} `json:"max_retry_after"`
		DisableJitter  *bool       `json:"disable_jitter"`
	}
	if err := json.Unmarshal(target, &parsed); err != nil {
		return ProviderRetryConfig{}, false
	}

	var cfg ProviderRetryConfig
	cfg.MaxAttempts = parsed.MaxAttempts
	cfg.DisableJitter = parsed.DisableJitter

	parseDuration := func(v interface{}) *time.Duration {
		if v == nil {
			return nil
		}
		switch val := v.(type) {
		case string:
			if d, err := time.ParseDuration(val); err == nil {
				return &d
			}
		case float64:
			d := time.Duration(val * float64(time.Millisecond))
			return &d
		}
		return nil
	}

	cfg.InitialBackoff = parseDuration(parsed.InitialBackoff)
	cfg.MaxBackoff = parseDuration(parsed.MaxBackoff)
	cfg.MaxRetryAfter = parseDuration(parsed.MaxRetryAfter)

	return cfg, true
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
// It checks for per-provider overrides registered via RegisterProviderRetry
// and embedded in prov.ExtraBody under the "retry" key.
// When LogRetries is enabled, it ensures provider ID is included in logs and callbacks.
func (p Policy) ForProvider(prov *provider.Provider) Policy {
	out := p
	if prov == nil {
		return out
	}

	if override, ok := getProviderOverride(prov.ID); ok {
		out.applyOverride(override)
	}

	if len(prov.ExtraBody) > 0 {
		if override, ok := parseExtraBodyRetry(prov.ExtraBody); ok {
			out.applyOverride(override)
		}
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

const peekLimit = 8192

// isPermanentRateLimit checks if a 429 response body indicates a permanent quota exhaustion
// ("insufficient_quota") that should not be retried. It peeks and restores resp.Body so downstream
// readers can still consume the response intact.
func isPermanentRateLimit(resp *http.Response) bool {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return false
	}

	peekBuf := make([]byte, peekLimit)
	n, readErr := io.ReadFull(resp.Body, peekBuf)
	if n == 0 {
		return false
	}
	peekData := peekBuf[:n]

	// Restore resp.Body so downstream can read the full body intact.
	if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
		if closeErr := resp.Body.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			log.Printf("gateway retry: peek close body: %v", closeErr)
		}
		resp.Body = io.NopCloser(bytes.NewReader(peekData))
	} else {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(peekData), resp.Body),
			Closer: resp.Body,
		}
	}

	return containsInsufficientQuota(peekData)
}

func containsInsufficientQuota(data []byte) bool {
	var env struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err == nil {
		if strings.EqualFold(env.Error.Code, "insufficient_quota") ||
			strings.EqualFold(env.Error.Type, "insufficient_quota") ||
			strings.EqualFold(env.Code, "insufficient_quota") ||
			strings.EqualFold(env.Type, "insufficient_quota") {
			return true
		}
	}
	lower := bytes.ToLower(data)
	return bytes.Contains(lower, []byte(`"insufficient_quota"`))
}

// IsRetryable reports whether a call outcome is safe to retry using default bounds.
// It returns false if:
//   - response streaming has started (first byte written to client),
//   - the error is non-retryable,
//   - the HTTP status is not retryable,
//   - 429 or 503 specifies Retry-After exceeding DefaultMaxRetryAfter,
//   - 429 error code/type indicates permanent "insufficient_quota".
func IsRetryable(resp *http.Response, err error, streamStarted bool) bool {
	return DefaultPolicy().IsRetryable(resp, err, streamStarted)
}

// IsRetryable reports whether a call outcome is safe to retry under policy p.
func (p Policy) IsRetryable(resp *http.Response, err error, streamStarted bool) bool {
	policy := p.withDefaults()
	if streamStarted {
		return false
	}
	if err != nil {
		return IsRetryableNetworkError(err)
	}
	if resp != nil {
		if !IsRetryableStatus(resp.StatusCode) {
			return false
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			if raw := resp.Header.Get("Retry-After"); raw != "" {
				if d, ok := ParseRetryAfter(raw); ok && d > policy.MaxRetryAfter {
					return false
				}
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests && isPermanentRateLimit(resp) {
			return false
		}
		return true
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
// For HTTP 429 or 503 with a Retry-After header, the parsed duration is honored up to MaxRetryAfter.
// Otherwise, exponential backoff with jitter is applied up to MaxBackoff.
func (p Policy) ComputeDelay(attempt int, resp *http.Response) time.Duration {
	policy := p.withDefaults()

	if resp != nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) {
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

		// Check if the outcome is retryable under this policy.
		if !policy.IsRetryable(resp, err, streamStarted) {
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
// It clones req per attempt and ensures req.Body is replayable across retries without
// mutating the caller's *http.Request.
func (p Policy) Do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if req == nil {
		return nil, errors.New("reliability: nil http.Request")
	}

	ctx := req.Context()
	var bodyBytes []byte
	hasBody := false
	if req.Body != nil && req.Body != http.NoBody {
		hasBody = true
		if req.GetBody == nil {
			var err error
			bodyBytes, err = io.ReadAll(req.Body)
			if closeErr := req.Body.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, fmt.Errorf("reliability: read request body: %w", err)
			}
		}
	}

	return p.Execute(ctx, func(attempt int) (*http.Response, bool, error) {
		reqClone := req.Clone(ctx)
		if hasBody {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, false, fmt.Errorf("reliability: get body for attempt %d: %w", attempt, err)
				}
				reqClone.Body = body
			} else {
				reqClone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				reqClone.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(bodyBytes)), nil
				}
			}
		}
		resp, err := client.Do(reqClone)
		return resp, false, err
	})
}

// Transport wraps an underlying http.RoundTripper with safe retry logic.
// In accordance with the http.RoundTripper contract:
//   - It does NOT follow redirects (does not use http.Client, calls Base.RoundTrip directly).
//   - It does NOT mutate the caller's *http.Request.
//   - It clones the request and restores a fresh body for each attempt.
type Transport struct {
	Base   http.RoundTripper
	Policy Policy
}

// RoundTrip executes the request using Base and Policy.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("reliability: nil http.Request")
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	ctx := req.Context()
	var bodyBytes []byte
	hasBody := false
	if req.Body != nil && req.Body != http.NoBody {
		hasBody = true
		if req.GetBody == nil {
			var err error
			bodyBytes, err = io.ReadAll(req.Body)
			if closeErr := req.Body.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, fmt.Errorf("reliability: read request body: %w", err)
			}
		}
	}

	return t.Policy.Execute(ctx, func(attempt int) (*http.Response, bool, error) {
		reqClone := req.Clone(ctx)
		if hasBody {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, false, fmt.Errorf("reliability: get body for attempt %d: %w", attempt, err)
				}
				reqClone.Body = body
			} else {
				reqClone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				reqClone.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(bodyBytes)), nil
				}
			}
		}
		resp, err := base.RoundTrip(reqClone)
		return resp, false, err
	})
}
