package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestSOCKSNegotiationClosesSocketAfterDetachedDialTimeout(t *testing.T) {
	for _, profile := range []provider.TLSProfile{provider.TLSDefault, provider.TLSChrome} {
		t.Run(string(profile), func(t *testing.T) {
			var lc net.ListenConfig
			listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := listener.Close(); err != nil {
					t.Errorf("close SOCKS listener: %v", err)
				}
			})
			closed := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer close(closed)
				defer func() {
					if err := conn.Close(); err != nil {
						t.Errorf("close SOCKS peer: %v", err)
					}
				}()
				// Accept the greeting without ever answering negotiation.
				if _, err := io.Copy(io.Discard, conn); err != nil {
					t.Logf("SOCKS peer read ended: %v", err)
				}
			}()
			client := testClient(t, &provider.Provider{
				ID: "socks-stall", TLS: profile, ProxyURL: "socks5://" + listener.Addr().String(),
				Timeouts: provider.Timeouts{Connect: 100 * time.Millisecond, ResponseHeader: 40 * time.Millisecond},
			}, nil)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://upstream.invalid/", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if resp != nil {
				if closeErr := resp.Body.Close(); closeErr != nil {
					t.Errorf("close SOCKS response: %v", closeErr)
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request error = %v, want deadline", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("timed-out SOCKS negotiation retained its socket before cache cleanup")
			}
		})
	}
}

func TestChromeSharedDialWaitHonorsEachRequestContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancellation-cause"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			var lc net.ListenConfig
			listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := listener.Close(); err != nil {
					t.Errorf("close TLS listener: %v", err)
				}
			}()
			accepted := make(chan struct{})
			closed := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				close(accepted)
				defer close(closed)
				defer func() {
					if err := conn.Close(); err != nil {
						t.Errorf("close stalled TLS peer: %v", err)
					}
				}()
				if _, err := io.Copy(io.Discard, conn); err != nil {
					t.Logf("TLS peer read ended: %v", err)
				}
			}()
			client := testClient(t, &provider.Provider{ID: "shared-dial", TLS: provider.TLSChrome}, &Options{Timeouts: provider.Timeouts{TLS: 2 * time.Second}})
			firstCtx, cancelFirst := context.WithCancel(t.Context())
			defer cancelFirst()
			first, err := http.NewRequestWithContext(firstCtx, http.MethodGet, "https://"+listener.Addr().String(), http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			firstDone := make(chan error, 1)
			go func() {
				resp, err := client.Do(first)
				if resp != nil {
					err = errors.Join(err, resp.Body.Close())
				}
				firstDone <- err
			}()
			awaitSignal(t, accepted)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			want := errors.New("second caller stopped")
			if deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
				want = context.DeadlineExceeded
			}
			waiting := make(chan struct{}, 1)
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { waiting <- struct{}{} }})
			second, err := http.NewRequestWithContext(ctx, http.MethodGet, first.URL.String(), http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			secondDone := make(chan error, 1)
			go func() {
				resp, err := client.Do(second)
				if resp != nil {
					err = errors.Join(err, resp.Body.Close())
				}
				secondDone <- err
			}()
			awaitSignal(t, waiting)
			if !deadline {
				cancel(want)
			}
			select {
			case err := <-secondDone:
				if !errors.Is(err, want) {
					t.Fatalf("waiting request error = %v, want %v", err, want)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("waiting request remained blocked on the first request's TLS handshake")
			}
			select {
			case err := <-firstDone:
				t.Fatalf("second request interrupted shared dial owner: %v", err)
			default:
			}
			cancelFirst()
			select {
			case err := <-firstDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("first request cancellation error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("first request did not finish after its own cancellation")
			}
			awaitSignal(t, closed)
		})
	}
}

func TestRetiredChromeConnectionExpiresWhenItBecomesIdle(t *testing.T) {
	started, finish, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("Chrome request protocol = %s", r.Proto)
		}
		if _, err := io.WriteString(w, "start"); err != nil {
			t.Errorf("write first body: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-finish:
			if _, err := io.WriteString(w, "finish"); err != nil {
				t.Errorf("write final body: %v", err)
			}
		case <-r.Context().Done():
			t.Error("retiring client aborted its active response")
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.StartTLS()
	defer server.Close()
	plain := &http.Transport{IdleConnTimeout: 40 * time.Millisecond}
	dialer := &net.Dialer{Timeout: time.Second}
	plain.DialContext = dialer.DialContext
	chrome := newChromeTransport(dialer, nil, time.Second, plain)
	chrome.h2.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone() //nolint:staticcheck // SA1019: trust only the local server in the uTLS transport fixture.
	client := &http.Client{Transport: chrome}
	defer client.CloseIdleConnections()
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
			t.Errorf("close retired body: %v", err)
		}
	}()
	awaitSignal(t, started)
	client.CloseIdleConnections()
	close(finish)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "startfinish" {
		t.Fatalf("retired active response = %q, %v", body, err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retired Chrome client retained its completed idle connection")
	}
}

func TestTotalDeadlineSpansRedirectAndFinalBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			if _, err := io.WriteString(w, "partial"); err != nil {
				t.Errorf("write redirected body: %v", err)
				return
			}
			w.(http.Flusher).Flush()
		}
		select {
		case <-time.After(120 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		if _, err := io.WriteString(w, "finished"); err != nil {
			t.Logf("write after total deadline: %v", err)
		}
	}))
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "redirect-total", Timeouts: provider.Timeouts{Total: 200 * time.Millisecond}}, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/start", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		body, readErr := io.ReadAll(resp.Body)
		if string(body) != "partial" {
			t.Fatalf("redirected body = %q, want partial body before deadline", body)
		}
		err = errors.Join(err, readErr, resp.Body.Close())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redirect chain error = %v, want one total deadline", err)
	}
}

func TestFactoryProxyParseErrorDoesNotExposeCredentials(t *testing.T) {
	factory := NewFactory()
	_, err := factory.NewClient(&provider.Provider{ID: "invalid-proxy", ProxyURL: "http://user:secret@proxy.invalid:bad"}, nil)
	if err == nil {
		t.Fatal("malformed credential-bearing proxy was accepted")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("proxy credentials entered diagnostic: %v", err)
	}
}
