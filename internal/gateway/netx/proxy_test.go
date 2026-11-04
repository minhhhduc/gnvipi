package netx

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestHTTPSProxyUsesHTTP1CONNECTWithProxySNI(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.ProtoMajor != 1 || r.Host != "origin.invalid:443" {
			t.Errorf("proxy request = %s %s Host=%q", r.Method, r.Proto, r.Host)
		}
		if r.TLS.NegotiatedProtocol != "http/1.1" || r.TLS.ServerName != "proxy.test" {
			t.Errorf("outer TLS ALPN=%q SNI=%q", r.TLS.NegotiatedProtocol, r.TLS.ServerName)
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("proxy CONNECT did not use an HTTP/1 connection")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack proxy connection: %v", err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Errorf("close proxy peer: %v", err)
			}
		}()
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Errorf("write CONNECT response: %v", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Errorf("flush CONNECT response: %v", err)
			return
		}
		if _, err := io.Copy(conn, buffered); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("proxy echo ended: %v", err)
		}
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.Host = net.JoinHostPort("proxy.test", proxyURL.Port())
	dialer := &net.Dialer{Timeout: time.Second}
	dial := DialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	})
	config := &tls.Config{InsecureSkipVerify: true, ServerName: "origin.invalid", NextProtos: []string{"h2", "http/1.1"}}
	conn, err := DialProxy(t.Context(), "tcp", "origin.invalid:443", proxyURL, dial, time.Second, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close proxy tunnel: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("CONNECT echo = %q, %v", line, err)
	}
	if config.ServerName != "origin.invalid" || config.NextProtos[0] != "h2" {
		t.Fatal("proxy handshake changed the caller's origin TLS configuration")
	}
}
