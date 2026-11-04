// Package upstream builds bounded, reusable provider HTTP clients. It does not
// replace the legacy passthrough or NVIDIA transports. Callers must close every
// response body and mark streaming requests with WithStream before Client.Do.
package upstream

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/netx"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// Options supplies defaults overridden by each provider's non-zero Timeouts,
// ProxyURL and TLS. Zero durations select dial 10s, TLS 10s, headers 120s,
// non-stream total 10m and stream idle 5m. Negative durations are rejected.
// Control is the net.Dialer socket hook, also used for proxy connections. A
// callback and its captured policy must remain immutable while cached clients
// use it; replace Options or call Factory.Invalidate when changing that policy.
// Options and providers must not be mutated concurrently with NewClient.
// Pointer arguments avoid copying provider maps/slices on cache hits.
type Options struct {
	provider.Timeouts
	ProxyURL string
	TLS      provider.TLSProfile
	Control  func(network, address string, c syscall.RawConn) error
	// DialContext overrides the native dialer. If it handles proxy tunneling,
	// DisableProxy prevents a second proxy layer. The callback is trusted.
	DialContext  func(context.Context, string, string) (net.Conn, error)
	DisableProxy bool
	// CheckRedirect must be immutable for the lifetime of cached clients.
	CheckRedirect func(*http.Request, []*http.Request) error
}

// RegisterFlags exposes options on an explicitly supplied flag set without
// enabling the factory in the legacy dispatcher. Existing non-zero defaults
// are retained; per-provider settings still take precedence after parsing.
func (o *Options) RegisterFlags(fs *flag.FlagSet) {
	applyDefaults(&o.Timeouts)
	if o.TLS == "" {
		o.TLS = provider.TLSDefault
	}
	fs.DurationVar(&o.Connect, "upstream-connect-timeout", o.Connect, "upstream TCP connect timeout")
	fs.DurationVar(&o.Timeouts.TLS, "upstream-tls-timeout", o.Timeouts.TLS, "upstream TLS handshake timeout")
	fs.DurationVar(&o.ResponseHeader, "upstream-response-header-timeout", o.ResponseHeader, "upstream response-header timeout")
	fs.DurationVar(&o.Total, "upstream-total-timeout", o.Total, "non-stream upstream total timeout, including body")
	fs.DurationVar(&o.Idle, "upstream-idle-timeout", o.Idle, "maximum silence in an upstream stream")
	fs.StringVar(&o.ProxyURL, "upstream-proxy-url", o.ProxyURL, "upstream proxy URL; empty honors HTTP(S)_PROXY and NO_PROXY")
	fs.BoolVar(&o.DisableProxy, "upstream-disable-proxy", o.DisableProxy, "disable built-in environment/provider proxy handling")
	fs.Func("upstream-tls-profile", "upstream TLS profile: default or chrome", func(value string) error {
		profile := provider.TLSProfile(value)
		if profile != provider.TLSDefault && profile != provider.TLSChrome {
			return errors.New("TLS profile must be default or chrome")
		}
		o.TLS = profile
		return nil
	})
}

func applyDefaults(t *provider.Timeouts) {
	if t.Connect == 0 {
		t.Connect = 10 * time.Second
	}
	if t.TLS == 0 {
		t.TLS = 10 * time.Second
	}
	if t.ResponseHeader == 0 {
		t.ResponseHeader = 120 * time.Second
	}
	if t.Total == 0 {
		t.Total = 10 * time.Minute
	}
	if t.Idle == 0 {
		t.Idle = 5 * time.Minute
	}
}

type clientConfig struct {
	timeouts     provider.Timeouts
	proxyURL     string
	tls          provider.TLSProfile
	controlOwner *Options
	controlCode  uintptr
	dialCode     uintptr
	redirectCode uintptr
	disableProxy bool
}

type cacheEntry struct {
	provider *provider.Provider
	config   clientConfig
	client   *http.Client
}

// Factory owns clients keyed by provider ID. The zero value is ready to use.
// Configuration changes replace the client and close the old idle connections;
// existing callers may finish using the previous client. No retries are added.
type Factory struct {
	mu      sync.Mutex
	clients map[string]*cacheEntry
}

// NewFactory returns an independent client cache, useful for server lifetimes.
func NewFactory() *Factory { return &Factory{} }

var sharedFactory Factory

// NewClient returns a client from the process-wide factory. Nil options use the
// documented defaults. Prefer NewFactory when an explicit cache lifecycle is
// needed. The provider and options are not retained as mutable configuration.
func NewClient(p *provider.Provider, options *Options) (*http.Client, error) {
	return sharedFactory.NewClient(p, options)
}

