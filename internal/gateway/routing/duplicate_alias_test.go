package routing

import (
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestLegacyDuplicateAliasKeepsFirstAndOtherBareModel(t *testing.T) {
	list := []provider.LegacyProvider{
		{Name: "p", BaseURL: "https://example.com", Model: "first", Alias: "shared"},
		{Name: "p", BaseURL: "https://example.com", Model: "second", Alias: "shared"},
	}
	r, err := NewLegacyResolver(list, nil)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"shared": "first", "first": "first", "second": "second"} {
		route, err := r.Resolve(t.Context(), model, ir.ProtocolOpenAIChat)
		if err != nil || route.UpstreamModel != want {
			t.Fatalf("%s route=%+v error=%v", model, route, err)
		}
	}
}
