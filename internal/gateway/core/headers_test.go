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
		"Connection": {"X-Private, x-session-secret"}, "X-Private": {"private"},
		"X-Session-Secret": {"session-secret"}, "User-Agent": {"client-agent"},
		"Anthropic-Beta": {"tools-2024-01-01"}, "X-Session-Id": {"session-id"},
		"X-Custom": {"allowed"}, "X-Unlisted": {"not-allowed"},
	}
	dst.Header.Set("Authorization", "Bearer stale")
	before := src.Header.Clone()
	p := &provider.Provider{Protocol: ir.ProtocolAnthropicMessages, Auth: provider.Auth{Scheme: provider.AuthAPIKey, Key: "provider-secret"}, ForwardHeaders: []string{"X-Custom", "X-Session-Id"}}
	if err := ApplyHeaders(dst, src, p); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Connection", "X-Private", "X-Session-Secret", "X-Unlisted"} {
		if value := dst.Header.Get(name); value != "" {
			t.Errorf("forbidden %s leaked %q", name, value)
		}
	}
	if got := dst.Header.Get("X-Api-Key"); got != "provider-secret" {
		t.Fatalf("credential=%q", got)
	}
	for name, want := range map[string]string{"User-Agent": "client-agent", "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "tools-2024-01-01", "X-Session-Id": "session-id", "X-Custom": "allowed"} {
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
		name, base, agent, auth, version string
		messages                         bool
	}{
		{"chat", "https://example.com/v1", "client", "Authorization", "", false},
		{"messages", "https://example.com", "curl/8.4.0", "X-Api-Key", "client-version", true},
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
			if dst.Header.Get("User-Agent") != tc.agent || dst.Header.Get(tc.auth) != wantAuth || dst.Header.Get("Anthropic-Version") != tc.version {
				t.Fatalf("migrated headers=%v", dst.Header)
			}
		})
	}
}

// A translated request keeps the client's experimental flags only inside their
// own protocol family; the other family would receive a feature name it cannot
// interpret.
func TestApplyHeadersDropsOtherFamilyProtocolHeaders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol ir.Protocol
		header   string
		kept     bool
		forward  bool
	}{
		{"messages-keeps-version", ir.ProtocolAnthropicMessages, "Anthropic-Version", true, false},
		{"messages-keeps-beta", ir.ProtocolAnthropicMessages, "Anthropic-Beta", true, false},
		{"messages-drops-openai-beta", ir.ProtocolAnthropicMessages, "Openai-Beta", false, false},
		{"chat-drops-version", ir.ProtocolOpenAIChat, "Anthropic-Version", false, false},
		{"chat-drops-beta", ir.ProtocolOpenAIChat, "Anthropic-Beta", false, false},
		{"chat-keeps-openai-beta", ir.ProtocolOpenAIChat, "Openai-Beta", true, false},
		{"responses-keeps-openai-beta", ir.ProtocolOpenAIResponses, "Openai-Beta", true, false},
		{"responses-drops-version", ir.ProtocolOpenAIResponses, "Anthropic-Version", false, false},
		// A provider that opts these account headers in must still not hand
		// them to a Messages target.
		{"messages-drops-organization", ir.ProtocolAnthropicMessages, "Openai-Organization", false, true},
		{"messages-drops-project", ir.ProtocolAnthropicMessages, "Openai-Project", false, true},
		{"chat-keeps-organization", ir.ProtocolOpenAIChat, "Openai-Organization", true, true},
		{"responses-keeps-project", ir.ProtocolOpenAIResponses, "Openai-Project", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := headerTestRequest(t), headerTestRequest(t)
			src.Header.Set(tc.header, "client-value")
			p := &provider.Provider{Protocol: tc.protocol, Auth: provider.Auth{Scheme: provider.AuthBearer, Key: "provider-secret"}}
			if tc.forward {
				p.ForwardHeaders = []string{tc.header}
			}
			if err := ApplyHeaders(dst, src, p); err != nil {
				t.Fatal(err)
			}
			got := dst.Header.Get(tc.header)
			if tc.kept && got != "client-value" {
				t.Fatalf("%s=%q, want the client value", tc.header, got)
			}
			if !tc.kept && got != "" {
				t.Fatalf("%s=%q leaked into the %s family", tc.header, got, tc.protocol)
			}
			if tc.protocol == ir.ProtocolAnthropicMessages && dst.Header.Get("Anthropic-Version") == "" {
				t.Fatal("a Messages target lost its protocol version")
			}
		})
	}
}
