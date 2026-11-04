package main

import (
	"errors"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestModelPrefsPublishesResolverBeforeReloadCallbacks(t *testing.T) {
	p := &modelPrefs{providers: []customProvider{{Name: "p", BaseURL: "https://example.com", Model: "old", Alias: "alias"}}}
	r, err := p.modelResolver()
	if err != nil {
		t.Fatal(err)
	}
	observed := false
	p.onChange = func(_ []customProvider) {
		route, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
		if err != nil || route.UpstreamModel != "new" {
			t.Errorf("reload observed stale route: %+v %v", route, err)
		}
		observed = true
	}
	if err := p.setProviders([]customProvider{{Name: "p", BaseURL: "https://example.com", Model: "new", Alias: "alias"}}); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("provider reload callback was lost")
	}
	if err := p.addDeleted("alias"); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve(t.Context(), "new", ir.ProtocolAnthropicMessages)
	var gatewayErr *gwerr.Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Kind != gwerr.KindNotFound {
		t.Fatalf("deleted alias not masked through bare model: %v", err)
	}
	if err := p.removeDeleted("alias"); err != nil {
		t.Fatal(err)
	}
	if err := p.setHidden([]string{"alias"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIResponses); err == nil {
		t.Fatal("manual mask not published")
	}
	if err := p.setHidden(nil); err != nil {
		t.Fatal(err)
	}
	route, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
	if err != nil || route.UpstreamModel != "new" {
		t.Fatalf("restored route=%+v error=%v", route, err)
	}
}
