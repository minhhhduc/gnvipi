// Package dispatch selects same-protocol custom-provider relays. NVIDIA and
// cross-protocol requests retain their original bodies for the legacy chain.
package dispatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

type cachedRelay struct {
	provider *provider.Provider
	model    string
	handler  *relay.Relay
}

// Dispatcher reuses relays for configured aliases, not arbitrary request models.
// Call Reset after a provider catalog update to retire removed aliases/clients.
// Resolver snapshots and options must remain immutable while being consumed.
type Dispatcher struct {
	resolver routing.Resolver
	options  relay.Options
	mu       sync.Mutex
	handlers map[string]cachedRelay
}

func New(resolver routing.Resolver, options *relay.Options) *Dispatcher {
	if resolver == nil {
		panic("dispatch: resolver is required")
	}
	d := &Dispatcher{resolver: resolver, handlers: make(map[string]cachedRelay)}
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
	clear(d.handlers)
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
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Next()
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
	if route.Kind == routing.Fallthrough || route.Kind == routing.Custom && route.UpstreamProtocol != protocol {
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Next()
		return
	}
	if route.Kind != routing.Custom || route.Provider == nil {
		writeDispatchError(c, protocol, &gwerr.Error{Kind: gwerr.KindInternal, Message: "resolver returned an invalid route"})
		return
	}
	handler, err := d.handler(payload.Model, route)
	if err != nil {
		writeDispatchError(c, protocol, err)
		return
	}
	c.Abort()
	handler.ServeBuffered(c.Writer, c.Request, body)
}
