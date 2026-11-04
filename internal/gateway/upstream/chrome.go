package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/netx"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

type chromeTransport struct {
	h2        *http2.Transport //nolint:staticcheck // SA1019: uTLS does not expose crypto/tls.Conn ALPN to the standard transport.
	pool      *chromeConnPool
	plain     *http.Transport
	proxy     func(*http.Request) (*url.URL, error)
	dial      netx.DialFunc
	connect   time.Duration
	handshake time.Duration
}

func newChromeTransport(dialer *net.Dialer, proxy func(*http.Request) (*url.URL, error), handshake time.Duration, plain *http.Transport) *chromeTransport {
	c := &chromeTransport{plain: plain, proxy: proxy, dial: plain.DialContext, connect: dialer.Timeout, handshake: handshake}
	c.pool = &chromeConnPool{transport: c, conns: make(map[string][]*http2.ClientConn), dialing: make(map[string]*chromeDialCall)} //nolint:staticcheck // SA1019: the uTLS transport's public pool contract requires ClientConn.
	c.h2 = &http2.Transport{                                                                                                       //nolint:staticcheck // SA1019: required for Chrome-uTLS HTTP/2 compatibility.
		DialTLSContext:  c.dialTLS,             //nolint:staticcheck // SA1019: context-aware uTLS dialer cannot use standard TLS ALPN introspection.
		ConnPool:        c.pool,                //nolint:staticcheck // SA1019: explicit pool is required for cancellation-aware uTLS acquisition.
		IdleConnTimeout: plain.IdleConnTimeout, //nolint:staticcheck // SA1019: bound idle uTLS connections without a standard TLS transport.
	}
	return c
}

func (c *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" {
		return c.plain.RoundTrip(req)
	}
	if _, selected := req.Context().Value(proxyURLKey{}).(*url.URL); !selected && c.proxy != nil {
		proxyURL, err := c.proxy(req)
		if err != nil {
			return nil, err
		}
		req = req.Clone(context.WithValue(req.Context(), proxyURLKey{}, proxyURL))
	}
	return c.h2.RoundTrip(req) //nolint:staticcheck // SA1019: Chrome-uTLS requires this HTTP/2 transport.
}

func (c *chromeTransport) dialTLS(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
	proxyURL, _ := ctx.Value(proxyURLKey{}).(*url.URL)
	raw, err := netx.DialProxy(ctx, network, address, proxyURL, c.dial, c.connect, config)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, closeChromeConn(raw, err)
	}
	settings := &utls.Config{ServerName: host}
	if config != nil {
		settings.RootCAs = config.RootCAs
		settings.InsecureSkipVerify = config.InsecureSkipVerify
		settings.MinVersion, settings.MaxVersion = config.MinVersion, config.MaxVersion
		if config.ServerName != "" {
			settings.ServerName = config.ServerName
		}
	}
	conn := utls.UClient(raw, settings, utls.HelloChrome_Auto)
	handshakeCtx, cancel := context.WithTimeout(ctx, c.handshake)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		return nil, closeChromeConn(raw, err)
	}
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		return nil, closeChromeConn(raw, fmt.Errorf("chrome TLS upstream did not negotiate HTTP/2"))
	}
	return conn, nil
}

func closeChromeConn(conn net.Conn, primary error) error {
	if err := conn.Close(); err != nil {
		return fmt.Errorf("%w; close TLS connection: %w", primary, err)
	}
	return primary
}

func (c *chromeTransport) CloseIdleConnections() {
	c.pool.CloseIdleConnections()
	c.plain.CloseIdleConnections()
}

// The dependency's default pool waits on a shared dial without consulting
// each waiting request. Keep sharing connections and dials, but let every
// waiter leave promptly without canceling the request that owns the dial.
type chromeConnPool struct {
	transport *chromeTransport
	mu        sync.Mutex
	conns     map[string][]*http2.ClientConn //nolint:staticcheck // SA1019: retain uTLS-compatible connections for the explicit pool.
	dialing   map[string]*chromeDialCall
}

type chromeDialCall struct {
	ctx  context.Context
	done chan struct{}
	err  error
}

