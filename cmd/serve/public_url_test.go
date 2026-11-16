package main

import "testing"

func TestConfiguredPublicURL(t *testing.T) {
	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := m[k]
			return v, ok
		}
	}

	if got := configuredPublicURL(lookup(nil)); got != "" {
		t.Fatalf("empty env = %q, want empty", got)
	}
	if got := configuredPublicURL(lookup(map[string]string{"GNVIPI_PUBLIC_URL": "https://api.example.com/"})); got != "https://api.example.com" {
		t.Fatalf("GNVIPI_PUBLIC_URL = %q", got)
	}
	if got := configuredPublicURL(lookup(map[string]string{"GNVIPI_DOMAIN": "gw.example.com"})); got != "https://gw.example.com" {
		t.Fatalf("GNVIPI_DOMAIN = %q", got)
	}
	if got := configuredPublicURL(lookup(map[string]string{"GNVIPI_DOMAIN": "localhost:8081"})); got != "http://localhost:8081" {
		t.Fatalf("localhost domain = %q", got)
	}
	if got := configuredPublicURL(lookup(map[string]string{
		"GNVIPI_PUBLIC_URL": "http://127.0.0.1:9000",
		"GNVIPI_DOMAIN":     "ignored.example.com",
	})); got != "http://127.0.0.1:9000" {
		t.Fatalf("priority = %q", got)
	}
}
