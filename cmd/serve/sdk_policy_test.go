package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gwprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type sdkPolicyResolver func(context.Context, string) ([]net.IPAddr, error)

func (f sdkPolicyResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

// Boot the real SDK so its own compatibility executor and HTTP-client helper
// consume the installed transport, rather than duplicating their priorities.
func sdkPolicyEngine(t *testing.T, baseURL, authProxy, defaultProxy string, transports *sdkPolicyTransports) (*gin.Engine, *coreauth.Manager) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, cfgPath, err := buildConfig(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(cfg.AuthDir); err != nil {
			t.Error(err)
		}
	})
	cfg.ProxyURL = defaultProxy
	cfg.OpenAICompatibility = []config.OpenAICompatibility{{
		Name: "sdk-policy-test", BaseURL: baseURL, DisableCooling: true,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "provider-secret", ProxyURL: authProxy}},
		Models:        []config.OpenAICompatibilityModel{{Name: "m", Alias: "sdk-policy-test/m"}},
	}}
	core := coreauth.NewManager(nil, nil, nil)
	ready := make(chan struct{})
	listening := make(chan struct{})
	var listeningOnce sync.Once
	var engine *gin.Engine
	svc, err := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(cfgPath).
		WithCoreAuthManager(core).
		WithWatcherFactory(func(string, string, func(*config.Config)) (*cliproxy.WatcherWrapper, error) {
			return &cliproxy.WatcherWrapper{}, nil
		}).
		WithServerOptions(api.WithEngineConfigurator(func(e *gin.Engine) { engine = e }),
			api.WithMiddleware(func(c *gin.Context) {
				if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/healthz" {
					listeningOnce.Do(func() { close(listening) })
				}
				c.Next()
			})).
		WithHooks(cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) { close(ready) }}).Build()
	if err != nil {
		t.Fatal(err)
	}
	installSDKPolicyTransports(core, cfg, transports)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- svc.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("SDK service did not stop")
		}
		for _, auth := range core.List() {
			cliproxy.GlobalModelRegistry().UnregisterClient(auth.ID)
		}
		for _, transport := range transports.byProxy {
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("SDK service exited before startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("SDK service did not start")
	}
	// The SDK hook follows a fixed delay, not listener initialization. A real
	// socket request and handler signal synchronize Start's writes with cleanup.
	healthClient := &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
	defer healthClient.CloseIdleConnections()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	healthRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for {
		response, err := healthClient.Do(healthRequest)
		if err == nil {
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-listening:
				return engine, core
			default:
			}
		}
		select {
		case err := <-done:
			t.Fatalf("SDK service exited before listener readiness: %v", err)
		case <-timeout.C:
			t.Fatal("SDK listener did not become ready")
		case <-ticker.C:
		}
	}
}

