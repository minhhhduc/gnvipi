package main

import (
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestRejectedProviderCandidateDoesNotPoisonPreferences(t *testing.T) {
	old := customProvider{Name: "p", BaseURL: "https://example.com", Model: "first", Alias: "alias"}
	p := &modelPrefs{providers: []customProvider{old}}
	r, err := p.modelResolver()
	if err != nil {
		t.Fatal(err)
	}
	bad := old
	bad.BaseURL = "invalid"
	if err := p.setProviders([]customProvider{bad}); err == nil {
		t.Fatal("invalid provider accepted")
	}
	if got := p.listProviders(); len(got) != 1 || got[0] != old {
		t.Fatalf("rejected candidate changed live providers: %+v", got)
	}
	route, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
	if err != nil || route.UpstreamModel != "first" {
		t.Fatalf("rejected candidate changed route: %+v %v", route, err)
	}
	if err := p.setHidden([]string{"alias"}); err != nil {
		t.Fatalf("rejection poisoned later preference save: %v", err)
	}
}
