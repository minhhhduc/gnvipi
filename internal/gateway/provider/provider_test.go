package provider

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func validProvider() Provider {
	return Provider{
		ID:       "vendor",
		Name:     "vendor",
		BaseURL:  "https://up.invalid/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     Auth{Scheme: AuthBearer, Key: "sk-test"},
		Headers:  map[string]string{"x-title": "gnvipi"},
		Models:   []ModelMapping{{Alias: "vendor/m", UpstreamModel: "m"}},
		Enabled:  true,
	}
}

func validationFields(t *testing.T, err error) []string {
	t.Helper()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	fields := make([]string, len(invalid.Errors))
	for i, e := range invalid.Errors {
		fields[i] = e.Field
	}
	return fields
}

func TestValidateAcceptsValidProvider(t *testing.T) {
	p := validProvider()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := p.Headers["X-Title"], "gnvipi"; got != want {
		t.Fatalf("header name not canonicalized: %#v", p.Headers)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Provider)
		field  string
	}{
		{"empty id", func(p *Provider) { p.ID = "  " }, "id"},
		{"empty name", func(p *Provider) { p.Name = "" }, "name"},
		{"unknown protocol", func(p *Provider) { p.Protocol = "graphql" }, "protocol"},
		{"url without scheme", func(p *Provider) { p.BaseURL = "up.invalid/v1" }, "base_url"},
		{"non-http scheme", func(p *Provider) { p.BaseURL = "ftp://up.invalid/v1" }, "base_url"},
		{"url with query", func(p *Provider) { p.BaseURL = "https://up.invalid/v1?x=1" }, "base_url"},
		{"unknown tls profile", func(p *Provider) { p.TLS = "firefox" }, "tls"},
		{"unknown auth scheme", func(p *Provider) { p.Auth = Auth{Scheme: "basic"} }, "auth.scheme"},
		{"key and key env", func(p *Provider) { p.Auth.KeyEnv = "VENDOR_KEY" }, "auth.key"},
		{"auth without credential", func(p *Provider) { p.Auth = Auth{Scheme: AuthBearer} }, "auth.key"},
		{"credential without scheme", func(p *Provider) { p.Auth = Auth{Key: "sk-test"} }, "auth.key"},
		{"hop-by-hop header", func(p *Provider) { p.Headers = map[string]string{"Connection": "keep-alive"} }, "headers[Connection]"},
		{"invalid header name", func(p *Provider) { p.Headers = map[string]string{"Bad Header": "x"} }, "headers[Bad Header]"},
		{"invalid header value", func(p *Provider) { p.Headers = map[string]string{"X-Foo": "a\nb"} }, "headers[X-Foo]"},
		{"duplicate header spellings", func(p *Provider) {
			p.Headers = map[string]string{"x-foo": "a", "X-Foo": "b"}
		}, "headers[X-Foo]"},
		{"empty alias", func(p *Provider) { p.Models[0].Alias = "" }, "models[0].alias"},
		{"alias with whitespace", func(p *Provider) { p.Models[0].Alias = "vendor/m m" }, "models[0].alias"},
		{"wildcard without prefix", func(p *Provider) {
			p.Models[0] = ModelMapping{Alias: "/*", UpstreamModel: "m"}
		}, "models[0].alias"},
		{"inner wildcard", func(p *Provider) { p.Models[0].Alias = "vendor/*/m" }, "models[0].alias"},
		{"empty upstream model", func(p *Provider) {
			p.Models[0] = ModelMapping{Alias: "vendor/m"}
		}, "models[0].upstream_model"},
		{"duplicate alias", func(p *Provider) {
			p.Models = append(p.Models, ModelMapping{Alias: "vendor/m", UpstreamModel: "m2"})
		}, "models[1].alias"},
		{"negative timeout", func(p *Provider) { p.Timeouts.Connect = -time.Second }, "timeouts.connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProvider()
			tt.mutate(&p)
			fields := validationFields(t, p.Validate())
			for _, got := range fields {
				if got == tt.field {
					return
				}
			}
			t.Fatalf("fields = %v, want %q", fields, tt.field)
		})
	}
}

