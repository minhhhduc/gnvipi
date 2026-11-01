package provider

import (
	"errors"
	"fmt"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"golang.org/x/net/http/httpguts"
)

// AuthScheme selects how a provider credential is sent upstream.
type AuthScheme string

const (
	// AuthNone sends no credential.
	AuthNone AuthScheme = "none"
	// AuthBearer sends "Authorization: Bearer <key>".
	AuthBearer AuthScheme = "bearer"
	// AuthAPIKey sends "x-api-key: <key>", which Anthropic-style endpoints expect.
	AuthAPIKey AuthScheme = "x-api-key"
)

// TLSProfile selects the TLS fingerprint the upstream client presents.
type TLSProfile string

const (
	// TLSDefault uses the standard library's TLS client.
	TLSDefault TLSProfile = "default"
	// TLSChrome impersonates a Chrome TLS fingerprint, for providers that
	// reject clients they do not recognize.
	TLSChrome TLSProfile = "chrome"
)

// Auth is one provider credential. Exactly one of Key and KeyEnv is set when
// the scheme is not none: Key is a literal secret, KeyEnv names the environment
// variable holding it.
type Auth struct {
	Scheme AuthScheme
	Key    string
	KeyEnv string
}

// ModelMapping is one model the provider serves: the alias clients ask for and
// the model identifier the upstream expects. Alias may end in "/*" to map a
// whole family (for example "vendor/*").
type ModelMapping struct {
	Alias         string
	UpstreamModel string
	Hidden        bool
}

// Timeouts bounds one upstream call. Zero means "use the client default".
type Timeouts struct {
	Connect        time.Duration // TCP connect
	ResponseHeader time.Duration // until the response headers arrive
	Idle           time.Duration // longest silence inside a streamed body
}

// Provider is one upstream endpoint.
type Provider struct {
	ID       string
	Name     string
	BaseURL  string
	Protocol ir.Protocol
	Auth     Auth
	Headers  map[string]string
	Models   []ModelMapping
	Timeouts Timeouts
	Enabled  bool
	TLS      TLSProfile
}

// FieldError reports one invalid field of one provider.
type FieldError struct {
	Provider string // provider ID, or "legacy[i]" while converting
	Field    string // "name", "base_url", "auth.key", "models[2].alias", ...
	Problem  string // what is wrong, without repeating the field name
}

// Error implements error.
func (e *FieldError) Error() string {
	return "provider " + e.Provider + ": " + e.Field + ": " + e.Problem
}

// ValidationError collects the field errors of one Validate or FromLegacy
// call. Errors are ordered deterministically — a fixed check order, with map
// keys sorted — so tests and admin responses can compare them.
type ValidationError struct {
	Errors []*FieldError
}

// Error implements error.
func (e *ValidationError) Error() string {
	if len(e.Errors) == 1 {
		return "invalid provider config: " + e.Errors[0].Error()
	}
	return "invalid provider config: " + strconv.Itoa(len(e.Errors)) + " problems, first: " + e.Errors[0].Error()
}

// hopByHopHeaders are connection-scoped headers the transport owns; a provider
// configuration must not set them.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// Validate reports every problem in p, or nil. It canonicalizes the names in
// p.Headers to their canonical spelling before checking them.
func (p *Provider) Validate() error {
	if p == nil {
		return &ValidationError{Errors: []*FieldError{{Provider: "<nil>", Field: "id", Problem: "provider is nil"}}}
	}
	label := p.ID
	if label == "" {
		label = "<unnamed>"
	}
	var errs []*FieldError
	add := func(field, problem string) {
		errs = append(errs, &FieldError{Provider: label, Field: field, Problem: problem})
	}

	if strings.TrimSpace(p.ID) == "" {
		add("id", "must not be empty")
	}
	if strings.TrimSpace(p.Name) == "" {
		add("name", "must not be empty")
	}
	if _, err := ir.ParseProtocol(p.Protocol.String()); err != nil {
		add("protocol", err.Error())
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		add("base_url", err.Error())
	}
	switch p.TLS {
	case "", TLSDefault, TLSChrome:
	default:
		add("tls", `must be "default" or "chrome"`)
	}
	p.validateAuth(add)
	p.validateHeaders(add)
	p.validateModels(add)
	p.validateTimeouts(add)

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

func (p *Provider) validateAuth(add func(field, problem string)) {
	switch p.Auth.Scheme {
	case "", AuthNone:
		if p.Auth.Key != "" || p.Auth.KeyEnv != "" {
			add("auth.key", "must be empty when auth.scheme is none")
		}
	case AuthBearer, AuthAPIKey:
		switch {
		case p.Auth.Key != "" && p.Auth.KeyEnv != "":
			add("auth.key", "set either auth.key or auth.key_env, not both")
		case p.Auth.Key == "" && p.Auth.KeyEnv == "":
			add("auth.key", "required when auth.scheme is "+string(p.Auth.Scheme))
		}
	default:
		add("auth.scheme", `must be "none", "bearer" or "x-api-key"`)
	}
}

func (p *Provider) validateHeaders(add func(field, problem string)) {
	if len(p.Headers) == 0 {
		return
	}
	canonical := make(map[string]string, len(p.Headers))
	dupes := make(map[string]bool, len(p.Headers))
	for name, value := range p.Headers {
		name = textproto.CanonicalMIMEHeaderKey(name)
		if _, dup := canonical[name]; dup {
			dupes[name] = true
			continue
		}
		canonical[name] = value
	}
	p.Headers = canonical

	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := "headers[" + name + "]"
		switch {
		case !httpguts.ValidHeaderFieldName(name):
			add(field, "invalid header name")
		case hopByHopHeaders[name]:
			add(field, "hop-by-hop headers belong to the transport, not to a provider")
		case dupes[name]:
			add(field, "set more than once under different spellings")
		case !httpguts.ValidHeaderFieldValue(canonical[name]):
			add(field, "invalid header value")
		}
	}
}

func (p *Provider) validateModels(add func(field, problem string)) {
	seen := make(map[string]int, len(p.Models))
	for i, m := range p.Models {
		field := "models[" + strconv.Itoa(i) + "]"
		switch {
		case strings.TrimSpace(m.Alias) == "":
			add(field+".alias", "must not be empty")
		case strings.ContainsAny(m.Alias, " \t\r\n"):
			add(field+".alias", "must not contain whitespace")
		}
		if strings.Contains(m.Alias, "*") && (!strings.HasSuffix(m.Alias, "/*") || m.Alias == "/*") {
			add(field+".alias", `may only use a trailing "/*" wildcard, after a prefix`)
		}
		if strings.TrimSpace(m.UpstreamModel) == "" && !strings.HasSuffix(m.Alias, "/*") {
			add(field+".upstream_model", "must not be empty")
		}
		if first, dup := seen[m.Alias]; m.Alias != "" && dup {
			add(field+".alias", "duplicates models["+strconv.Itoa(first)+"].alias")
		} else if m.Alias != "" {
			seen[m.Alias] = i
		}
	}
}

func (p *Provider) validateTimeouts(add func(field, problem string)) {
	for _, t := range []struct {
		field string
		value time.Duration
	}{
		{"timeouts.connect", p.Timeouts.Connect},
		{"timeouts.response_header", p.Timeouts.ResponseHeader},
		{"timeouts.idle", p.Timeouts.Idle},
	} {
		if t.value < 0 {
			add(t.field, "must not be negative")
		}
	}
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("must use http or https, not %q", u.Scheme)
	case u.Host == "":
		return errors.New("must include a host")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("must not include a query or fragment")
	}
	return nil
}
