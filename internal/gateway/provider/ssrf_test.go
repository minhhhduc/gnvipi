package provider

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestURLPolicySaveAndDialBoundaries(t *testing.T) {
	for _, raw := range []string{"http://169.254.169.254", "https://169.254.1.2", "http://[fe80::1]", "http://[fd00:ec2::254]", "http://[::ffff:169.254.169.254]", "http://metadata.google.internal.", "http://u:p@example.com", "ftp://example.com", "http:///missing"} {
		t.Run(raw, func(t *testing.T) {
			err := (URLPolicy{}).Validate(raw)
			var field *FieldError
			if !errors.As(err, &field) || field.Field != "base_url" {
				t.Fatalf("error=%v want base_url error", err)
			}
		})
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://192.168.1.10", "http://[::1]", "https://example.com/v1"} {
		if err := (URLPolicy{}).Validate(raw); err != nil {
			t.Fatalf("legitimate upstream %s rejected: %v", raw, err)
		}
	}
	for _, address := range []string{"169.254.169.254:80", "[fe80::1]:80", "[fd00:ec2::254]:80", "[::ffff:169.254.169.254]:80"} {
		if err := (URLPolicy{}).Control("tcp", address, nil); err == nil {
			t.Fatalf("unsafe resolved dial %s accepted", address)
		}
	}
	if err := (URLPolicy{DenyPrivate: true}).Validate("http://127.0.0.1"); err == nil {
		t.Fatal("deny-private did not reject loopback")
	}
	if err := (URLPolicy{AllowUnsafe: true}).Validate("http://169.254.169.254"); err != nil {
		t.Fatalf("explicit unsafe override rejected: %v", err)
	}
	if err := (URLPolicy{AllowUnsafe: true}).Validate("http://u:p@example.com"); err == nil {
		t.Fatal("unsafe override disabled URL syntax validation")
	}
}

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func closeNetwork(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close: %v", err)
	}
}

