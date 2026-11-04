package core

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func headerTestRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://upstream.example/v1/messages", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestApplyHeadersRejectsClientCredentialsAndConnectionHeaders(t *testing.T) {
	src, dst := headerTestRequest(t), headerTestRequest(t)
	src.Header = http.Header{
		"aUtHoRiZaTiOn": {"Bearer client-secret"}, "X-Api-Key": {"client-key"},
		"Cookie": {"session=secret"}, "Proxy-Authorization": {"proxy-secret"},
		"Connection": {"X-Private, x-opencode-secret"}, "X-Private": {"private"},
		"X-Opencode-Secret": {"session-secret"}, "User-Agent": {"client-agent"},
		"Anthropic-Beta": {"tools-2024-01-01"}, "X-Opencode-Session": {"session-id"},
		"X-Custom": {"allowed"}, "X-Unlisted": {"not-allowed"},
	}
	dst.Header.Set("Authorization", "Bearer stale")
	before := src.Header.Clone()
	p := &provider.Provider{Protocol: ir.ProtocolAnthropicMessages, Auth: provider.Auth{Scheme: provider.AuthAPIKey, Key: "provider-secret"}, ForwardHeaders: []string{"X-Custom"}}
	if err := ApplyHeaders(dst, src, p); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Connection", "X-Private", "X-Opencode-Secret", "X-Unlisted"} {
		if value := dst.Header.Get(name); value != "" {
			t.Errorf("forbidden %s leaked %q", name, value)
		}
	}
	if got := dst.Header.Get("X-Api-Key"); got != "provider-secret" {
		t.Fatalf("credential=%q", got)
	}
	for name, want := range map[string]string{"User-Agent": "client-agent", "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "tools-2024-01-01", "X-Opencode-Session": "session-id", "X-Custom": "allowed"} {
		if got := dst.Header.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}
	if !reflect.DeepEqual(src.Header, before) {
		t.Fatal("source was mutated")
	}
}

func TestApplyHeadersStaticPrecedenceAndEnvironmentAuth(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "environment-key")
	src, dst := headerTestRequest(t), headerTestRequest(t)
	src.Header.Set("User-Agent", "client")
	src.Header.Set("Anthropic-Version", "client-version")
	p := &provider.Provider{Protocol: ir.ProtocolAnthropicMessages, Auth: provider.Auth{Scheme: provider.AuthBearer, KeyEnv: "TEST_UPSTREAM_KEY"}, Headers: map[string]string{"User-Agent": "provider", "anthropic-version": "provider-version"}}
	if err := ApplyHeaders(dst, src, p); err != nil {
		t.Fatal(err)
	}
	if dst.Header.Get("Authorization") != "Bearer environment-key" || dst.Header.Get("User-Agent") != "provider" || dst.Header.Get("Anthropic-Version") != "provider-version" {
		t.Fatalf("headers=%v", dst.Header)
	}
	if _, ok := p.Headers["anthropic-version"]; !ok {
		t.Fatal("provider was mutated")
	}
	p.Auth.KeyEnv = "TEST_MISSING_UPSTREAM_KEY"
	t.Setenv(p.Auth.KeyEnv, "")
	if err := ApplyHeaders(headerTestRequest(t), src, p); err == nil {
		t.Fatal("missing credential environment was accepted")
	}
}

func TestApplyHeadersRejectsUnsafeConfiguration(t *testing.T) {
	for _, name := range []string{"Host", "Content-Length", "Connection", "Cookie", "Authorization", "X-Api-Key"} {
		t.Run(name, func(t *testing.T) {
			p := &provider.Provider{Headers: map[string]string{name: "unsafe"}}
			if err := ApplyHeaders(headerTestRequest(t), headerTestRequest(t), p); err == nil {
				t.Fatal("unsafe static header accepted")
			}
			p.Headers = nil
			p.ForwardHeaders = []string{name}
			if err := ApplyHeaders(headerTestRequest(t), headerTestRequest(t), p); err == nil {
				t.Fatal("unsafe forwarding accepted")
			}
		})
	}
}

func TestMigratedProviderHeadersPreserveLegacyModes(t *testing.T) {
	for _, tc := range []struct {
		name, base, agent, auth string
		messages                bool
	}{
		{"chat", "https://example.com/v1", "client", "Authorization", false},
		{"agentrouter", "https://agentrouter.org/v1", "opencode/1.18.25", "Authorization", false},
		{"messages", "https://example.com", "curl/8.4.0", "X-Api-Key", true},
		{"messages-agentrouter", "https://agentrouter.org", "curl/8.4.0", "X-Api-Key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := provider.FromLegacy([]provider.LegacyProvider{{Name: tc.name, BaseURL: tc.base, Model: "model", APIKey: "key", MessagesOnly: tc.messages}})
			if err != nil {
				t.Fatal(err)
			}
			src, dst := headerTestRequest(t), headerTestRequest(t)
			src.Header.Set("User-Agent", "client")
			src.Header.Set("Anthropic-Version", "client-version")
			if err := ApplyHeaders(dst, src, &list[0]); err != nil {
				t.Fatal(err)
			}
			wantAuth := "key"
			if tc.auth == "Authorization" {
				wantAuth = "Bearer key"
			}
			if dst.Header.Get("User-Agent") != tc.agent || dst.Header.Get(tc.auth) != wantAuth || dst.Header.Get("Anthropic-Version") != "client-version" {
				t.Fatalf("migrated headers=%v", dst.Header)
			}
		})
	}
}
