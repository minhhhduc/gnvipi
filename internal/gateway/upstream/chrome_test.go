package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestChromeRequestCloseUsesDedicatedHTTP2Connection(t *testing.T) {
	closed := make(chan string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("Chrome request protocol = %s", r.Proto)
		}
		if _, err := io.WriteString(w, r.RemoteAddr); err != nil {
			t.Errorf("write connection identity: %v", err)
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- conn.RemoteAddr().String()
		}
	}
	server.StartTLS()
	defer server.Close()
	client := testClient(t, &provider.Provider{ID: "chrome-close", TLS: provider.TLSChrome}, nil)
	chrome := client.Transport.(*timeoutTransport).base.(*chromeTransport)
	chrome.h2.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone() //nolint:staticcheck // SA1019: trust only the local server in the uTLS transport fixture.
	fetch := func(ctx context.Context, closeRequest bool) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Close = closeRequest
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read connection identity: %v; close: %v", readErr, closeErr)
		}
		return string(body)
	}
	pooled := fetch(t.Context(), false)
	dedicated := fetch(t.Context(), true)
	if dedicated == pooled {
		t.Fatal("request Close reused and retired another request's pooled connection")
	}
	if reused := fetch(t.Context(), false); reused != pooled {
		t.Fatalf("ordinary request lost cached connection: first=%q next=%q", pooled, reused)
	}
	select {
	case addr := <-closed:
		if addr != dedicated {
			t.Fatalf("closed connection = %q, want dedicated %q", addr, dedicated)
		}
	case <-time.After(time.Second):
		t.Fatal("dedicated request-close connection remained open")
	}
}
