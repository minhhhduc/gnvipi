package gwerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Protocol names the inbound protocol whose error shape is wanted. The
// values match internal/gateway/ir.Protocol's so the two align once the
// packages are unified.
type Protocol string

const (
	ProtocolAnthropicMessages Protocol = "anthropic-messages"
	ProtocolOpenAIChat        Protocol = "openai-chat"
	ProtocolOpenAIResponses   Protocol = "openai-responses"
)

// Kind is the protocol-independent classification of an error. Nine kinds
// cover the gateway's whole error surface; do not add a tenth without
// teaching every encoder about it.
type Kind string

const (
	KindInvalidRequest Kind = "invalid_request"
	KindAuthentication Kind = "authentication"
	KindPermission     Kind = "permission"
	KindNotFound       Kind = "not_found"
	KindRateLimit      Kind = "rate_limit"
	KindOverloaded     Kind = "overloaded"
	KindTimeout        Kind = "timeout"
	KindUpstreamError  Kind = "upstream_error"
	KindInternal       Kind = "internal"
)

// Error is one normalized gateway error. Status is the HTTP status the
// gateway answers with; zero means derive it from Kind via StatusForKind.
// UpstreamStatus is the provider's status when this error wraps an upstream
// response, zero otherwise. RetryAfter, when positive, becomes a
// Retry-After header. Code carries a provider error code when the upstream
// named one (OpenAI's "insufficient_quota", for instance). Secrets are
// redacted from Message before it is encoded.
type Error struct {
	Status            int
	Kind              Kind
	Message           string
	Code              string
	UpstreamRequestID string
	UpstreamStatus    int
	RetryAfter        time.Duration
	Secrets           []string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("gwerr: %s: %s", e.Kind, e.Message)
}

// ScrubbedMessage returns Message with every secret in Secrets replaced by
// "[redacted]". Empty secrets are ignored.
func (e *Error) ScrubbedMessage() string {
	return Scrub(e.Message, e.Secrets...)
}

// StatusForKind is the default HTTP status for an error classified as k.
func StatusForKind(k Kind) int {
	switch k {
	case KindInvalidRequest:
		return 400
	case KindAuthentication:
		return 401
	case KindPermission:
		return 403
	case KindNotFound:
		return 404
	case KindRateLimit:
		return 429
	case KindOverloaded:
		return 503
	case KindTimeout:
		return 504
	case KindUpstreamError:
		return 502
	default:
		return 500
	}
}

// KindFromHTTPStatus maps an HTTP status — normally an upstream's — to a
// Kind. It covers the statuses the gateway's providers return: the 4xx
// client errors, 408 and other timeouts, 429, and the 5xx family where
// 502/503/504/529 read as overload and the rest as a plain upstream
// failure. Unknown statuses map to KindUpstreamError.
func KindFromHTTPStatus(status int) Kind {
	switch {
	case status == 400, status == 409, status == 413, status == 422:
		return KindInvalidRequest
	case status == 401:
		return KindAuthentication
	case status == 403:
		return KindPermission
	case status == 404:
		return KindNotFound
	case status == 408:
		return KindTimeout
	case status == 429:
		return KindRateLimit
	case status == 502, status == 503, status == 504, status == 529:
		return KindOverloaded
	case status >= 500:
		return KindUpstreamError
	default:
		return KindUpstreamError
	}
}

// KindFromErr maps a transport error — a read timeout, a canceled context,
// a refused connection — to a Kind. Timeouts and context cancellation are
// KindTimeout (the client may retry); everything else is KindUpstreamError.
func KindFromErr(err error) Kind {
	if err == nil {
		return KindUpstreamError
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return KindTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return KindTimeout
	}
	return KindUpstreamError
}

// Scrub replaces every non-empty secret in s with "[redacted]". Upstream
// error bodies sometimes embed the key that requested them; call this
// before any such text reaches a client.
func Scrub(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}
