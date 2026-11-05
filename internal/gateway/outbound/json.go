package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// NewJSONRequest builds one provider-targeted JSON POST without changing fields,
// target.Headers, or their nested values. The resolved target model is always
// authoritative. suffix follows the gateway's provider endpoint convention.
func NewJSONRequest(ctx context.Context, target adapter.Target, suffix string, fields map[string]any) (*http.Request, error) {
	if ctx == nil {
		return nil, errors.New("outbound: request context is nil")
	}
	if strings.TrimSpace(target.Model) == "" {
		return nil, errors.New("outbound: target model is required")
	}
	if err := provider.ValidateEndpointPath(suffix); err != nil {
		return nil, fmt.Errorf("outbound: endpoint suffix: %w", err)
	}
	endpoint, err := jsonEndpoint(target.BaseURL, suffix)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]any, len(fields)+1)
	for name, value := range fields {
		owned[name] = value
	}
	owned["model"] = target.Model
	body, err := json.Marshal(owned)
	if err != nil {
		return nil, fmt.Errorf("outbound: encode JSON request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("outbound: build request: %w", err)
	}
	request.Header = target.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func jsonEndpoint(base, suffix string) (*url.URL, error) {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, suffix):
		base = strings.TrimSuffix(base, suffix)
	case !strings.HasSuffix(base, "/v1"):
		suffix = "/v1" + suffix
	}
	return endpointURL(base, suffix)
}

// UpstreamError maps a provider HTTP error and optional JSON envelope without
// taking ownership of resp.Body. OpenAI-style, Anthropic-style, and flat
// message/code envelopes are accepted; malformed bodies retain a safe fallback.
func UpstreamError(resp *http.Response, body []byte) *gwerr.Error {
	if resp == nil {
		return &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "upstream response is nil"}
	}
	message, code := upstreamErrorFields(body)
	if message == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	requestID := resp.Header.Get(core.RequestIDHeader)
	if requestID == "" {
		requestID = resp.Header.Get("request-id")
	}
	if requestID == "" {
		requestID = resp.Header.Get("openai-request-id")
	}
	return &gwerr.Error{
		Kind:              gwerr.KindFromHTTPStatus(resp.StatusCode),
		Message:           message,
		Code:              code,
		UpstreamRequestID: requestID,
		UpstreamStatus:    resp.StatusCode,
		RetryAfter:        retryAfter(resp.Header.Get("Retry-After")),
	}
}

func upstreamErrorFields(body []byte) (message, code string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return "", ""
	}
	payload := root
	if raw, found := root["error"]; found && !rawNull(raw) {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && nested != nil {
			payload = nested
		} else if message := rawScalar(raw); message != "" {
			return message, rawScalar(root["code"])
		}
	}
	message = rawScalar(payload["message"])
	if message == "" {
		message = rawScalar(root["message"])
	}
	code = rawScalar(payload["code"])
	if code == "" {
		code = rawScalar(root["code"])
	}
	return message, code
}

func rawScalar(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	if raw[0] == '{' || raw[0] == '[' || !json.Valid(raw) {
		return ""
	}
	return string(raw)
}

func rawNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if duration, err := time.ParseDuration(value + "s"); err == nil && duration > 0 {
		return duration
	}
	if when, err := http.ParseTime(value); err == nil {
		if duration := time.Until(when); duration > 0 {
			return duration
		}
	}
	return 0
}

// JSONObject reports whether raw is a non-empty, valid JSON object {...}.
func JSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) >= 2 && raw[0] == '{' && raw[len(raw)-1] == '}' && json.Valid(raw)
}

// ExtensionFields copies unreserved extension fields into a newly allocated
// map[string]any. It reports an error if any retained extension is not valid JSON.
func ExtensionFields(extensions map[string]json.RawMessage, isReserved func(name string) bool) (map[string]any, error) {
	fields := make(map[string]any, len(extensions)+8)
	for name, value := range extensions {
		if isReserved != nil && isReserved(name) {
			continue
		}
		if !json.Valid(value) {
			return nil, fmt.Errorf("outbound: extension %q contains invalid JSON", name)
		}
		fields[name] = value
	}
	return fields, nil
}
