package dispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

type cachedTranslation struct {
	provider *provider.Provider
	model    string
	handler  *translation
}

type translation struct {
	provider provider.Provider
	model    string
	outbound adapter.Outbound
	policy   provider.URLPolicy
	client   *http.Client
	recorder core.Recorder
}

func (d *Dispatcher) translator(model string, route routing.Route, base adapter.Outbound) (*translation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cached, found := d.translations[model]; found && cached.provider == route.Provider && cached.model == route.UpstreamModel {
		return cached.handler, nil
	}
	handler, err := newTranslation(route.Provider, route.UpstreamModel, base, &d.options)
	if err != nil {
		return nil, err
	}
	d.translations[model] = cachedTranslation{provider: route.Provider, model: route.UpstreamModel, handler: handler}
	return handler, nil
}

func (d *Dispatcher) failTranslation(c *gin.Context, protocol ir.Protocol, route routing.Route, request *core.RequestContext, stream bool, err error) {
	request.ResolvedProvider = route.Provider.ID
	request.UpstreamModel = route.UpstreamModel
	request.UpstreamProtocol = route.UpstreamProtocol
	request.Stream = stream
	ctx, cancel := context.WithCancel(c.Request.Context())
	request.BeginRecording(ctx, d.options.Recorder)
	writeTranslationError(c, protocol, err)
	request.MarkFirstByte()
	request.Complete(ir.Usage{}, completionError(err))
	cancel()
}

func newTranslation(p *provider.Provider, upstreamModel string, base adapter.Outbound, options *relay.Options) (*translation, error) {
	if p == nil {
		return nil, errors.New("dispatch: translation provider is required")
	}
	if options == nil {
		options = &relay.Options{}
	}
	policy := provider.DefaultURLPolicy()
	if options.Policy != nil {
		policy = *options.Policy
	}
	snapshot := provider.Clone(p)
	redact.Default.Update(snapshot.Auth.Key)
	if err := snapshot.ValidateWithPolicy(policy); err != nil {
		return nil, err
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, errors.New("dispatch: resolved upstream model is required")
	}
	configured, err := outbound.Configure(base, &snapshot)
	if err != nil {
		return nil, err
	}
	proxy := options.ProxyURL
	if snapshot.ProxyURL != "" {
		proxy = snapshot.ProxyURL
	}
	var proxyURL *url.URL
	if proxy != "" {
		proxyURL, err = url.Parse(proxy)
	} else {
		origin, parseErr := url.Parse(snapshot.BaseURL)
		if parseErr != nil {
			return nil, parseErr
		}
		proxyURL, err = http.ProxyFromEnvironment(&http.Request{URL: origin})
	}
	if err != nil {
		return nil, errors.New("dispatch: invalid upstream proxy configuration")
	}
	clientOptions := &upstream.Options{
		Timeouts: options.Timeouts, ProxyURL: proxy,
		Control: policy.Control, DialContext: policy.Dialer(proxyURL, net.DefaultResolver, nil),
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
	return &translation{provider: snapshot, model: upstreamModel, outbound: configured, policy: policy, client: client, recorder: options.Recorder}, nil
}

func (h *translation) Serve(c *gin.Context, body []byte, inbound adapter.Inbound, request *core.RequestContext) {
	c.Abort()
	request.ResolvedProvider = h.provider.ID
	request.UpstreamModel = h.model
	request.UpstreamProtocol = h.provider.Protocol
	request.Stream = false
	ctx, cancel := context.WithCancel(c.Request.Context())
	request.BeginRecording(ctx, h.recorder)
	var usage ir.Usage
	var terminal error
	defer func() {
		request.Complete(usage, completionError(terminal))
		cancel()
	}()

	decodedRequest := c.Request.Clone(ctx)
	decodedRequest.Body = io.NopCloser(bytes.NewReader(body))
	decodedRequest.ContentLength = int64(len(body))
	normalized, err := inbound.DecodeRequest(decodedRequest)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if normalized == nil {
		terminal = errors.New("dispatch: inbound adapter returned a nil request")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	if normalized.Stream {
		terminal = &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "streaming translation is not enabled"}
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}

	headerRequest := c.Request.Clone(ctx)
	headerRequest.Body = http.NoBody
	headerRequest.Header = make(http.Header)
	if err := core.ApplyHeaders(headerRequest, c.Request, &h.provider); err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	headerRequest.Header.Set("Accept-Encoding", "identity")
	credentialHeader := "Authorization"
	if h.provider.Auth.Scheme == provider.AuthAPIKey {
		credentialHeader = "X-Api-Key"
	}
	redact.Default.Update(strings.TrimPrefix(headerRequest.Header.Get(credentialHeader), "Bearer "))
	target := adapter.Target{BaseURL: h.provider.BaseURL, Model: h.model, Headers: headerRequest.Header}
	outgoing, err := h.outbound.BuildRequest(ctx, normalized, target)
	if err != nil {
		if outgoing != nil && outgoing.Body != nil {
			err = errors.Join(err, outgoing.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if outgoing == nil {
		terminal = errors.New("dispatch: outbound adapter returned a nil request")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	if outgoing.Context() != ctx {
		outgoing = outgoing.WithContext(ctx)
	}
	if err := h.policy.ValidateRequest(outgoing); err != nil {
		if outgoing.Body != nil {
			err = errors.Join(err, outgoing.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	response, err := h.client.Do(outgoing)
	if err != nil {
		if response != nil && response.Body != nil {
			err = errors.Join(err, response.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if response == nil || response.Body == nil {
		terminal = errors.New("dispatch: upstream returned a response without a body")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	response.Body = &firstByteBody{ReadCloser: response.Body, request: request}
	normalizedResponse, parseErr := h.outbound.ParseResponse(response)
	closeErr := response.Body.Close()
	if parseErr != nil || closeErr != nil {
		terminal = errors.Join(parseErr, closeErr)
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	if normalizedResponse == nil {
		terminal = errors.New("dispatch: outbound adapter returned a nil response")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	if err := ctx.Err(); err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	request.MarkFirstByte()
	normalizedResponse.Model = request.ClientModel
	usage = normalizedResponse.Usage
	encoded, err := inbound.EncodeResponse(normalizedResponse)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	n, err := c.Writer.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	terminal = err
}

type firstByteBody struct {
	io.ReadCloser
	request *core.RequestContext
	marked  bool
}

func (b *firstByteBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.marked {
		b.marked = true
		b.request.MarkFirstByte()
	}
	return n, err
}

type recordedError struct {
	cause   error
	message string
}

func (e *recordedError) Error() string { return e.message }
func (e *recordedError) Unwrap() error { return e.cause }

func completionError(err error) error {
	if err == nil {
		return nil
	}
	return &recordedError{cause: err, message: redact.Default.Text(err.Error())}
}

func writeTranslationError(c *gin.Context, protocol ir.Protocol, err error) {
	var normalized *gwerr.Error
	if !errors.As(err, &normalized) {
		normalized = &gwerr.Error{Kind: gwerr.KindFromErr(err), Message: redact.Default.Text(err.Error())}
	}
	writeDispatchError(c, protocol, normalized)
}