func (p *chromeConnPool) GetClientConn(req *http.Request, address string) (*http2.ClientConn, error) { //nolint:staticcheck // SA1019: implements the uTLS transport's required pool contract.
	ctx := req.Context()
	if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.GetConn != nil {
		trace.GetConn(address)
	}
	if req.Close || httpguts.HeaderValuesContainsToken(req.Header["Connection"], "close") {
		// Native HTTP/2 request-close semantics require a dedicated connection.
		// RoundTrip marks it non-reusable when it starts the first stream.
		cc, err := p.newConn(ctx, address)
		if err != nil {
			return nil, err
		}
		if !cc.ReserveNewRequest() {
			return nil, errors.Join(errors.New("chrome HTTP/2 connection became unavailable"), cc.Close())
		}
		return cc, nil
	}
	for {
		if err := requestContextError(ctx); err != nil {
			return nil, err
		}
		p.mu.Lock()
		for _, cc := range p.conns[address] {
			if cc.ReserveNewRequest() {
				p.mu.Unlock()
				return cc, nil
			}
		}
		call := p.dialing[address]
		if call == nil {
			call = &chromeDialCall{ctx: ctx, done: make(chan struct{})}
			p.dialing[address] = call
			go p.dial(call, address)
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, requestContextError(ctx)
		case <-call.done:
		}
		if call.err != nil {
			if call.ctx != ctx && call.ctx.Err() != nil && (errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded)) {
				// A canceled dial owner must not cancel another live waiter.
				continue
			}
			return nil, call.err
		}
	}
}

func (p *chromeConnPool) newConn(ctx context.Context, address string) (*http2.ClientConn, error) { //nolint:staticcheck // SA1019: establish a connection usable by the uTLS transport's pool.
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{ServerName: host, NextProtos: []string{"h2"}}
	if configured := p.transport.h2.TLSClientConfig; configured != nil { //nolint:staticcheck // SA1019: retain the origin TLS policy for uTLS.
		config = configured.Clone()
		if config.ServerName == "" {
			config.ServerName = host
		}
	}
	conn, err := p.transport.h2.DialTLSContext(ctx, "tcp", address, config) //nolint:staticcheck // SA1019: Chrome connections require the uTLS callback.
	if err != nil {
		if cause := requestContextError(ctx); cause != nil {
			err = errors.Join(err, cause)
		}
		return nil, err
	}
	// NewClientConn writes the HTTP/2 preface; cancellation also bounds that
	// final establishment step rather than just TCP and TLS negotiation.
	stop := context.AfterFunc(ctx, func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("close canceled Chrome HTTP/2 dial: %v", err)
		}
	})
	cc, err := p.transport.h2.NewClientConn(conn) //nolint:staticcheck // SA1019: the explicit uTLS pool creates public HTTP/2 connections.
	stop()
	if cause := requestContextError(ctx); cause != nil {
		return nil, closeChromeConn(conn, errors.Join(err, cause))
	}
	if err != nil {
		return nil, err // NewClientConn closes connections on preface failure.
	}
	return cc, nil
}

func (p *chromeConnPool) dial(call *chromeDialCall, address string) {
	cc, err := p.newConn(call.ctx, address)
	p.mu.Lock()
	delete(p.dialing, address)
	if err == nil {
		p.conns[address] = append(p.conns[address], cc)
	}
	call.err = err
	p.mu.Unlock()
	close(call.done)
}

func (p *chromeConnPool) MarkDead(dead *http2.ClientConn) { //nolint:staticcheck // SA1019: implements the uTLS transport's required pool contract.
	p.mu.Lock()
	defer p.mu.Unlock()
	for address, conns := range p.conns {
		for i, cc := range conns {
			if cc != dead {
				continue
			}
			copy(conns[i:], conns[i+1:])
			conns[len(conns)-1] = nil
			if len(conns) == 1 {
				delete(p.conns, address)
			} else {
				p.conns[address] = conns[:len(conns)-1]
			}
			break
		}
	}
}

func (p *chromeConnPool) CloseIdleConnections() {
	p.mu.Lock()
	var idle []*http2.ClientConn //nolint:staticcheck // SA1019: close idle connections owned by the uTLS pool.
	for _, conns := range p.conns {
		for _, cc := range conns {
			state := cc.State()
			if state.StreamsActive == 0 && state.StreamsReserved == 0 && state.StreamsPending == 0 {
				// Hold the pool lock through marking so acquisition cannot race
				// the idle snapshot and reserve a connection we are closing.
				cc.SetDoNotReuse()
				idle = append(idle, cc)
			}
		}
	}
	p.mu.Unlock()
	for _, cc := range idle {
		if err := cc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("close idle Chrome HTTP/2 connection: %v", err)
		}
		p.MarkDead(cc)
	}
}
