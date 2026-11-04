package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"golang.org/x/net/http/httpguts"
)

// Configure snapshots p's declarative outbound extensions around base. It does
// not resolve credentials, choose a transport, or change the legacy dispatcher.
// Endpoint suffixes append to Target.BaseURL's existing path prefix; they never
// replace its origin. ExtraBody is a shallow JSON merge: configured fields win
// collisions except model and stream, which are forbidden. Objects/arrays are
// replaced as whole values, null is a value, and numbers keep their precision.
// Auth.HeaderName moves the exact credential value already in Target.Headers,
// overriding a stale destination header without modifying the caller's map.
// BuildRequest fails rather than using p.Auth.Key/KeyEnv when auth is unresolved.
//
// Custom registrations retain their base wire protocol for EndpointPaths lookup.
// Configure validates extension fields even if p.Validate was not called; full
// provider validation and resolved Target construction remain the relay's job.
func Configure(base adapter.Outbound, p *provider.Provider) (adapter.Outbound, error) {
	if p == nil {
		return nil, errors.New("outbound: provider configuration is nil")
	}
	if base == nil {
		return nil, errors.New("outbound: nil base adapter")
	}
	for protocol, path := range p.EndpointPaths {
		if _, err := ir.ParseProtocol(protocol.String()); err != nil {
			return nil, fmt.Errorf("outbound: endpoint_path: %w", err)
		}
		if err := provider.ValidateEndpointPath(path); err != nil {
			return nil, fmt.Errorf("outbound: endpoint_path[%s]: %w", protocol, err)
		}
	}
	extra, err := provider.DecodeExtraBody(p.ExtraBody)
	if err != nil {
		return nil, fmt.Errorf("outbound: extra_body: %w", err)
	}
	if err := provider.ValidateAuthHeaderName(p.Auth.Scheme, p.Auth.HeaderName); err != nil {
		return nil, fmt.Errorf("outbound: auth.header_name: %w", err)
	}
	path := p.EndpointPaths[wireProtocol(base)]
	if path == "" && len(extra) == 0 && p.Auth.HeaderName == "" {
		return base, nil
	}
	return &declarative{Outbound: base, authScheme: p.Auth.Scheme, authHeader: textproto.CanonicalMIMEHeaderKey(p.Auth.HeaderName), endpoint: path, extra: extra}, nil
}

type declarative struct {
	adapter.Outbound
	authScheme provider.AuthScheme
	authHeader string
	endpoint   string
	extra      map[string]json.RawMessage
}

func (d *declarative) BuildRequest(ctx context.Context, req *ir.Request, target adapter.Target) (*http.Request, error) {
	if d.authHeader != "" {
		headers, err := moveAuth(target.Headers, d.authScheme, d.authHeader)
		if err != nil {
			return nil, err
		}
		target.Headers = headers
	}
	r, err := d.Outbound.BuildRequest(ctx, req, target)
	if err != nil {
		return nil, errors.Join(err, closeRequest(r))
	}
	if r == nil {
		return nil, errors.New("outbound: base returned a nil request")
	}
	if d.endpoint != "" {
		u, err := endpointURL(target.BaseURL, d.endpoint)
		if err != nil {
			return nil, errors.Join(err, closeRequest(r))
		}
		r.URL = u
		// The URL, not a stale Host override, determines upstream routing.
		r.Host = ""
	}
	if err := mergeExtraBody(r, d.extra); err != nil {
		return nil, errors.Join(err, closeRequest(r))
	}
	return r, nil
}

func endpointURL(raw, endpoint string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("outbound: target base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("outbound: target must be an HTTP(S) base URL without credentials, query or fragment")
	}
	prefix := strings.TrimRight(u.EscapedPath(), "/")
	if prefix != "" {
		if err := provider.ValidateEndpointPath(prefix); err != nil {
			return nil, fmt.Errorf("outbound: target base path: %w", err)
		}
	}
	u.Path = prefix + endpoint
	u.RawPath = ""
	return u, nil
}

func moveAuth(headers http.Header, scheme provider.AuthScheme, destination string) (http.Header, error) {
	source := "Authorization"
	if scheme == provider.AuthAPIKey {
		source = "X-Api-Key"
	}
	var values []string
	found := false
	for name, candidate := range headers {
		if !strings.EqualFold(name, source) {
			continue
		}
		if found {
			return nil, errors.New("outbound: resolved auth has ambiguous header spellings")
		}
		found, values = true, candidate
	}
	if !found || len(values) != 1 || values[0] == "" || !httpguts.ValidHeaderFieldValue(values[0]) {
		return nil, errors.New("outbound: expected one valid already-resolved auth header value")
	}
	out := headers.Clone()
	for name := range out {
		if strings.EqualFold(name, source) || strings.EqualFold(name, destination) {
			delete(out, name)
		}
	}
	out.Set(destination, values[0])
	return out, nil
}

func mergeExtraBody(req *http.Request, extra map[string]json.RawMessage) error {
	if len(extra) == 0 {
		return nil
	}
	if req.Body == nil {
		return errors.New("outbound: extra_body requires a JSON request body")
	}
	if req.Header.Get("Content-Encoding") != "" {
		return errors.New("outbound: extra_body cannot merge an encoded request body")
	}
	body, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	req.Body = http.NoBody
	if err != nil {
		return fmt.Errorf("outbound: read JSON request: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("outbound: close JSON request: %w", closeErr)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return errors.New("outbound: extra_body requires a JSON object request")
	}
	for name, value := range extra {
		fields[name] = value
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("outbound: merge JSON request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(merged))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(merged)), nil }
	req.ContentLength = int64(len(merged))
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	return nil
}

func wireProtocol(a adapter.Outbound) ir.Protocol {
	if a, ok := a.(interface{ wireProtocol() ir.Protocol }); ok {
		return a.wireProtocol()
	}
	return a.Protocol()
}
func (w *wrapped) wireProtocol() ir.Protocol     { return wireProtocol(w.Outbound) }
func (d *declarative) wireProtocol() ir.Protocol { return wireProtocol(d.Outbound) }
