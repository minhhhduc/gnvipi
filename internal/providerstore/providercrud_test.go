package providerstore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// TestProviderRowFromV2MapsDefinitionOnly proves the v2 bridge keeps provider
// configuration and leaves credentials and models to their own tables.
func TestProviderRowFromV2MapsDefinitionOnly(t *testing.T) {
	in := provider.Provider{
		ID:             "v2-provider",
		Name:           "V2",
		BaseURL:        "https://v2.example/v1",
		ProxyURL:       "http://proxy.example:8080",
		Protocol:       ir.ProtocolOpenAIChat,
		Auth:           provider.Auth{Scheme: provider.AuthBearer, Key: "sk-must-not-be-stored"},
		Headers:        map[string]string{"X-Tenant": "acme"},
		Models:         []provider.ModelMapping{{Alias: "v2/model", UpstreamModel: "upstream"}},
		Timeouts:       provider.Timeouts{Connect: 5 * time.Second},
		Enabled:        true,
		TLS:            provider.TLSChrome,
		ForwardHeaders: []string{"X-Request-Id"},
		EndpointPaths:  map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/chat"},
		ExtraBody:      []byte(`{"reasoning":{"effort":"low"}}`),
		Extensions:     map[string]json.RawMessage{"capability": json.RawMessage(`"chat"`)},
		MaxConcurrency: 4,
		QueueTimeout:   2 * time.Second,
	}

	row, err := ProviderRowFromV2(in)
	if err != nil {
		t.Fatalf("ProviderRowFromV2: %v", err)
	}
	if row.ID != in.ID || row.Name != in.Name || row.Protocol != "openai-chat" ||
		row.BaseURL != in.BaseURL || row.TLS != "chrome" || !row.Enabled ||
		row.MaxConcurrency != 4 || row.QueueTimeout != int64(2*time.Second) {
		t.Fatalf("provider row = %+v", row)
	}
	if len(row.Headers) == 0 || len(row.EndpointPaths) == 0 || len(row.ExtraBody) == 0 {
		t.Fatalf("provider row dropped definition fields: %+v", row)
	}
	if string(row.ExtraBody) == "null" || string(row.EndpointPaths) == "null" {
		t.Fatalf("provider row kept null JSONB defaults: %+v", row)
	}
}

func TestProviderRowFromV2RejectsUnrepresentableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   provider.Provider
	}{
		{
			name: "bearer header override",
			in: provider.Provider{
				ID: "p", Name: "P", BaseURL: "https://p.example/v1", Protocol: ir.ProtocolOpenAIChat,
				Auth: provider.Auth{Scheme: provider.AuthBearer, HeaderName: "X-Custom"},
			},
		},
		{
			name: "credential in base url",
			in: provider.Provider{
				ID: "p", Name: "P", BaseURL: "https://user:pass@p.example/v1", Protocol: ir.ProtocolOpenAIChat,
			},
		},
		{
			name: "credential header",
			in: provider.Provider{
				ID: "p", Name: "P", BaseURL: "https://p.example/v1", Protocol: ir.ProtocolOpenAIChat,
				Headers: map[string]string{"Authorization": "Bearer secret"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ProviderRowFromV2(tc.in); err == nil {
				t.Fatal("ProviderRowFromV2 accepted unrepresentable configuration")
			}
		})
	}
}

func TestProviderRowFromV2ReportsFieldError(t *testing.T) {
	_, err := ProviderRowFromV2(provider.Provider{
		ID: "p", Name: "P", BaseURL: "https://p.example/v1", Protocol: ir.ProtocolOpenAIChat,
		Auth: provider.Auth{Scheme: provider.AuthBearer, HeaderName: "X-Custom"},
	})
	var fieldErr *provider.FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "auth.header_name" {
		t.Fatalf("error = %v, want auth.header_name field error", err)
	}
}
