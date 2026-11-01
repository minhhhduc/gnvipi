package gwerr

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKindFromHTTPStatus(t *testing.T) {
	tests := []struct {
		status int
		want   Kind
	}{
		{400, KindInvalidRequest},
		{401, KindAuthentication},
		{403, KindPermission},
		{404, KindNotFound},
		{408, KindTimeout},
		{409, KindInvalidRequest},
		{413, KindInvalidRequest},
		{422, KindInvalidRequest},
		{429, KindRateLimit},
		{500, KindUpstreamError},
		{501, KindUpstreamError},
		{502, KindOverloaded},
		{503, KindOverloaded},
		{504, KindOverloaded},
		{529, KindOverloaded},
		{418, KindUpstreamError}, // unmapped upstream status
	}
	for _, tt := range tests {
		if got := KindFromHTTPStatus(tt.status); got != tt.want {
			t.Errorf("KindFromHTTPStatus(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestStatusForKind(t *testing.T) {
	tests := []struct {
		kind Kind
		want int
	}{
		{KindInvalidRequest, 400},
		{KindAuthentication, 401},
		{KindPermission, 403},
		{KindNotFound, 404},
		{KindRateLimit, 429},
		{KindOverloaded, 503},
		{KindTimeout, 504},
		{KindUpstreamError, 502},
		{KindInternal, 500},
	}
	for _, tt := range tests {
		if got := StatusForKind(tt.kind); got != tt.want {
			t.Errorf("StatusForKind(%q) = %d, want %d", tt.kind, got, tt.want)
		}
	}
}

func TestKindFromErr(t *testing.T) {
	timeoutErr := &timeoutNetErr{}
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, KindUpstreamError},
		{"context deadline", context.DeadlineExceeded, KindTimeout},
		{"context canceled", context.Canceled, KindTimeout},
		{"wrapped deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), KindTimeout},
		{"net timeout", timeoutErr, KindTimeout},
		{"other", errors.New("connection refused"), KindUpstreamError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindFromErr(tt.err); got != tt.want {
				t.Errorf("KindFromErr(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return false }

func TestEncodeGoldenBodies(t *testing.T) {
	// One error per kind, encoded for all three protocols. Bodies are
	// golden: an inbound client parses these exactly as the real providers'
	// APIs would answer.
	tests := []struct {
		kind      Kind
		status    int
		anthropic string
		chat      string
		responses string
	}{
		{KindInvalidRequest, 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"invalid_request_error"}}`},
		{KindAuthentication, 401,
			`{"type":"error","error":{"type":"authentication_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"authentication_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"authentication_error"}}`},
		{KindPermission, 403,
			`{"type":"error","error":{"type":"permission_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"permission_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"permission_error"}}`},
		{KindNotFound, 404,
			`{"type":"error","error":{"type":"not_found_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"not_found_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"not_found_error"}}`},
		{KindRateLimit, 429,
			`{"type":"error","error":{"type":"rate_limit_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"rate_limit_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"rate_limit_error"}}`},
		{KindOverloaded, 503,
			`{"type":"error","error":{"type":"overloaded_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"server_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"server_error"}}`},
		{KindTimeout, 504,
			`{"type":"error","error":{"type":"timeout_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"server_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"server_error"}}`},
		{KindUpstreamError, 502,
			`{"type":"error","error":{"type":"api_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"server_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"server_error"}}`},
		{KindInternal, 500,
			`{"type":"error","error":{"type":"api_error","message":"bad"}}`,
			`{"error":{"message":"bad","type":"server_error","param":null,"code":null}}`,
			`{"error":{"code":null,"message":"bad","param":null,"type":"server_error"}}`},
	}
	for _, tt := range tests {
		e := &Error{Kind: tt.kind, Message: "bad"}
		for _, tc := range []struct {
			proto Protocol
			want  string
		}{
			{ProtocolAnthropicMessages, tt.anthropic},
			{ProtocolOpenAIChat, tt.chat},
			{ProtocolOpenAIResponses, tt.responses},
		} {
			body, status, err := Encode(tc.proto, e)
			if err != nil {
				t.Fatalf("Encode(%s, %s): %v", tc.proto, tt.kind, err)
			}
			if status != tt.status {
				t.Errorf("Encode(%s, %s) status = %d, want %d", tc.proto, tt.kind, status, tt.status)
			}
			if got := string(body); got != tc.want {
				t.Errorf("Encode(%s, %s) =\n%s\nwant\n%s", tc.proto, tt.kind, got, tc.want)
			}
		}
	}
}

func TestEncodeExplicitStatusWins(t *testing.T) {
	e := &Error{Status: 418, Kind: KindInvalidRequest, Message: "bad"}
	if _, status, err := Encode(ProtocolOpenAIChat, e); err != nil || status != 418 {
		t.Errorf("Encode with explicit Status = %d, %v, want 418", status, err)
	}
}

func TestEncodeCarriesProviderCode(t *testing.T) {
	e := &Error{Kind: KindRateLimit, Message: "quota exceeded", Code: "insufficient_quota"}
	for _, proto := range []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		body, _, err := Encode(proto, e)
		if err != nil {
			t.Fatalf("Encode(%s): %v", proto, err)
		}
		if !strings.Contains(string(body), `"insufficient_quota"`) {
			t.Errorf("Encode(%s) = %s, missing the provider code", proto, body)
		}
	}
}

func TestEncodeUnknownProtocol(t *testing.T) {
	_, _, err := Encode(Protocol("wut"), &Error{Kind: KindInternal})
	if err == nil {
		t.Fatal("Encode on an unknown protocol succeeded")
	}
}

func TestWriteResponse(t *testing.T) {
	e := &Error{
		Status:            429,
		Kind:              KindRateLimit,
		Message:           "too many requests",
		UpstreamRequestID: "req_abc",
		RetryAfter:        30 * time.Second,
	}
	rec := httptest.NewRecorder()
	if err := Write(rec, ProtocolAnthropicMessages, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := rec.Code; got != 429 {
		t.Errorf("status = %d, want 429", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req_abc" {
		t.Errorf("X-Request-ID = %q, want req_abc", got)
	}
	if got := rec.Header().Get("request-id"); got != "req_abc" {
		t.Errorf("request-id = %q, want req_abc (Anthropic convention)", got)
	}
	want := `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestWriteOmitsOptionalHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Write(rec, ProtocolOpenAIChat, &Error{Kind: KindInternal, Message: "bad"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.Code != 500 {
		t.Errorf("status = %d, want 500 (derived from kind)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want empty", got)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "" {
		t.Errorf("X-Request-ID = %q, want empty", got)
	}
	if got := rec.Header().Get("request-id"); got != "" {
		t.Errorf("request-id = %q, want empty on non-Anthropic protocols", got)
	}
}

func TestScrubRemovesSecrets(t *testing.T) {
	const key = "sk-deadbeef1234"
	t.Run("Scrub", func(t *testing.T) {
		got := Scrub("auth failed for "+key+" in "+key, key, "")
		const want = "auth failed for [redacted] in [redacted]"
		if got != want {
			t.Errorf("Scrub() = %q, want %q", got, want)
		}
	})
	t.Run("encoders scrub the message", func(t *testing.T) {
		e := &Error{
			Kind:    KindAuthentication,
			Message: "upstream said: invalid key " + key,
			Secrets: []string{key},
		}
		for _, proto := range []Protocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses} {
			body, _, err := Encode(proto, e)
			if err != nil {
				t.Fatalf("Encode(%s): %v", proto, err)
			}
			if strings.Contains(string(body), key) {
				t.Errorf("Encode(%s) = %s leaked the API key", proto, body)
			}
			if !strings.Contains(string(body), "[redacted]") {
				t.Errorf("Encode(%s) = %s lost the redaction marker", proto, body)
			}
		}
	})
	t.Run("SSE errors scrub the message", func(t *testing.T) {
		e := &Error{Kind: KindUpstreamError, Message: "bad key " + key, Secrets: []string{key}}
		var sb strings.Builder
		if err := WriteSSEError(&sb, ProtocolOpenAIResponses, e, "resp_1"); err != nil {
			t.Fatalf("WriteSSEError: %v", err)
		}
		if strings.Contains(sb.String(), key) {
			t.Errorf("SSE error leaked the API key: %s", sb.String())
		}
	})
}

func TestWriteSSEErrorGoldenBodies(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		e := &Error{Kind: KindOverloaded, Message: "overloaded"}
		var sb strings.Builder
		if err := WriteSSEError(&sb, ProtocolAnthropicMessages, e, ""); err != nil {
			t.Fatalf("WriteSSEError: %v", err)
		}
		want := "event: error\n" +
			`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}` + "\n\n"
		if got := sb.String(); got != want {
			t.Errorf("SSE =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("openai chat", func(t *testing.T) {
		e := &Error{Kind: KindRateLimit, Message: "slow down", Code: "insufficient_quota"}
		var sb strings.Builder
		if err := WriteSSEError(&sb, ProtocolOpenAIChat, e, ""); err != nil {
			t.Fatalf("WriteSSEError: %v", err)
		}
		want := `data: {"error":{"message":"slow down","type":"rate_limit_error","param":null,"code":"insufficient_quota"}}` + "\n\n"
		if got := sb.String(); got != want {
			t.Errorf("SSE =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("responses", func(t *testing.T) {
		e := &Error{Kind: KindUpstreamError, Message: "provider failed"}
		var sb strings.Builder
		if err := WriteSSEError(&sb, ProtocolOpenAIResponses, e, "resp_42"); err != nil {
			t.Fatalf("WriteSSEError: %v", err)
		}
		want := "event: error\n" +
			`data: {"type":"error","code":"server_error","message":"provider failed","param":null}` + "\n\n" +
			"event: response.failed\n" +
			`data: {"type":"response.failed","response":{"id":"resp_42","status":"failed","error":{"code":"server_error","message":"provider failed","param":null}}}` + "\n\n"
		if got := sb.String(); got != want {
			t.Errorf("SSE =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("unknown protocol", func(t *testing.T) {
		var sb strings.Builder
		if err := WriteSSEError(&sb, Protocol("wut"), &Error{Kind: KindInternal}, ""); err == nil {
			t.Error("WriteSSEError on an unknown protocol succeeded")
		}
	})
}

func TestSSEEventsParseAsSSE(t *testing.T) {
	// The raw bytes WriteSSEError emits must be a valid SSE event stream:
	// feed them to the internal/gateway/sse reader's event grammar by hand.
	// (gwerr must not import sse, so this asserts the grammar directly:
	// "event:" line, "data:" line, blank line, LF endings.)
	var sb strings.Builder
	e := &Error{Kind: KindUpstreamError, Message: "boom"}
	if err := WriteSSEError(&sb, ProtocolOpenAIResponses, e, "resp_9"); err != nil {
		t.Fatalf("WriteSSEError: %v", err)
	}
	for _, want := range []string{
		"event: error\ndata: {\"type\":\"error\"",
		"\n\n",
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_9\"",
		"\n\n",
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("SSE stream %q missing %q", sb.String(), want)
		}
	}
}

func TestErrorImplementsError(t *testing.T) {
	e := &Error{Kind: KindInvalidRequest, Message: "bad"}
	var err error = e
	if got := err.Error(); got != "gwerr: invalid_request: bad" {
		t.Errorf("Error() = %q", got)
	}
}
