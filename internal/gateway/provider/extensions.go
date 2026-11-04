package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"sort"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"golang.org/x/net/http/httpguts"
)

// ParseProtocol accepts standard wire protocols and outbound-only custom names.
// Custom names are case-sensitive lowercase ASCII identifiers: [a-z0-9] followed
// by zero or more [a-z0-9._-]. It does not expand ir.ParseProtocol's inbound set
// or imply that a named adapter has been registered in a particular registry.
func ParseProtocol(raw string) (ir.Protocol, error) {
	if name, custom := strings.CutPrefix(raw, "custom:"); custom {
		for i, c := range name {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_' || c == '.') {
				continue
			}
			return "", errors.New("custom adapter name must match [a-z0-9][a-z0-9._-]*")
		}
		if name == "" {
			return "", errors.New("custom adapter name must not be empty")
		}
		return ir.Protocol(raw), nil
	}
	return ir.ParseProtocol(raw)
}

// ValidateEndpointPath accepts an absolute, unescaped endpoint suffix, not a
// URL reference. Queries, fragments, escapes, empty/dot segments, whitespace,
// backslashes and control characters are rejected so neither URL resolution
// nor an upstream's path decoding can escape a configured base path prefix.
func ValidateEndpointPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("must be an absolute path beginning with one slash")
	}
	for _, c := range path {
		if c <= ' ' || c >= 127 || strings.ContainsRune("?#\\%", c) {
			return errors.New("must not contain escapes, query, fragment, whitespace or non-ASCII characters")
		}
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "." || segment == ".." || segment == "" {
			return errors.New("must not contain empty or dot path segments")
		}
	}
	return nil
}

// ValidateAuthHeaderName checks a credential destination independently of key
// resolution. Transport, routing and content-negotiation headers cannot be used
// as credential destinations. Empty means the scheme's normal header.
func ValidateAuthHeaderName(scheme AuthScheme, name string) error {
	if name == "" {
		return nil
	}
	if scheme != AuthBearer && scheme != AuthAPIKey {
		return errors.New("requires bearer or x-api-key auth")
	}
	if !httpguts.ValidHeaderFieldName(name) {
		return errors.New("invalid header name")
	}
	name = textproto.CanonicalMIMEHeaderKey(name)
	if hopByHopHeaders[name] {
		return errors.New("must not use a hop-by-hop header")
	}
	switch name {
	case "Host", "Content-Length", "Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "Expect", "Range":
		return errors.New("must not use a transport, routing or content-negotiation header")
	}
	return nil
}

// DecodeExtraBody decodes an optional top-level JSON object without converting
// numbers to float64. model and stream remain authoritative adapter fields and
// cannot be supplied even when their configured values would be identical.
func DecodeExtraBody(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("must be a valid JSON object")
	}
	if fields == nil {
		return nil, errors.New("must be a JSON object, not null")
	}
	for _, name := range []string{"model", "stream"} {
		if _, found := fields[name]; found {
			return nil, fmt.Errorf("must not override %s", name)
		}
	}
	return fields, nil
}

func (p *Provider) validateExtensions(add func(field, problem string)) {
	protocols := make([]string, 0, len(p.EndpointPaths))
	for protocol := range p.EndpointPaths {
		protocols = append(protocols, protocol.String())
	}
	sort.Strings(protocols)
	for _, protocol := range protocols {
		field := "endpoint_path[" + protocol + "]"
		if _, err := ir.ParseProtocol(protocol); err != nil {
			add(field, "key must be a standard wire protocol")
		} else if err := ValidateEndpointPath(p.EndpointPaths[ir.Protocol(protocol)]); err != nil {
			add(field, err.Error())
		}
	}
	if _, err := DecodeExtraBody(p.ExtraBody); err != nil {
		add("extra_body", err.Error())
	}
	if err := ValidateAuthHeaderName(p.Auth.Scheme, p.Auth.HeaderName); err != nil {
		add("auth.header_name", err.Error())
	} else {
		p.Auth.HeaderName = textproto.CanonicalMIMEHeaderKey(p.Auth.HeaderName)
	}
}