func sdkPolicyRequest(t *testing.T, engine *gin.Engine, path string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(`{"model":"sdk-policy-test/m","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, stream)
	if path == "/v1/responses" {
		payload = fmt.Sprintf(`{"model":"sdk-policy-test/m","stream":%t,"input":"ping"}`, stream)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	return response
}

func sdkPolicyProxy(t *testing.T, requests chan<- *http.Request, location string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, connect *http.Request) {
		if connect.Method != http.MethodConnect || connect.Host != "93.184.216.34:80" {
			t.Errorf("unvalidated proxy destination: %s %s", connect.Method, connect.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}()
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
		body, err := io.ReadAll(upstream.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := upstream.Body.Close(); err != nil {
			t.Error(err)
			return
		}
		if upstream.Host != "origin.example" || upstream.Header.Get("Authorization") != "Bearer provider-secret" || !bytes.Contains(body, []byte(`"model":"m"`)) {
			t.Errorf("unexpected translated request: host=%s authorization=%q body=%s", upstream.Host, upstream.Header.Get("Authorization"), body)
		}
		requests <- upstream
		status, contentType := http.StatusOK, "application/json"
		payload := `{"id":"chat-policy","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		if bytes.Contains(body, []byte(`"stream":true`)) {
			contentType = "text/event-stream"
			payload = "data: {\"id\":\"chat-policy\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-policy\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		}
		header := http.Header{"Content-Type": {contentType}}
		if location != "" {
			status = http.StatusTemporaryRedirect
			header.Set("Location", location)
		}
		response := &http.Response{StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Header: header, Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Close: true}
		if err := response.Write(conn); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSDKTranslatedCustomProviderUsesGuardedProxyTransport(t *testing.T) {
	for _, mode := range []string{"auth", "default"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan *http.Request, 8)
			proxy := sdkPolicyProxy(t, requests, "")
			var blocked atomic.Bool
			transports := &sdkPolicyTransports{policy: gwprovider.URLPolicy{DenyPrivate: true}, resolver: sdkPolicyResolver(func(context.Context, string) ([]net.IPAddr, error) {
				ip := "93.184.216.34"
				if blocked.Load() {
					ip = "169.254.169.254"
				}
				return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
			})}
			authProxy, defaultProxy := proxy.URL, ""
			if mode == "default" {
				authProxy, defaultProxy = "", proxy.URL
			}
			engine, core := sdkPolicyEngine(t, "http://origin.example/v1", authProxy, defaultProxy, transports)
			for _, path := range []string{"/v1/messages", "/v1/responses"} {
				for _, stream := range []bool{false, true} {
					response := sdkPolicyRequest(t, engine, path, stream)
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pong") {
						t.Fatalf("%s stream=%t status=%d body=%s", path, stream, response.Code, response.Body)
					}
					select {
					case request := <-requests:
						if request.URL.Path != "/v1/chat/completions" {
							t.Fatalf("upstream path=%s", request.URL.Path)
						}
					default:
						t.Fatal("SDK did not use the guarded local proxy")
					}
				}
			}
			blocked.Store(true)
			for _, path := range []string{"/v1/messages", "/v1/responses"} {
				for _, stream := range []bool{false, true} {
					response := sdkPolicyRequest(t, engine, path, stream)
					if response.Code < 400 {
						t.Fatalf("metadata resolution succeeded: %s stream=%t status=%d body=%s", path, stream, response.Code, response.Body)
					}
				}
			}
			select {
			case request := <-requests:
				t.Fatalf("metadata request reached trusted proxy: %s", request.URL)
			default:
			}
			found := false
			for _, auth := range core.List() {
				if auth.Attributes["compat_name"] != "sdk-policy-test" {
					continue
				}
				found = true
				if auth.ProxyURL != authProxy || auth.Attributes["api_key"] != "provider-secret" || auth.Metadata["disable_cooling"] != true {
					t.Fatalf("execution changed stored credential: proxy=%q attributes=%v metadata=%v", auth.ProxyURL, auth.Attributes, auth.Metadata)
				}
			}
			if !found {
				t.Fatal("SDK custom-provider credential disappeared")
			}
		})
	}
}

func TestSDKTransportRejectsCrossOriginRedirect(t *testing.T) {
	requests := make(chan *http.Request, 2)
	proxy := sdkPolicyProxy(t, requests, "http://93.184.216.35/stolen")
	transports := &sdkPolicyTransports{resolver: sdkPolicyResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	})}
	engine, _ := sdkPolicyEngine(t, "http://origin.example/v1", proxy.URL, "", transports)
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		response := sdkPolicyRequest(t, engine, path, false)
		if response.Code < 400 {
			t.Fatalf("cross-origin redirect followed: status=%d body=%s", response.Code, response.Body)
		}
		select {
		case <-requests:
		default:
			t.Fatal("redirecting origin was not reached")
		}
		select {
		case request := <-requests:
			t.Fatalf("redirect sent credentials to another origin: %s", request.URL)
		default:
		}
	}
}

func TestSDKDirectAuthProxyOverridesDefaultProxy(t *testing.T) {
	for _, setting := range []string{"direct", "none"} {
		t.Run(setting, func(t *testing.T) {
			var proxyRequests atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxyRequests.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer proxy.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, `{"id":"direct","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"direct-pong"},"finish_reason":"stop"}]}`); err != nil {
					t.Error(err)
				}
			}))
			defer origin.Close()
			engine, core := sdkPolicyEngine(t, origin.URL+"/v1", setting, proxy.URL, &sdkPolicyTransports{})
			response := sdkPolicyRequest(t, engine, "/v1/messages", false)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "direct-pong") || proxyRequests.Load() != 0 {
				t.Fatalf("direct override: status=%d body=%s proxy requests=%d", response.Code, response.Body, proxyRequests.Load())
			}
			found := false
			for _, auth := range core.List() {
				if auth.Attributes["compat_name"] == "sdk-policy-test" {
					found = true
					if auth.ProxyURL != setting {
						t.Fatalf("stored direct override changed: %q", auth.ProxyURL)
					}
				}
			}
			if !found {
				t.Fatal("SDK custom-provider credential disappeared")
			}
		})
	}
}

func TestSDKPolicyLeavesNVIDIATransportOwnershipUnchanged(t *testing.T) {
	auth := &coreauth.Auth{Provider: nvidiaProvider, ProxyURL: "http://127.0.0.1:8080"}
	transports := &sdkPolicyTransports{policy: gwprovider.URLPolicy{DenyPrivate: true}}
	if transports.RoundTripperFor(auth) != nil || auth.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatal("SDK policy replaced NVIDIA transport or proxy configuration")
	}
}
