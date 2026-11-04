package upstream

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func testClient(t *testing.T, p *provider.Provider, options *Options) *http.Client {
	t.Helper()
	factory := NewFactory()
	t.Cleanup(factory.CloseIdleConnections)
	client, err := factory.NewClient(p, options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
}

func TestNoHeadersTimeoutMapsTo504(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	p := &provider.Provider{ID: "stall", Timeouts: provider.Timeouts{ResponseHeader: 40 * time.Millisecond}}
	client := testClient(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		}()
	}
	if err == nil || gwerr.StatusForKind(gwerr.KindFromErr(err)) != http.StatusGatewayTimeout {
		t.Fatalf("error = %v, want transport timeout mapped to 504", err)
	}
	awaitSignal(t, canceled)
}

func TestStreamSilenceTimesOutWithoutAnotherRead(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			t.Errorf("write stream response: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "silent", Timeouts: provider.Timeouts{Idle: 40 * time.Millisecond}}, nil)
	req, err := http.NewRequestWithContext(WithStream(context.Background()), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	// The watchdog must run while the consumer is idle, not only during Read.
	awaitSignal(t, canceled)
	_, err = resp.Body.Read(make([]byte, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v, want idle deadline", err)
	}
}

func TestCallerCancellationReachesUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "cancel"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(WithStream(ctx), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	awaitSignal(t, started)
	cancel()
	awaitSignal(t, canceled)
	_, err = resp.Body.Read(make([]byte, 1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want caller cancellation", err)
	}
}

func TestTotalDeadlineCoversNonStreamBody(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "partial"); err != nil {
			t.Errorf("write partial response: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "body", Timeouts: provider.Timeouts{Total: 40 * time.Millisecond}}, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if string(body) != "partial" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body = %q, error = %v; want partial body and total deadline", body, err)
	}
	awaitSignal(t, canceled)
}

func TestStreamProgressResetsIdleAndIgnoresTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 6 {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "progress", Timeouts: provider.Timeouts{Total: 30 * time.Millisecond, Idle: 100 * time.Millisecond}}, nil)
	req, err := http.NewRequestWithContext(WithStream(context.Background()), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "xxxxxx" {
		t.Fatalf("body = %q, error = %v; active stream must outlive non-stream total", body, err)
	}
}

func TestCacheInvalidationChangesTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			if _, err := io.WriteString(w, "ready"); err != nil {
				t.Errorf("write ready response: %v", err)
				return
			}
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	factory := NewFactory()
	defer factory.CloseIdleConnections()
	p := &provider.Provider{ID: "cached", Headers: map[string]string{"X-Config": "one"}}
	first, err := factory.NewClient(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := factory.NewClient(p, nil)
	if err != nil || cached != first {
		t.Fatalf("unchanged provider did not reuse cached client: %v", err)
	}
	p.Headers["X-Config"] = "two"
	p.Timeouts.ResponseHeader = 30 * time.Millisecond
	changed, err := factory.NewClient(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("changed provider reused stale client")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	changedResp, err := changed.Do(req)
	if changedResp != nil {
		defer func() {
			if err := changedResp.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		}()
	}
	if gwerr.StatusForKind(gwerr.KindFromErr(err)) != 504 {
		t.Fatalf("changed client's error = %v, want shorter timeout", err)
	}
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := first.Do(req)
	if err != nil {
		t.Fatalf("previous client should remain usable for in-flight callers: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ready" {
		t.Fatalf("original body = %q, error = %v", body, err)
	}
}

func TestTLSHandshakeDeadline(t *testing.T) {
	for _, profile := range []provider.TLSProfile{provider.TLSDefault, provider.TLSChrome} {
		t.Run(string(profile), func(t *testing.T) {
			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := listener.Close(); err != nil {
					t.Errorf("close TLS listener: %v", err)
				}
			}()
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() {
					if err := conn.Close(); err != nil {
						t.Errorf("close TLS connection: %v", err)
					}
				}()
				if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Errorf("set TLS read deadline: %v", err)
					return
				}
				// The client's handshake deadline can reset this stalled connection.
				if _, err := io.Copy(io.Discard, conn); err != nil {
					t.Logf("stalled TLS read ended: %v", err)
				}
			}()
			client := testClient(t, &provider.Provider{ID: "tls", TLS: profile, Timeouts: provider.Timeouts{TLS: 40 * time.Millisecond}}, nil)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+listener.Addr().String(), http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if resp != nil {
				defer func() {
					if err := resp.Body.Close(); err != nil {
						t.Errorf("close response body: %v", err)
					}
				}()
			}
			if err == nil || gwerr.StatusForKind(gwerr.KindFromErr(err)) != 504 {
				t.Fatalf("TLS error = %v, want timeout mapped to 504", err)
			}
			awaitSignal(t, closed)
		})
	}
}

func TestProviderProxyOverridesFactoryProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() || r.URL.Host != "unresolvable.invalid" {
			t.Errorf("proxy request target = %q", r.RequestURI)
		}
		if _, err := io.WriteString(w, "proxied"); err != nil {
			t.Errorf("write proxy response: %v", err)
			return
		}
	}))
	defer proxy.Close()
	client := testClient(t, &provider.Provider{ID: "proxy", ProxyURL: proxy.URL}, &Options{ProxyURL: "http://127.0.0.1:1"})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unresolvable.invalid/v1/chat", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "proxied" {
		t.Fatalf("proxy response = %q, error = %v", body, err)
	}
}

func TestEnvironmentProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "environment.invalid" {
			t.Errorf("proxy host = %q", r.URL.Host)
		}
		if _, err := io.WriteString(w, "environment proxy"); err != nil {
			t.Errorf("write environment proxy response: %v", err)
			return
		}
	}))
	defer proxy.Close()
	// ProxyFromEnvironment caches its first lookup process-wide. A fresh
	// process makes this behavior deterministic and leaves other tests alone.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEnvironmentProxyChild$")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD", "UPSTREAM_PROXY_CHILD":
		default:
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "NO_PROXY=", "UPSTREAM_PROXY_CHILD=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy child: %v\n%s", err, output)
	}
}

func TestEnvironmentProxyChild(t *testing.T) {
	if os.Getenv("UPSTREAM_PROXY_CHILD") != "1" {
		t.Skip("invoked by TestEnvironmentProxy with a fresh environment")
	}
	client := testClient(t, &provider.Provider{ID: "environment"}, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://environment.invalid/chat", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "environment proxy" {
		t.Fatalf("body = %q, error = %v", body, err)
	}
}

func TestControlCanRejectDial(t *testing.T) {
	denied := errors.New("test dial denied")
	client := testClient(t, &provider.Provider{ID: "controlled"}, &Options{Control: func(string, string, syscall.RawConn) error { return denied }})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		}()
	}
	if !errors.Is(err, denied) {
		t.Fatalf("dial error = %v, want Control rejection", err)
	}
}

func TestRegisteredFlagsConfigureTimeout(t *testing.T) {
	var options Options
	flags := flag.NewFlagSet("upstream", flag.ContinueOnError)
	options.RegisterFlags(flags)
	if err := flags.Parse([]string{"-upstream-response-header-timeout=30ms"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "flags"}, &options)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		}()
	}
	if err == nil || gwerr.StatusForKind(gwerr.KindFromErr(err)) != 504 {
		t.Fatalf("flag-configured error = %v, want header timeout", err)
	}
}
