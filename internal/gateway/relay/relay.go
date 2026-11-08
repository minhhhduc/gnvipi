// Package relay forwards a resolved request to an upstream speaking the same
// wire protocol. It performs no registration, translation, or retries.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gwpolicy "github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

// Options supplies the relay's client lifecycle, default bounds and recorder.
// Nil Factory uses the shared upstream factory; nil Policy snapshots the process
// URL policy; nil Recorder selects core.DefaultRecorder. Provider timeouts and
// proxy settings override these defaults. An empty proxy uses ProxyFromEnvironment.
// The relay always installs the policy's dial and redirect guards.
type Options struct {
	Factory       *upstream.Factory
	Timeouts      provider.Timeouts
	ProxyURL      string
	Policy        *provider.URLPolicy
	Recorder      core.Recorder
	FeaturePolicy gwpolicy.Config
}

// Relay is an http.Handler for one immutable provider and resolved upstream model.
// Reuse it across requests; create a replacement when provider configuration or
// URL policy changes. New snapshots the provider and policy, not the recorder.
// Routing, alias resolution, body size limits and authentication belong to the
// dispatcher. An existing core.RequestContext retains its original ClientModel.
type Relay struct {
	provider provider.Provider
	model    string
	path     string
	target   string
	policy   provider.URLPolicy
	client   *http.Client
	recorder core.Recorder
}

// New builds a same-protocol handler without registering it. upstreamModel must
// be the dispatcher's resolved model, including any wildcard alias expansion.
// A nil options pointer selects the ordinary defaults.
func New(p *provider.Provider, upstreamModel string, options *Options) (*Relay, error) {
	if options == nil {
		options = &Options{}
	}
	policy := provider.DefaultURLPolicy()
	if options.Policy != nil {
		policy = *options.Policy
	}
	if p == nil {
		return nil, errors.New("relay: provider is required")
	}
	snapshot := provider.Clone(p)
	if err := snapshot.ValidateWithPolicy(policy); err != nil {
		return nil, err
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, errors.New("relay: resolved upstream model is required")
	}
	var suffix string
	switch snapshot.Protocol {
	case ir.ProtocolAnthropicMessages:
		suffix = "/messages"
	case ir.ProtocolOpenAIChat:
		suffix = "/chat/completions"
	case ir.ProtocolOpenAIResponses:
		suffix = "/responses"
	default:
		return nil, errors.New("relay: provider must use a supported inbound wire protocol")
	}
	target := providerEndpoint(snapshot.BaseURL, suffix)
	if configured := snapshot.EndpointPaths[snapshot.Protocol]; configured != "" {
		// Declarative endpoint suffixes append to the configured base path, as
		// outbound.Configure does; they never replace the provider origin.
		target = strings.TrimRight(snapshot.BaseURL, "/") + configured
	}
	if err := policy.Validate(target); err != nil {
		return nil, err
	}
	proxy := options.ProxyURL
	if snapshot.ProxyURL != "" {
		proxy = snapshot.ProxyURL
	}
	var proxyURL *url.URL
	var err error
	if proxy != "" {
		proxyURL, err = url.Parse(proxy)
	} else {
		origin, parseErr := url.Parse(target)
		if parseErr != nil {
			return nil, parseErr
		}
		proxyURL, err = http.ProxyFromEnvironment(&http.Request{URL: origin})
	}
	if err != nil {
		return nil, errors.New("relay: invalid upstream proxy configuration")
	}
	clientOptions := &upstream.Options{
		Timeouts: options.Timeouts, ProxyURL: proxy,
		Control:      policy.Control,
		DialContext:  policy.Dialer(proxyURL, net.DefaultResolver, nil),
		DisableProxy: true, CheckRedirect: policy.CheckRedirect,
	}
	var client *http.Client
	if options.Factory == nil {
		client, err = upstream.NewClient(&snapshot, clientOptions)
	} else {
		client, err = options.Factory.NewClient(&snapshot, clientOptions)
	}
	if err != nil {
		return nil, err
	}
	redact.Default.Update(snapshot.Auth.Key)
	return &Relay{provider: snapshot, model: upstreamModel, path: "/v1" + suffix,
		target: target, policy: policy, client: client, recorder: options.Recorder}, nil
}

// providerEndpoint keeps the existing passthrough convention: a bare base gets
// /v1, a /v1 base gets the suffix, and a full endpoint is not appended twice.
func providerEndpoint(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + suffix
	}
	return base + "/v1" + suffix
}

// ServeHTTP forwards only this protocol's POST endpoint (including Anthropic
// count_tokens). It consumes the request body; dispatch must select the relay
// before calling it. Successful response bytes are not translated or redacted.
func (h *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveHTTP(w, r, nil, false)
}

// ServeBuffered relays a body already read by the dispatcher. The caller must
// retain the bytes unchanged until return; no second body-sized copy is made.
func (h *Relay) ServeBuffered(w http.ResponseWriter, r *http.Request, body []byte) {
	h.serveHTTP(w, r, body, true)
}

