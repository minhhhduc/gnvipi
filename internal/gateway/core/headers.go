package core

import (
	"net/http"
	"os"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"golang.org/x/net/http/httpguts"
)

var forwardedHeaders = map[string]bool{
	"User-Agent": true, "Anthropic-Version": true, "Anthropic-Beta": true,
	"Openai-Beta": true, "X-Request-Id": true, "Accept": true,
}

// ApplyHeaders builds the upstream header policy without mutating src or p.
// Safe existing destination fields are retained; allowed client fields are
// copied next, then absent protocol defaults, then provider static headers,
// then provider auth. Transport-owned identity wins last. Credentials, cookies
// and Connection-nominated fields cannot enter via the client allow-list.
func ApplyHeaders(dst, src *http.Request, p *provider.Provider) error {
	if dst == nil {
		return &provider.FieldError{Field: "request", Problem: "destination request is nil"}
	}
	if err := provider.ValidateHeaderPolicy(p); err != nil {
		return err
	}
	key := p.Auth.Key
	if p.Auth.KeyEnv != "" {
		key = os.Getenv(p.Auth.KeyEnv)
		if key == "" {
			return &provider.FieldError{Provider: p.ID, Field: "auth.key_env", Problem: "credential environment variable is empty"}
		}
	}
	if !httpguts.ValidHeaderFieldValue(key) {
		return &provider.FieldError{Provider: p.ID, Field: "auth.key", Problem: "invalid credential header value"}
	}

	var incoming http.Header
	if src != nil {
		incoming = src.Header
	}
	nominated := make(map[string]bool)
	for _, headers := range []http.Header{incoming, dst.Header} {
		for name, values := range headers {
			if !strings.EqualFold(name, "Connection") {
				continue
			}
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					nominated[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	out := make(http.Header, len(dst.Header)+len(p.Headers))
	for name, values := range dst.Header {
		canonical := http.CanonicalHeaderKey(name)
		if provider.ForbiddenForwardHeader(canonical) || nominated[canonical] {
			continue
		}
		out[canonical] = append(out[canonical], values...)
	}
	for name, values := range incoming {
		canonical := http.CanonicalHeaderKey(name)
		if provider.ForbiddenForwardHeader(canonical) || nominated[canonical] || !headerAllowed(canonical, p.ForwardHeaders) || !protocolHeader(canonical, p.Protocol) {
			continue
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return &provider.FieldError{Provider: p.ID, Field: "headers[" + canonical + "]", Problem: "invalid forwarded header value"}
			}
		}
		out[canonical] = append([]string(nil), values...)
	}
	if p.Protocol == ir.ProtocolAnthropicMessages && out.Get("Anthropic-Version") == "" {
		out.Set("Anthropic-Version", "2023-06-01")
	}
	for name, value := range p.Headers {
		out.Set(name, value)
	}
	switch p.Auth.Scheme {
	case provider.AuthBearer:
		out.Set("Authorization", "Bearer "+key)
	case provider.AuthAPIKey:
		out.Set("X-Api-Key", key)
	}
	dst.Header = out
	PropagateRequestID(dst)
	return nil
}

// protocolHeader reports whether a family-specific header may reach the target
// protocol. Anthropic-Version and Anthropic-Beta name Anthropic features, and
// Openai-Beta names an OpenAI feature; forwarding either to the other family
// leaves the upstream with a flag it cannot interpret. The gateway sets the
// Anthropic protocol default itself, so dropping a client's value loses
// nothing that a Messages target needs.
func protocolHeader(name string, protocol ir.Protocol) bool {
	switch name {
	case "Anthropic-Version", "Anthropic-Beta":
		return protocol == ir.ProtocolAnthropicMessages
	case "Openai-Beta":
		return protocol != ir.ProtocolAnthropicMessages
	default:
		return true
	}
}

func headerAllowed(name string, extras []string) bool {
	if forwardedHeaders[name] {
		return true
	}
	for _, extra := range extras {
		if strings.EqualFold(name, extra) {
			return true
		}
	}
	return false
}
