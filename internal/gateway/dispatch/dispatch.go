// Package dispatch selects same-protocol relays and registered non-streaming
// translations. NVIDIA, streaming and unregistered traffic retain legacy flow.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gwpolicy "github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/reliability"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/tokencount"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

type cachedRelay struct {
	provider *provider.Provider
	model    string
	handler  *relay.Relay
}

// Dispatcher reuses relays and translations for configured aliases, not
// arbitrary request models. Set Registry before serving; nil or missing
// adapters preserve the legacy fallthrough path. Call Reset after a provider
// catalog update to retire removed aliases and cached clients.
// Resolver snapshots, Registry and options must remain immutable while used.
type Dispatcher struct {
	// Registry is optional so construction remains compatible with the legacy
	// dispatcher. Assign it before serving requests; do not replace it live.
	Registry *adapter.Registry
	// Limiter bounds concurrent upstream calls per provider.
	Limiter *reliability.Limiter

	// FeaturePolicy configures capability validation and unsupported parameter handling.
	FeaturePolicy gwpolicy.Config

	resolver     routing.Resolver
	options      relay.Options
	mu           sync.Mutex
	handlers     map[string]cachedRelay
	translations map[string]cachedTranslation
}

func New(resolver routing.Resolver, options *relay.Options) *Dispatcher {
	if resolver == nil {
		panic("dispatch: resolver is required")
	}
	d := &Dispatcher{
		resolver:     resolver,
		handlers:     make(map[string]cachedRelay),
		translations: make(map[string]cachedTranslation),
		Limiter:      reliability.NewLimiter(),
	}
	if options != nil {
		d.options = *options
	}
	if d.options.Factory == nil {
		d.options.Factory = upstream.NewFactory()
	}
	return d
}

// Reset retires cached configuration without aborting in-flight requests.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, entry := range d.handlers {
		d.options.Factory.Invalidate(entry.provider.ID)
	}
	for _, entry := range d.translations {
		d.options.Factory.Invalidate(entry.provider.ID)
	}
	clear(d.handlers)
	clear(d.translations)
}

func routeProtocol(path string) ir.Protocol {
	switch path {
	case "/v1/chat/completions":
		return ir.ProtocolOpenAIChat
	case "/v1/messages", "/v1/messages/count_tokens":
		return ir.ProtocolAnthropicMessages
	case "/v1/responses":
		return ir.ProtocolOpenAIResponses
	default:
		return ""
	}
}

func restoreAndContinue(c *gin.Context, body []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Next()
}

func requestedStream(raw json.RawMessage) bool {
	var stream bool
	return len(raw) != 0 && json.Unmarshal(raw, &stream) == nil && stream
}

func (d *Dispatcher) featurePolicyConfig() gwpolicy.Config {
	cfg := d.options.FeaturePolicy
	if d.FeaturePolicy.DefaultMode != "" {
		cfg.DefaultMode = d.FeaturePolicy.DefaultMode
	}
	if len(d.FeaturePolicy.ProviderModes) > 0 {
		if cfg.ProviderModes == nil {
			cfg.ProviderModes = make(map[string]gwpolicy.Mode)
		}
		for k, v := range d.FeaturePolicy.ProviderModes {
			cfg.ProviderModes[k] = v
		}
	}
	if len(d.FeaturePolicy.ProviderOverrides) > 0 {
		if cfg.ProviderOverrides == nil {
			cfg.ProviderOverrides = make(map[string]gwpolicy.CapabilityOverrides)
		}
		for k, v := range d.FeaturePolicy.ProviderOverrides {
			cfg.ProviderOverrides[k] = v
		}
	}
	return cfg
}

func (d *Dispatcher) handler(model string, route routing.Route) (*relay.Relay, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cached, found := d.handlers[model]; found && cached.provider == route.Provider && cached.model == route.UpstreamModel {
		return cached.handler, nil
	}
	handler, err := relay.New(route.Provider, route.UpstreamModel, &d.options)
	if err != nil {
		return nil, err
	}
	d.handlers[model] = cachedRelay{provider: route.Provider, model: route.UpstreamModel, handler: handler}
	return handler, nil
}

func writeDispatchError(c *gin.Context, protocol ir.Protocol, err error) {
	c.Abort()
	var normalized *gwerr.Error
	if !errors.As(err, &normalized) {
		normalized = &gwerr.Error{Kind: gwerr.KindFromErr(err), Message: err.Error()}
	}
	if writeErr := gwerr.Write(c.Writer, gwerr.Protocol(protocol), normalized); writeErr != nil {
		log.Printf("gateway response write failed: %v", writeErr)
	}
}