func (h *Relay) serveHTTP(w http.ResponseWriter, r *http.Request, body []byte, buffered bool) {
	rc := core.RequestContextFrom(r.Context())
	if rc == nil {
		rc = core.NewRequestContext(r.Header.Get(core.RequestIDHeader))
		r = r.WithContext(core.WithRequestContext(r.Context(), rc))
	}
	w.Header().Set(core.RequestIDHeader, rc.ID)
	countTokens := h.provider.Protocol == ir.ProtocolAnthropicMessages && r.URL.Path == h.path+"/count_tokens"
	var payload map[string]json.RawMessage
	var model string
	var streamed bool
	var requestErr *gwerr.Error
	switch {
	case r.Method != http.MethodPost || r.URL.Path != h.path && !countTokens:
		requestErr = &gwerr.Error{Kind: gwerr.KindNotFound, Message: "endpoint is not served by this relay"}
	case rc.InboundProtocol != "" && rc.InboundProtocol != h.provider.Protocol:
		requestErr = &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "same-protocol relay cannot translate this request"}
	default:
		var readErr error
		if !buffered && r.Body != nil {
			body, readErr = io.ReadAll(r.Body)
		}
		switch {
		case readErr != nil:
			status := http.StatusBadRequest
			var tooLarge *http.MaxBytesError
			if errors.As(readErr, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			requestErr = &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: status, Message: "could not read request body"}
		case json.Unmarshal(body, &payload) != nil || payload == nil || json.Unmarshal(payload["model"], &model) != nil || strings.TrimSpace(model) == "":
			requestErr = &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "request must be a JSON object with a model string"}
		default:
			if err := json.Unmarshal(payload["stream"], &streamed); err != nil {
				streamed = false
			}
			streamed = streamed && !countTokens
		}
	}
	rc.InboundProtocol, rc.UpstreamProtocol = h.provider.Protocol, h.provider.Protocol
	rc.ResolvedProvider, rc.UpstreamModel, rc.Stream = h.provider.ID, h.model, streamed
	if rc.ClientModel == "" {
		rc.ClientModel = model
	}
	ctx, cancel := context.WithCancel(r.Context())
	if streamed {
		ctx = upstream.WithStream(ctx)
	}
	rc.BeginRecording(ctx, h.recorder)
	var usage ir.Usage
	var terminal error
	defer func() {
		if terminal != nil {
			terminal = &safeError{cause: terminal, message: redact.Default.Text(terminal.Error())}
		}
		rc.Complete(usage, terminal)
		cancel()
	}()
	if requestErr != nil {
		terminal = errors.Join(requestErr, gwerr.Write(w, gwerr.Protocol(h.provider.Protocol), requestErr))
		return
	}
	if model != h.model {
		// Identical to rewriteJSONModel's RawMessage convention: nested fields,
		// unknown extensions, nulls and large numeric values are never decoded
		// through float64, and only the top-level model value changes.
		modelJSON, err := json.Marshal(h.model)
		if err != nil {
			terminal = h.writeError(w, err)
			return
		}
		payload["model"] = modelJSON
		body, err = json.Marshal(payload)
		if err != nil {
			terminal = h.writeError(w, err)
			return
		}
	}
	target := h.target
	if countTokens {
		target = strings.TrimRight(target, "/") + "/count_tokens"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		err = core.ApplyHeaders(req, r, &h.provider)
	}
	if err == nil {
		// This transport-owned guard must survive client Connection filtering.
		// Otherwise net/http can silently decompress successful response bytes.
		req.Header.Set("Accept-Encoding", "identity")
		source := "Authorization"
		if h.provider.Auth.Scheme == provider.AuthAPIKey {
			source = "X-Api-Key"
		}
		credential := req.Header.Get(source)
		redact.Default.Update(strings.TrimPrefix(credential, "Bearer "))
		if h.provider.Auth.HeaderName != "" {
			req.Header.Del(source)
			req.Header.Set(h.provider.Auth.HeaderName, credential)
		}
		err = h.policy.ValidateRequest(req)
	}
	if err != nil {
		terminal = h.writeError(w, err)
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		terminal = h.writeError(w, err)
		return
	}
	defer func() {
		terminal = errors.Join(terminal, resp.Body.Close())
	}()
	usage, terminal = h.copyResponse(ctx, w, resp, rc)
}

func (h *Relay) writeError(w http.ResponseWriter, err error) error {
	e := &gwerr.Error{Kind: gwerr.KindFromErr(err), Message: "upstream: " + redact.Default.Text(err.Error())}
	return errors.Join(err, gwerr.Write(w, gwerr.Protocol(h.provider.Protocol), e))
}

type safeError struct {
	cause   error
	message string
}

func (e *safeError) Error() string { return e.message }
func (e *safeError) Unwrap() error { return e.cause }

func upstreamStatusError(resp *http.Response) error {
	return &gwerr.Error{Kind: gwerr.KindFromHTTPStatus(resp.StatusCode), UpstreamStatus: resp.StatusCode,
		Status: resp.StatusCode, Message: fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode),
		UpstreamRequestID: redact.Default.Text(resp.Header.Get(core.RequestIDHeader))}
}
