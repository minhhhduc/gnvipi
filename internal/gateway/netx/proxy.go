// Package netx provides the shared context-aware proxy tunnel for upstreams.
// CONNECT preserves HTTP Host and TLS SNI when a caller pins a destination IP.
package netx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// DialFunc adapts a context-aware connection callback to the SOCKS interface.
// Actual tunnel creation always uses DialContext; Dial is required by FromURL.
type DialFunc func(context.Context, string, string) (net.Conn, error)

func (f DialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}
func (f DialFunc) Dial(network, address string) (net.Conn, error) {
	return f(context.Background(), network, address)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// DialProxy uses a CONNECT tunnel for HTTP(S) proxies, including plaintext
// upstreams, so a validated IP is not re-resolved by the proxy. The caller owns
// the returned connection. Proxy credentials never enter diagnostic messages.
func DialProxy(ctx context.Context, network, address string, proxyURL *url.URL, dial DialFunc, timeout time.Duration, tlsConfig *tls.Config) (net.Conn, error) {
	if dial == nil {
		return nil, errors.New("proxy connection callback is nil")
	}
	if proxyURL == nil {
		return dial(ctx, network, address)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		normalized := *proxyURL
		normalized.Scheme = "socks5"
		d, err := proxy.FromURL(&normalized, dial)
		if err != nil {
			return nil, err
		}
		contextual, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("proxy dialer does not support context cancellation")
		}
		return contextual.DialContext(connectCtx, network, address)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, errors.New("unsupported proxy scheme")
	}
	port := proxyURL.Port()
	if port == "" {
		if proxyURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := dial(connectCtx, "tcp", net.JoinHostPort(proxyURL.Hostname(), port))
	if err != nil {
		return nil, err
	}
	raw := conn
	stop := context.AfterFunc(connectCtx, func() {
		if err := raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("close canceled proxy tunnel: %v", err)
		}
	})
	defer stop()
	if deadline, ok := connectCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, closeConn(connectCtx, conn, err)
		}
	}
	if proxyURL.Scheme == "https" {
		config := &tls.Config{ServerName: proxyURL.Hostname()}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			config.ServerName = proxyURL.Hostname()
		}
		// CONNECT below speaks HTTP/1.1 regardless of the origin's ALPN.
		config.NextProtos = []string{"http/1.1"}
		encrypted := tls.Client(conn, config)
		if err := encrypted.HandshakeContext(connectCtx); err != nil {
			return nil, closeConn(connectCtx, conn, err)
		}
		conn = encrypted
	}
	req, err := http.NewRequestWithContext(connectCtx, http.MethodConnect, "http://"+address, http.NoBody)
	if err != nil {
		return nil, closeConn(connectCtx, conn, err)
	}
	req.URL.Opaque = address
	req.Host = address
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		return nil, closeConn(connectCtx, conn, err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, closeConn(connectCtx, conn, err)
	}
	if response.StatusCode != http.StatusOK {
		failure := closeConn(connectCtx, conn, fmt.Errorf("proxy CONNECT returned HTTP %d", response.StatusCode))
		if err := response.Body.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failure = errors.Join(failure, err)
		}
		return nil, failure
	}
	// CONNECT 200 switches protocols: tunnel ownership transfers to the caller.
	// Closing an HTTP response body here could close the live tunnel.
	if !stop() && connectCtx.Err() != nil {
		return nil, closeConn(connectCtx, conn, connectCtx.Err())
	}
	if err := connectCtx.Err(); err != nil {
		return nil, closeConn(connectCtx, conn, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, closeConn(connectCtx, conn, err)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func closeConn(ctx context.Context, conn net.Conn, primary error) error {
	if err := ctx.Err(); err != nil {
		primary = errors.Join(primary, err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return errors.Join(primary, err)
	}
	return primary
}