func TestProxyDialPinsResolvedDestinationAndRetainsHost(t *testing.T) {
	hosts := make(chan string, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Error(err)
		}
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	var connects atomic.Int32
	var tunnels sync.WaitGroup
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunnels.Add(1)
		defer tunnels.Done()
		connects.Add(1)
		want := net.JoinHostPort("127.0.0.1", originURL.Port())
		if r.Method != http.MethodConnect || r.Host != want {
			t.Errorf("proxy target=%s %s want CONNECT %s", r.Method, r.Host, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstream, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", r.Host)
		if err != nil {
			t.Error(err)
			return
		}
		downstream, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			closeNetwork(t, upstream)
			t.Error(err)
			return
		}
		defer closeNetwork(t, downstream)
		defer closeNetwork(t, upstream)
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		tunnels.Add(1)
		go func() {
			defer tunnels.Done()
			if _, err := io.Copy(upstream, buffered.Reader); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
			closeNetwork(t, upstream)
		}()
		if _, err := io.Copy(downstream, upstream); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	}))
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	var blocked atomic.Bool
	blocked.Store(true)
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		ip := "127.0.0.1"
		if blocked.Load() {
			ip = "169.254.169.254"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	})
	guard := (URLPolicy{}).GuardTransport(&http.Transport{Proxy: http.ProxyURL(proxyURL)}, resolver)
	client := &http.Client{Transport: guard}
	defer func() { client.CloseIdleConnections(); tunnels.Wait() }()
	target := "http://origin.example:" + originURL.Port() + "/v1"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	unexpected, requestErr := client.Do(req)
	if unexpected != nil {
		defer func() {
			if err := unexpected.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	if requestErr == nil {
		t.Fatal("proxy bypassed metadata resolution guard")
	}
	if connects.Load() != 0 {
		t.Fatal("blocked target reached proxy")
	}
	blocked.Store(false)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("body=%q error=%v", body, err)
	}
	select {
	case got := <-hosts:
		if got != "origin.example:"+originURL.Port() {
			t.Fatalf("origin host=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("origin did not receive request")
	}
}

func TestDenyPrivateAllowsTrustedLocalHTTPProxy(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[encrypted], func(t *testing.T) {
			var connects atomic.Int32
			proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connects.Add(1)
				if r.Method != http.MethodConnect || r.Host != "93.184.216.34:80" {
					t.Errorf("proxy destination=%s %s", r.Method, r.Host)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				conn, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer closeNetwork(t, conn)
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					t.Error(err)
					return
				}
				if err := buffered.Flush(); err != nil {
					t.Error(err)
					return
				}
				upstream, err := http.ReadRequest(buffered.Reader)
				if err != nil {
					t.Error(err)
					return
				}
				defer closeNetwork(t, upstream.Body)
				if upstream.Host != "origin.example" {
					t.Errorf("upstream host=%q", upstream.Host)
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"); err != nil {
					t.Error(err)
				}
			})
			proxyServer := httptest.NewUnstartedServer(proxyHandler)
			if encrypted {
				proxyServer.StartTLS()
			} else {
				proxyServer.Start()
			}
			defer proxyServer.Close()
			proxyURL, err := url.Parse(proxyServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			base := proxyServer.Client().Transport.(*http.Transport).Clone()
			base.Proxy = http.ProxyURL(proxyURL)
			var blocked atomic.Bool
			resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
				ip := "93.184.216.34"
				if blocked.Load() {
					ip = "169.254.169.254"
				}
				return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
			})
			client := &http.Client{Transport: (URLPolicy{DenyPrivate: true}).GuardTransport(base, resolver), Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://origin.example/v1", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("public upstream via trusted loopback proxy: %v", err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil || string(body) != "ok" {
				t.Fatalf("body=%q error=%v", body, readErr)
			}
			blocked.Store(true)
			blockedResponse, blockedErr := client.Do(req)
			if blockedResponse != nil {
				defer func() {
					if err := blockedResponse.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			if blockedErr == nil || connects.Load() != 1 {
				t.Fatalf("metadata bypassed upstream policy: error=%v proxy connections=%d", blockedErr, connects.Load())
			}
		})
	}
}

func TestDenyPrivateAllowsTrustedLocalSOCKSProxy(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		serve := func() error {
			conn, err := listener.Accept()
			if err != nil {
				return err
			}
			defer closeNetwork(t, conn)
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			greeting := make([]byte, 3)
			if _, err := io.ReadFull(conn, greeting); err != nil {
				return err
			}
			if greeting[0] != 5 || greeting[1] != 1 || greeting[2] != 0 {
				return errors.New("unexpected SOCKS greeting")
			}
			if _, err := conn.Write([]byte{5, 0}); err != nil {
				return err
			}
			destination := make([]byte, 10)
			if _, err := io.ReadFull(conn, destination); err != nil {
				return err
			}
			if destination[0] != 5 || destination[1] != 1 || destination[3] != 1 || net.IP(destination[4:8]).String() != "93.184.216.34" || destination[8] != 0 || destination[9] != 80 {
				return errors.New("SOCKS destination was not pinned to the validated public IP")
			}
			if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
				return err
			}
			request, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				return err
			}
			defer closeNetwork(t, request.Body)
			if request.Host != "origin.example" {
				return errors.New("SOCKS tunnel changed the upstream Host")
			}
			_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			return err
		}
		done <- serve()
	}()
	defer func() {
		closeNetwork(t, listener)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	proxyURL, err := url.Parse("socks5h://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	})
	client := &http.Client{Transport: (URLPolicy{DenyPrivate: true}).GuardTransport(&http.Transport{Proxy: http.ProxyURL(proxyURL)}, resolver), Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://origin.example/v1", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("public upstream via trusted loopback SOCKS proxy: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil || string(body) != "ok" {
		t.Fatalf("body=%q error=%v", body, readErr)
	}
}
