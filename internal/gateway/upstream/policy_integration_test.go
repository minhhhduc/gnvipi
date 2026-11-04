package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestCachedClientPreservesGuardedRedirect(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	policy := provider.URLPolicy{}
	options := &Options{DisableProxy: true, DialContext: policy.Dialer(nil, nil, nil), CheckRedirect: policy.CheckRedirect}
	factory := NewFactory()
	defer factory.CloseIdleConnections()
	p := &provider.Provider{ID: "guarded-redirect"}
	for range 2 {
		client, err := factory.NewClient(p, options)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", "fixture-secret")
		response, err := client.Do(req)
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if err == nil {
			t.Fatal("cached factory followed a cross-origin credential-bearing redirect")
		}
	}
	if reached.Load() != 0 {
		t.Fatal("redirect target received a guarded request")
	}
}
