package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestCustomProtocolStaysOutboundOnly(t *testing.T) {
	p := validProvider()
	p.Protocol = "custom:vendor-v2"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := ir.ParseProtocol(p.Protocol.String()); err == nil {
		t.Fatal("custom adapter became an inbound wire protocol")
	}
	for _, name := range []string{"custom:", "custom:../vendor", "custom: vendor", "custom:vendor/name", "custom:Vendor"} {
		p.Protocol = ir.Protocol(name)
		if err := p.Validate(); err == nil {
			t.Errorf("accepted invalid custom protocol %q", name)
		}
	}
}

func TestValidateOutboundExtensionBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Provider)
		field  string
	}{
		{"external endpoint", func(p *Provider) {
			p.EndpointPaths = map[ir.Protocol]string{ir.ProtocolOpenAIChat: "https://evil.invalid/chat"}
		}, "endpoint_path[openai-chat]"},
		{"network path", func(p *Provider) {
			p.EndpointPaths = map[ir.Protocol]string{ir.ProtocolOpenAIChat: "//evil.invalid/chat"}
		}, "endpoint_path[openai-chat]"},
		{"traversal", func(p *Provider) { p.EndpointPaths = map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/../chat"} }, "endpoint_path[openai-chat]"},
		{"escaped traversal", func(p *Provider) { p.EndpointPaths = map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/%2e%2e/chat"} }, "endpoint_path[openai-chat]"},
		{"query", func(p *Provider) { p.EndpointPaths = map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/chat?key=secret"} }, "endpoint_path[openai-chat]"},
		{"unknown endpoint protocol", func(p *Provider) { p.EndpointPaths = map[ir.Protocol]string{"custom:vendor": "/chat"} }, "endpoint_path[custom:vendor]"},
		{"non-object extras", func(p *Provider) { p.ExtraBody = json.RawMessage(`[]`) }, "extra_body"},
		{"null extras", func(p *Provider) { p.ExtraBody = json.RawMessage(`null`) }, "extra_body"},
		{"malformed extras", func(p *Provider) { p.ExtraBody = json.RawMessage(`{"x":`) }, "extra_body"},
		{"model override", func(p *Provider) { p.ExtraBody = json.RawMessage(`{"model":"wrong"}`) }, "extra_body"},
		{"stream override", func(p *Provider) { p.ExtraBody = json.RawMessage(`{"stream":false}`) }, "extra_body"},
		{"auth host", func(p *Provider) { p.Auth.HeaderName = "host" }, "auth.header_name"},
		{"auth framing", func(p *Provider) { p.Auth.HeaderName = "Content-Length" }, "auth.header_name"},
		{"auth hop header", func(p *Provider) { p.Auth.HeaderName = "Proxy-Authorization" }, "auth.header_name"},
		{"auth injection", func(p *Provider) { p.Auth.HeaderName = "X-Key\r\nHost" }, "auth.header_name"},
		{"auth without scheme", func(p *Provider) { p.Auth = Auth{HeaderName: "X-Key"} }, "auth.header_name"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p := validProvider()
			tt.mutate(&p)
			fields := validationFields(t, p.Validate())
			for _, field := range fields {
				if field == tt.field {
					return
				}
			}
			t.Fatalf("fields %v do not contain %s", fields, tt.field)
		})
	}
}

func TestOutboundExtensionJSONConfiguration(t *testing.T) {
	p := validProvider()
	if err := json.Unmarshal([]byte(`{"endpoint_path":{"openai-chat":"/api/chat"},"extra_body":{"tenant":"a"},"auth":{"Scheme":"bearer","Key":"configured-but-not-resolved","header_name":"x-vendor-key"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.EndpointPaths[ir.ProtocolOpenAIChat] != "/api/chat" || p.Auth.HeaderName != "X-Vendor-Key" {
		t.Fatalf("extension config not decoded/canonicalized: %#v", p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"endpoint_path":`, `"extra_body":`, `"header_name":`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("JSON omits %s: %s", field, encoded)
		}
	}
}