// Handle reads the original client body once. Owned traffic uses ServeBuffered;
// fallthrough gets a reader over those same bytes, without JSON re-encoding.
func (d *Dispatcher) Handle(c *gin.Context) {
	protocol := routeProtocol(c.Request.URL.Path)
	if c.Request.Method != http.MethodPost || protocol == "" {
		c.Next()
		return
	}
	rc := core.RequestContextFrom(c.Request.Context())
	if rc == nil {
		rc = core.NewRequestContext(c.Request.Header.Get(core.RequestIDHeader))
		c.Request = c.Request.WithContext(core.WithRequestContext(c.Request.Context(), rc))
	}
	rc.InboundProtocol = protocol
	c.Header(core.RequestIDHeader, rc.ID)
	if err := c.Request.Context().Err(); err != nil {
		writeDispatchError(c, protocol, err)
		return
	}
	var body []byte
	var err error
	if c.Request.Body != nil {
		body, err = io.ReadAll(c.Request.Body)
	}
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeDispatchError(c, protocol, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: status, Message: "could not read request body"})
		return
	}
	var payload struct {
		Model  string          `json:"model"`
		Stream json.RawMessage `json:"stream"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
		restoreAndContinue(c, body)
		return
	}
	if rc.ClientModel == "" {
		rc.ClientModel = payload.Model
	}
	route, err := d.resolver.Resolve(c.Request.Context(), payload.Model, protocol)
	if err != nil {
		writeDispatchError(c, protocol, err)
		return
	}
	if route.Kind == routing.Fallthrough {
		restoreAndContinue(c, body)
		return
	}
	if route.Kind != routing.Custom || route.Provider == nil || route.UpstreamProtocol != route.Provider.Protocol {
		writeDispatchError(c, protocol, &gwerr.Error{Kind: gwerr.KindInternal, Message: "resolver returned an invalid route"})
		return
	}
	if d.Limiter != nil {
		release, acquireErr := d.Limiter.Acquire(c.Request.Context(), route.Provider)
		if acquireErr != nil {
			writeDispatchError(c, protocol, acquireErr)
			return
		}
		defer release()
	}
	if route.UpstreamProtocol == protocol {
		handler, err := d.handler(payload.Model, route)
		if err != nil {
			writeDispatchError(c, protocol, err)
			return
		}
		c.Abort()
		handler.ServeBuffered(c.Writer, c.Request, body)
		return
	}
	if c.Request.URL.Path == "/v1/messages/count_tokens" {
		d.serveCountTokensEstimate(c, body, route, rc)
		return
	}
	if d.Registry == nil {
		restoreAndContinue(c, body)
		return
	}
	inbound, inboundOK := d.Registry.Inbound(protocol)
	base, outboundOK := d.Registry.Outbound(route.Provider.Protocol)
	if !inboundOK || !outboundOK {
		restoreAndContinue(c, body)
		return
	}
	isStream := requestedStream(payload.Stream)
	if isStream && !base.Capabilities().Streaming {
		restoreAndContinue(c, body)
		return
	}
	handler, err := d.translator(payload.Model, route, base)
	if err != nil {
		d.failTranslation(c, protocol, route, rc, isStream, err)
		return
	}
	if isStream {
		handler.ServeStream(c, body, inbound, rc)
		return
	}
	handler.Serve(c, body, inbound, rc)
}

func (d *Dispatcher) serveCountTokensEstimate(c *gin.Context, body []byte, route routing.Route, rc *core.RequestContext) {
	c.Abort()
	protocol := rc.InboundProtocol
	rc.ResolvedProvider = route.Provider.ID
	rc.UpstreamModel = route.UpstreamModel
	rc.UpstreamProtocol = route.Provider.Protocol
	rc.Stream = false
	ctx, cancel := context.WithCancel(c.Request.Context())
	rc.BeginRecording(ctx, d.options.Recorder)
	var usage ir.Usage
	var terminal error
	defer func() {
		rc.Complete(usage, completionError(terminal))
		cancel()
	}()

	var inbound adapter.Inbound
	if d.Registry != nil {
		if registered, ok := d.Registry.Inbound(protocol); ok {
			inbound = registered
		}
	}
	if inbound == nil {
		inbound = inboundanthropic.New()
	}

	decodedRequest := c.Request.Clone(ctx)
	decodedRequest.Body = io.NopCloser(bytes.NewReader(body))
	decodedRequest.ContentLength = int64(len(body))
	normalized, err := inbound.DecodeRequest(decodedRequest)
	if err != nil {
		terminal = err
		writeTranslationError(c, protocol, err)
		return
	}
	if normalized == nil {
		terminal = errors.New("dispatch: inbound adapter returned a nil request")
		writeTranslationError(c, protocol, terminal)
		return
	}

	tokens, err := tokencount.EstimateRequest(body)
	if err != nil {
		terminal = err
		writeTranslationError(c, protocol, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: "invalid json body"})
		return
	}

	usage.InputTokens = tokens
	c.Header("X-Gateway-Token-Estimate", "true")
	c.JSON(http.StatusOK, gin.H{"input_tokens": tokens})
}