// NewClient returns the cached client for p, invalidating it if configuration
// changed. Transport policy is immutable in the returned client; do not mutate
// its Transport, Timeout or redirect policy while shared by other callers.
func (f *Factory) NewClient(p *provider.Provider, options *Options) (*http.Client, error) {
	if p == nil || strings.TrimSpace(p.ID) == "" {
		return nil, errors.New("upstream provider must have an ID")
	}
	config, control, err := resolveConfig(p, options)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.clients[p.ID]; ok && entry.config == config && reflect.DeepEqual(entry.provider, p) {
		return entry.client, nil
	}
	proxy, err := proxyFunction(config.proxyURL)
	if err != nil {
		return nil, err
	}
	if config.disableProxy {
		proxy = nil
	}
	dialer := &net.Dialer{Timeout: config.timeouts.Connect, KeepAlive: 30 * time.Second, Control: control}
	dial := dialer.DialContext
	if options != nil && options.DialContext != nil {
		custom, limit := options.DialContext, config.timeouts.Connect
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			return custom(dialCtx, network, address)
		}
	}
	nativeProxy := proxy
	if proxy != nil {
		nativeProxy = func(req *http.Request) (*url.URL, error) {
			u, selected := req.Context().Value(proxyURLKey{}).(*url.URL)
			var err error
			if !selected {
				u, err = proxy(req)
			}
			if err == nil && u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
				// SOCKS is negotiated by DialContext with an independent bound,
				// not by net/http after its detached TCP dial has returned.
				return nil, nil
			}
			return u, err
		}
	}
	nativeDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if u, ok := ctx.Value(proxyURLKey{}).(*url.URL); ok && u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
			return netx.DialProxy(ctx, network, address, u, dial, config.timeouts.Connect, nil)
		}
		return dial(ctx, network, address)
	}
	standard := &http.Transport{
		Proxy:                 nativeProxy,
		DialContext:           nativeDial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   config.timeouts.TLS,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		ExpectContinueTimeout: time.Second,
	}
	var base http.RoundTripper = standard
	if config.tls == provider.TLSChrome {
		chrome := newChromeTransport(dialer, proxy, config.timeouts.TLS, standard)
		chrome.dial = dial // Chrome performs its own proxy negotiation.
		base = chrome
	}
	client := &http.Client{Transport: &timeoutTransport{base: base, timeouts: config.timeouts, proxy: proxy}}
	if options != nil {
		client.CheckRedirect = options.CheckRedirect
	}
	// Copy only on configuration changes. Cache lookups do not serialize or
	// copy provider credentials, maps or model slices.
	snapshot := provider.Clone(p)
	if old, ok := f.clients[p.ID]; ok {
		old.client.CloseIdleConnections()
	}
	if f.clients == nil {
		f.clients = make(map[string]*cacheEntry)
	}
	f.clients[p.ID] = &cacheEntry{provider: &snapshot, config: config, client: client}
	return client, nil
}

// Invalidate removes one cached provider and closes its idle connections. Use
// this when replacing a Control callback's captured policy without changing
// the provider or Options pointer.
func (f *Factory) Invalidate(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.clients[id]; ok {
		entry.client.CloseIdleConnections()
		delete(f.clients, id)
	}
}

// CloseIdleConnections closes pooled idle sockets without aborting requests.
func (f *Factory) CloseIdleConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, entry := range f.clients {
		entry.client.CloseIdleConnections()
	}
}

func resolveConfig(p *provider.Provider, o *Options) (config clientConfig, control func(string, string, syscall.RawConn) error, err error) {
	if o != nil {
		config.timeouts, config.proxyURL, config.tls = o.Timeouts, o.ProxyURL, o.TLS
		control = o.Control
		config.disableProxy = o.DisableProxy
		if o.CheckRedirect != nil {
			config.controlOwner = o
			config.redirectCode = reflect.ValueOf(o.CheckRedirect).Pointer()
		}
		if o.DialContext != nil {
			config.controlOwner = o
			config.dialCode = reflect.ValueOf(o.DialContext).Pointer()
		}
		if control != nil {
			config.controlOwner = o
			config.controlCode = reflect.ValueOf(control).Pointer()
		}
	}
	// Validate both sources before applying overrides so a malformed default
	// cannot silently disable a bound for other providers.
	for _, t := range []provider.Timeouts{config.timeouts, p.Timeouts} {
		if t.Connect < 0 || t.TLS < 0 || t.ResponseHeader < 0 || t.Total < 0 || t.Idle < 0 {
			return config, nil, errors.New("upstream timeouts must not be negative")
		}
	}
	if p.Timeouts.Connect != 0 {
		config.timeouts.Connect = p.Timeouts.Connect
	}
	if p.Timeouts.TLS != 0 {
		config.timeouts.TLS = p.Timeouts.TLS
	}
	if p.Timeouts.ResponseHeader != 0 {
		config.timeouts.ResponseHeader = p.Timeouts.ResponseHeader
	}
	if p.Timeouts.Total != 0 {
		config.timeouts.Total = p.Timeouts.Total
	}
	if p.Timeouts.Idle != 0 {
		config.timeouts.Idle = p.Timeouts.Idle
	}
	applyDefaults(&config.timeouts)
	if p.ProxyURL != "" {
		config.proxyURL = p.ProxyURL
	}
	if p.TLS != "" {
		config.tls = p.TLS
	}
	if config.tls == "" {
		config.tls = provider.TLSDefault
	}
	if config.tls != provider.TLSDefault && config.tls != provider.TLSChrome {
		return config, nil, fmt.Errorf("unsupported upstream TLS profile %q", config.tls)
	}
	return config, control, nil
}

func proxyFunction(raw string) (func(*http.Request) (*url.URL, error), error) {
	if raw == "" {
		return http.ProxyFromEnvironment, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid upstream proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("upstream proxy must use http, https, socks5 or socks5h")
	}
	if u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("upstream proxy must have a host and no path, query or fragment")
	}
	return http.ProxyURL(u), nil
}

// WithStream marks a request context as streaming: there is no total deadline,
// but silence after response headers is bounded by Timeouts.Idle. It preserves
// caller cancellation/deadlines. Apply before http.NewRequestWithContext; no
// payload inspection or response Content-Type guessing is performed.
func WithStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamKey{}, true)
}

type streamKey struct{}

type proxyURLKey struct{}