func TestValidateAllowsWildcardMapping(t *testing.T) {
	p := validProvider()
	p.Models = []ModelMapping{{Alias: "vendor/*"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateAllowsZeroValuedOptionals(t *testing.T) {
	// Auth scheme, TLS profile and timeouts may all be left unset.
	p := validProvider()
	p.Auth = Auth{}
	p.TLS = ""
	p.Timeouts = Timeouts{}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateIsDeterministic(t *testing.T) {
	p := validProvider()
	p.ID = ""
	p.Name = ""
	p.BaseURL = "nope"
	p.Auth = Auth{Scheme: "basic"}
	p.Headers = map[string]string{"Zz": "1", "Aa": "2", "Mm": "3", "Connection": "x"}
	p.Models = []ModelMapping{{Alias: ""}, {Alias: ""}}

	first, second := p.Validate(), p.Validate()
	if first == nil {
		t.Fatal("expected validation errors")
	}
	if first.Error() != second.Error() {
		t.Fatalf("validation is not deterministic:\n%s\n%s", first, second)
	}
	joined := first.Error()
	// Sorted header names keep the message stable regardless of map order.
	if !strings.Contains(joined, "first:") {
		t.Fatalf("message = %q", joined)
	}
}

func TestValidateReportsDuplicateHeadersBeforeValueCheck(t *testing.T) {
	// One spelling carries an invalid value, so whichever value the randomized
	// map iteration keeps, the duplicate must be what gets reported.
	for i := 0; i < 100; i++ {
		p := validProvider()
		p.Headers = map[string]string{"x-foo": "ok", "X-Foo": "bad\nval"}
		err := p.Validate()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := err.Error(); !strings.Contains(got, "set more than once under different spellings") {
			t.Fatalf("iteration %d: error = %q", i, got)
		}
	}
}

func TestFromLegacyProtocols(t *testing.T) {
	tests := []struct {
		name  string
		flags LegacyProvider
		want  ir.Protocol
	}{
		{"no flags", LegacyProvider{}, ir.ProtocolOpenAIChat},
		{"messages only", LegacyProvider{MessagesOnly: true}, ir.ProtocolAnthropicMessages},
		{"responses only", LegacyProvider{ResponsesOnly: true}, ir.ProtocolOpenAIResponses},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.flags
			in.Name, in.BaseURL, in.Model = "vendor", "https://up.invalid/v1", "m"
			out, err := FromLegacy([]LegacyProvider{in})
			if err != nil {
				t.Fatalf("FromLegacy: %v", err)
			}
			if len(out) != 1 || out[0].Protocol != tt.want {
				t.Fatalf("providers = %#v, want protocol %q", out, tt.want)
			}
		})
	}
}

func TestFromLegacyRejectsBothFlags(t *testing.T) {
	_, err := FromLegacy([]LegacyProvider{{
		Name: "vendor", BaseURL: "https://up.invalid/v1", Model: "m",
		MessagesOnly: true, ResponsesOnly: true,
	}})
	fields := validationFields(t, err)
	if len(fields) != 1 || fields[0] != "protocol" {
		t.Fatalf("fields = %v, want one protocol error", fields)
	}
}

func TestFromLegacyMergesEntriesWithTheSameIdentity(t *testing.T) {
	out, err := FromLegacy([]LegacyProvider{
		{Name: "vendor", BaseURL: "https://up.invalid/v1", APIKey: "k", Model: "m1"},
		{Name: "vendor", BaseURL: "https://up.invalid/v1", APIKey: "k", Model: "m2"},
	})
	if err != nil {
		t.Fatalf("FromLegacy: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("providers = %d, want 1", len(out))
	}
	got := []string{out[0].Models[0].Alias, out[0].Models[1].Alias}
	if want := []string{"vendor/m1", "vendor/m2"}; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("aliases = %v, want %v", got, want)
	}
	if out[0].Auth.Scheme != AuthBearer || out[0].Auth.Key != "k" {
		t.Fatalf("auth = %#v, want bearer with the legacy key", out[0].Auth)
	}
	if !out[0].Enabled {
		t.Fatal("a converted provider must be enabled")
	}
}

func TestFromLegacyKeepsDifferentEndpointsApart(t *testing.T) {
	out, err := FromLegacy([]LegacyProvider{
		{Name: "vendor", BaseURL: "https://a.invalid/v1", Model: "m1"},
		{Name: "vendor", BaseURL: "https://b.invalid/v1", Model: "m2"},
	})
	if err != nil {
		t.Fatalf("FromLegacy: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("providers = %d, want 2", len(out))
	}
	if out[0].ID != "vendor" || out[1].ID != "vendor-2" {
		t.Fatalf("ids = %q, %q, want vendor, vendor-2", out[0].ID, out[1].ID)
	}
}

func TestFromLegacyKeepsDifferentProtocolsApart(t *testing.T) {
	out, err := FromLegacy([]LegacyProvider{
		{Name: "vendor", BaseURL: "https://up.invalid/v1", APIKey: "k", Model: "m1"},
		{Name: "vendor", BaseURL: "https://up.invalid/v1", APIKey: "k", Model: "m2", ResponsesOnly: true},
	})
	if err != nil {
		t.Fatalf("FromLegacy: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("providers = %d, want 2", len(out))
	}
	if out[0].Protocol != ir.ProtocolOpenAIChat || out[1].Protocol != ir.ProtocolOpenAIResponses {
		t.Fatalf("protocols = %q, %q, want openai-chat and openai-responses", out[0].Protocol, out[1].Protocol)
	}
	if out[0].ID != "vendor" || out[1].ID != "vendor-2" {
		t.Fatalf("ids = %q, %q, want vendor, vendor-2", out[0].ID, out[1].ID)
	}
}

func TestFromLegacyAliasDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   LegacyProvider
		want string
	}{
		{"explicit alias wins", LegacyProvider{Name: "vendor", Model: "m", Alias: "custom"}, "custom"},
		{"already namespaced", LegacyProvider{Name: "vendor", Model: "vendor/m"}, "vendor/m"},
		{"plain model", LegacyProvider{Name: "vendor", Model: "m"}, "vendor/m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			in.BaseURL = "https://up.invalid/v1"
			out, err := FromLegacy([]LegacyProvider{in})
			if err != nil {
				t.Fatalf("FromLegacy: %v", err)
			}
			if got := out[0].Models[0].Alias; got != tt.want {
				t.Fatalf("alias = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFromLegacyValidatesConvertedProviders(t *testing.T) {
	// A legacy entry without a model produces an alias but no upstream model.
	_, err := FromLegacy([]LegacyProvider{{Name: "vendor", BaseURL: "https://up.invalid/v1"}})
	fields := validationFields(t, err)
	if len(fields) != 1 || fields[0] != "models[0].upstream_model" {
		t.Fatalf("fields = %v, want one models[0].upstream_model error", fields)
	}
	if !strings.Contains(err.Error(), "provider vendor:") {
		t.Fatalf("error = %q, want it addressed to the converted provider", err)
	}
}
