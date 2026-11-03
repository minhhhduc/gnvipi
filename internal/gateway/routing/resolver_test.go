package routing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestLegacyResolverPreservesFlatOrderAndMaskPrecedence(t *testing.T) {
	list := []provider.LegacyProvider{
		{Name: "first", BaseURL: "https://first.example/v1", Model: "m", Alias: "alias"},
		{Name: "second", BaseURL: "https://second.example/v1", Model: "x"},
		{Name: "first", BaseURL: "https://first.example/v1", Model: "x", Alias: "late"},
	}
	r, err := NewLegacyResolver(list, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []ir.Protocol{ir.ProtocolOpenAIChat, ir.ProtocolAnthropicMessages, ir.ProtocolOpenAIResponses} {
		for _, tc := range []struct{ model, name, upstream string }{{"alias", "first", "m"}, {"m", "first", "m"}, {"x", "second", "x"}, {"late", "first", "x"}} {
			route, err := r.Resolve(t.Context(), tc.model, protocol)
			if err != nil || route.Kind != Custom || route.Provider.Name != tc.name || route.UpstreamModel != tc.upstream {
				t.Fatalf("model=%s route=%+v error=%v", tc.model, route, err)
			}
		}
	}
	for _, model := range []string{"unknown", "nvidia/model", "first/m"} {
		route, err := r.Resolve(t.Context(), model, ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Fallthrough {
			t.Fatalf("model=%s route=%+v error=%v", model, route, err)
		}
	}
	if err := r.Update(list, []string{"alias"}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"alias", "m"} {
		_, err := r.Resolve(t.Context(), model, ir.ProtocolOpenAIChat)
		var gatewayErr *gwerr.Error
		if !errors.As(err, &gatewayErr) || gatewayErr.Kind != gwerr.KindNotFound {
			t.Fatalf("hidden %s error=%v", model, err)
		}
	}
	if err := r.Update(list, []string{"m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat); err != nil {
		t.Fatalf("bare-name mask incorrectly hid full alias: %v", err)
	}
	_, err = r.Resolve(t.Context(), "m", ir.ProtocolOpenAIChat)
	if err == nil {
		t.Fatal("bare-name mask was ignored")
	}
}

func TestLegacyResolverSnapshotAndErrors(t *testing.T) {
	list := []provider.LegacyProvider{{Name: "p", BaseURL: "https://example.com", Model: "model", Alias: "alias", APIKey: "original"}}
	r, err := NewLegacyResolver(list, nil)
	if err != nil {
		t.Fatal(err)
	}
	list[0].APIKey = "changed-after-publication"
	route, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
	if err != nil || route.Provider.Auth.Key != "original" {
		t.Fatalf("snapshot mutated: %+v %v", route, err)
	}
	list[0].BaseURL = "invalid"
	if err := r.Update(list, nil); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	route, err = r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
	if err != nil || route.Provider.Auth.Key != "original" {
		t.Fatalf("failed update replaced snapshot: %+v %v", route, err)
	}
	_, err = r.Resolve(t.Context(), " ", ir.ProtocolOpenAIChat)
	var gatewayErr *gwerr.Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Kind != gwerr.KindInvalidRequest {
		t.Fatalf("empty model error=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Resolve(ctx, "alias", ir.ProtocolOpenAIChat); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolve=%v", err)
	}
}

func TestLegacyResolverConcurrentPublication(t *testing.T) {
	makeList := func(name string) []provider.LegacyProvider {
		return []provider.LegacyProvider{{Name: "p", BaseURL: "https://example.com", Model: name, Alias: "alias", APIKey: name}}
	}
	r, err := NewLegacyResolver(makeList("A"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		for range 200 {
			if err := r.Update(makeList("B"), nil); err != nil {
				t.Error(err)
				return
			}
			if err := r.Update(makeList("A"), nil); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 4 {
		go func() {
			defer wg.Done()
			for range 500 {
				route, err := r.Resolve(t.Context(), "alias", ir.ProtocolOpenAIChat)
				if err != nil || route.Provider.Auth.Key != route.UpstreamModel {
					t.Errorf("torn route=%+v error=%v", route, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
