package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/netx"
)

// IPResolver is injectable for deterministic final-destination checks.
type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func splitDialAddress(address string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return "", "", &FieldError{Field: "base_url", Problem: "dial address must include a host and port"}
	}
	return host, port, nil
}

// Dialer pins validated DNS answers before any HTTP/SOCKS proxy sees the
// destination. Use it with upstream.Options.DialContext and DisableProxy=true
// when composing with the upstream factory; Control is still available for
// native, direct dialers. A returned tunnel keeps the original HTTP Host/SNI.
// A caller deadline bounds resolution, connection and tunneling together; an
// unbounded caller gets a ten-second fallback.
func (p URLPolicy) Dialer(proxyURL *url.URL, resolver IPResolver, tlsConfig *tls.Config) netx.DialFunc {
	if proxyURL != nil {
		copyURL := *proxyURL
		proxyURL = &copyURL
	}
	if tlsConfig != nil {
		tlsConfig = tlsConfig.Clone()
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		timeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
			if timeout <= 0 {
				return nil, context.DeadlineExceeded
			}
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		host, port, err := splitDialAddress(address)
		if err != nil {
			return nil, err
		}
		if err := p.checkHost(host); err != nil {
			return nil, &FieldError{Field: "base_url", Problem: err.Error()}
		}
		pinned := address
		if _, err := netip.ParseAddr(host); err != nil && (proxyURL != nil || resolver != nil) {
			lookup := resolver
			if lookup == nil {
				lookup = net.DefaultResolver
			}
			addresses, err := lookup.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve upstream host: %w", err)
			}
			var selected netip.Addr
			for i := range addresses {
				addr, valid := netip.AddrFromSlice(addresses[i].IP)
				if !valid {
					return nil, errors.New("resolver returned an invalid upstream address")
				}
				addr = addr.Unmap()
				if addr.Is6() && addresses[i].Zone != "" {
					addr = addr.WithZone(addresses[i].Zone)
				}
				if err := p.checkIP(addr); err != nil {
					return nil, &FieldError{Field: "base_url", Problem: err.Error()}
				}
				if !selected.IsValid() || (!selected.Is4() && addr.Is4()) {
					selected = addr
				}
			}
			if !selected.IsValid() {
				return nil, errors.New("resolver returned no upstream addresses")
			}
			pinned = net.JoinHostPort(selected.String(), port)
		}
		dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: p.Control}
		if proxyURL != nil {
			// The configured proxy is a trusted hop, not the upstream. Its
			// loopback/LAN socket must not inherit the origin's private-IP ban;
			// the ultimate target was already validated and pinned above.
			dialer.Control = nil
		}
		return netx.DialProxy(ctx, network, pinned, proxyURL, dialer.DialContext, dialer.Timeout, tlsConfig)
	}
}

// GuardedTransport preserves standard TLS and proxy behavior while preventing
// remote proxy DNS rebinding. For custom TLS dialers, compose Dialer with the
// upstream factory instead. Connection pools are separated by proxy identity.
type GuardedTransport struct {
	policy   URLPolicy
	base     *http.Transport
	proxy    func(*http.Request) (*url.URL, error)
	resolver IPResolver
	mu       sync.Mutex
	proxied  map[string]*http.Transport
}

func (p URLPolicy) GuardTransport(base *http.Transport, resolver IPResolver) *GuardedTransport {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	copyTransport := base.Clone()
	g := &GuardedTransport{policy: p, proxy: copyTransport.Proxy, resolver: resolver, proxied: make(map[string]*http.Transport)}
	copyTransport.Proxy = nil
	copyTransport.DialTLSContext = nil
	copyTransport.DialContext = p.Dialer(nil, resolver, copyTransport.TLSClientConfig)
	g.base = copyTransport
	return g
}

func (g *GuardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.policy.ValidateRequest(req); err != nil {
		return nil, err
	}
	if g.proxy == nil {
		return g.base.RoundTrip(req)
	}
	proxyURL, err := g.proxy(req)
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return g.base.RoundTrip(req)
	}
	key := proxyURL.String()
	g.mu.Lock()
	transport := g.proxied[key]
	if transport == nil {
		transport = g.base.Clone()
		transport.DialContext = g.policy.Dialer(proxyURL, g.resolver, transport.TLSClientConfig)
		g.proxied[key] = transport
	}
	g.mu.Unlock()
	return transport.RoundTrip(req)
}

func (g *GuardedTransport) CloseIdleConnections() {
	g.base.CloseIdleConnections()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, transport := range g.proxied {
		transport.CloseIdleConnections()
	}
}
