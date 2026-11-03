package provider

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"golang.org/x/net/http/httpguts"
)

// ForbiddenForwardHeader identifies client credentials, cookies and fields
// belonging to the transport. An extra allow-list cannot bypass this boundary.
func ForbiddenForwardHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	if hopByHopHeaders[name] {
		return true
	}
	switch name {
	case "Host", "Content-Length", "Proxy-Connection", "Authorization", "X-Api-Key", "Cookie", "Cookie2", "Set-Cookie", "Set-Cookie2":
		return true
	}
	return false
}

// ValidateHeaderPolicy checks only the fields needed by ApplyHeaders. It never
// mutates the provider, unlike full configuration canonicalization in Validate.
func ValidateHeaderPolicy(p *Provider) error {
	if p == nil {
		return &FieldError{Provider: "<nil>", Field: "id", Problem: "provider is nil"}
	}
	var errs []*FieldError
	add := func(field, problem string) {
		errs = append(errs, &FieldError{Provider: p.ID, Field: field, Problem: problem})
	}
	p.validateAuth(add)
	p.validateForwardHeaders(add)
	names := make([]string, 0, len(p.Headers))
	for name := range p.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		canonical := http.CanonicalHeaderKey(name)
		field := "headers[" + canonical + "]"
		switch {
		case !httpguts.ValidHeaderFieldName(name):
			add(field, "invalid header name")
		case ForbiddenForwardHeader(name):
			add(field, "credentials and transport headers use dedicated configuration")
		case seen[canonical]:
			add(field, "set more than once under different spellings")
		case !httpguts.ValidHeaderFieldValue(p.Headers[name]):
			add(field, "invalid header value")
		}
		seen[canonical] = true
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

func (p *Provider) validateForwardHeaders(add func(field, problem string)) {
	for i, name := range p.ForwardHeaders {
		field := "forward_headers[" + strconv.Itoa(i) + "]"
		switch {
		case !httpguts.ValidHeaderFieldName(name):
			add(field, "invalid header name")
		case ForbiddenForwardHeader(name):
			add(field, "client credentials and transport headers cannot be forwarded")
		}
	}
}

// ApplyLegacyHeaderDefaults turns the old passthrough exceptions into provider
// data. It is used only during legacy migration, not for new v2 configuration.
func ApplyLegacyHeaderDefaults(p *Provider) {
	if p.Protocol == ir.ProtocolAnthropicMessages {
		if p.Headers == nil {
			p.Headers = make(map[string]string)
		}
		p.Headers["User-Agent"] = "curl/8.4.0"
		p.TLS = TLSChrome
		if p.Auth.Scheme == AuthBearer {
			p.Auth.Scheme = AuthAPIKey
		}
		return
	}
	u, err := url.Parse(p.BaseURL)
	if err == nil && strings.EqualFold(u.Hostname(), "agentrouter.org") {
		if p.Headers == nil {
			p.Headers = make(map[string]string)
		}
		p.Headers["User-Agent"] = "opencode/1.18.25"
		p.Headers["X-App"] = "opencode"
	}
}
