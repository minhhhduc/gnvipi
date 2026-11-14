package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testFunctionID = "3b9748d8-1d85-40e8-8573-0eeaa63a4b63"
	testNamespace  = "qc69jvmznzxy"
)

func writePage(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write page: %v", err)
	}
}

func TestDiscoverCatalogParsesPlaygroundPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "" {
			t.Errorf("models page fetched without the Playground filters")
		}
		writePage(t, w, `
			<a href="/models/community">community</a>
			<a href="/acme/alpha">alpha</a>
			<a href="/acme/beta">beta</a>
			<a href="/assets/icons/logo.svg">asset</a>`)
	})
	mux.HandleFunc("/acme/alpha/playground", func(w http.ResponseWriter, r *http.Request) {
		writePage(t, w, `{\"nvcfFunctionId\":\"`+testFunctionID+`\",\"namespace\":\"`+testNamespace+`\"}`)
	})
	mux.HandleFunc("/acme/beta/playground", func(w http.ResponseWriter, r *http.Request) {
		writePage(t, w, `<html>inference only</html>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	groups, err := (CatalogDiscoverer{BaseURL: server.URL}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(groups) != 1 || groups[0].Publisher != "acme" {
		t.Fatalf("groups = %#v, want one acme group", groups)
	}
	if len(groups[0].Models) != 1 {
		t.Fatalf("models = %#v, want only the playground-capable model", groups[0].Models)
	}
	got := groups[0].Models[0]
	if got.Model != "acme/alpha" || got.Slug != "alpha" || got.Namespace != testNamespace || got.FunctionID != testFunctionID {
		t.Fatalf("row = %#v", got)
	}
}

func TestDiscoverCatalogFailsOnPartialRead(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		writePage(t, w, `<a href="/acme/alpha">alpha</a>`)
	})
	mux.HandleFunc("/acme/alpha/playground", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := (CatalogDiscoverer{BaseURL: server.URL}).Discover(context.Background())
	var discoveryErr *DiscoveryError
	if !errors.As(err, &discoveryErr) {
		t.Fatalf("error = %v, want *DiscoveryError", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("error = %v, want the probe status", err)
	}
}

func TestDiscoverCatalogEmptyModelsPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePage(t, w, `<html>no models</html>`)
	}))
	defer server.Close()

	_, err := (CatalogDiscoverer{BaseURL: server.URL}).Discover(context.Background())
	var discoveryErr *DiscoveryError
	if !errors.As(err, &discoveryErr) {
		t.Fatalf("error = %v, want *DiscoveryError", err)
	}
}
