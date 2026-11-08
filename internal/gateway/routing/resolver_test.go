package routing

import (
	"context"
	"errors"
	"strings"
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

func TestVirtualModelsAutomode(t *testing.T) {
	provList := []provider.Provider{
		{
			ID:       "custom-provider",
			Name:     "custom",
			BaseURL:  "https://custom.example/v1",
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  true,
			Models: []provider.ModelMapping{
				{Alias: "local/verifier", UpstreamModel: "verifier-model"},
				{Alias: "hidden/model", UpstreamModel: "hidden-upstream", Hidden: true},
			},
		},
	}

	// 1. Automode pointing to custom provider
	r, err := NewRegistryResolver(provList, nil, WithVirtualOptions(VirtualOptions{
		AutoModel: "local/verifier",
	}))
	if err != nil {
		t.Fatal(err)
	}

	route, err := r.Resolve(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil {
		t.Fatalf("resolve automode: %v", err)
	}
	if route.Kind != Custom || route.UpstreamModel != "verifier-model" || route.Provider.ID != "custom-provider" {
		t.Fatalf("unexpected route: %+v", route)
	}
	candidates, err := r.ResolveCandidates(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 1 || candidates[0].UpstreamModel != "verifier-model" {
		t.Fatalf("unexpected candidates: %+v %v", candidates, err)
	}

	// 2. Automode pointing to NVIDIA model
	r.UpdateVirtual(VirtualOptions{
		AutoModel: "meta/llama-3.1-70b-instruct",
	})
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil {
		t.Fatalf("resolve automode: %v", err)
	}
	if route.Kind != Fallthrough || route.UpstreamModel != "meta/llama-3.1-70b-instruct" {
		t.Fatalf("unexpected route for nvidia target: %+v", route)
	}

	// 3. Automode pointing to hidden model returns 404
	r.UpdateVirtual(VirtualOptions{
		AutoModel: "hidden/model",
	})
	_, err = r.Resolve(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	var gwErr *gwerr.Error
	if !errors.As(err, &gwErr) || gwErr.Kind != gwerr.KindNotFound || !strings.Contains(gwErr.Message, "automode target is hidden") {
		t.Fatalf("expected hidden error, got: %v", err)
	}

	// 4. Automode on Chat/Responses disabled by default
	r.UpdateVirtual(VirtualOptions{
		AutoModel:              "local/verifier",
		AutoModelChatResponses: false,
	})
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route.Kind != Fallthrough || route.UpstreamModel != "automode" {
		t.Fatalf("expected fallthrough for chat when disabled, got: %+v", route)
	}
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolOpenAIResponses)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route.Kind != Fallthrough || route.UpstreamModel != "automode" {
		t.Fatalf("expected fallthrough for responses when disabled, got: %+v", route)
	}

	// 5. Automode on Chat/Responses enabled via config option
	r.UpdateVirtual(VirtualOptions{
		AutoModel:              "local/verifier",
		AutoModelChatResponses: true,
	})
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolOpenAIChat)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "verifier-model" {
		t.Fatalf("expected custom route when chat enabled, got: %+v err=%v", route, err)
	}
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolOpenAIResponses)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "verifier-model" {
		t.Fatalf("expected custom route when responses enabled, got: %+v err=%v", route, err)
	}
}

func TestVirtualModelsCycle(t *testing.T) {
	provList := []provider.Provider{
		{
			ID:       "prov-1",
			Name:     "p1",
			BaseURL:  "https://p1.example/v1",
			Protocol: ir.ProtocolAnthropicMessages,
			Enabled:  true,
			Models: []provider.ModelMapping{
				{Alias: "custom/one", UpstreamModel: "upstream-1"},
				{Alias: "custom/hidden", UpstreamModel: "upstream-h", Hidden: true},
			},
		},
		{
			ID:       "prov-2",
			Name:     "p2",
			BaseURL:  "https://p2.example/v1",
			Protocol: ir.ProtocolOpenAIChat,
			Enabled:  true,
			Models: []provider.ModelMapping{
				{Alias: "custom/two", UpstreamModel: "upstream-2"},
			},
		},
	}

	cycleModels := []string{"custom/one", "meta/llama-3.1-70b-instruct", "custom/two"}
	r, err := NewRegistryResolver(provList, nil, WithVirtualOptions(VirtualOptions{
		CycleModels: cycleModels,
	}))
	if err != nil {
		t.Fatal(err)
	}

	// First attempt starts at index 0: custom/one
	route, err := r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "upstream-1" {
		t.Fatalf("initial resolve: %+v err=%v", route, err)
	}
	candidates, err := r.ResolveCandidates(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 3 {
		t.Fatalf("candidates len=%d err=%v", len(candidates), err)
	}
	if candidates[0].UpstreamModel != "upstream-1" || candidates[1].UpstreamModel != "meta/llama-3.1-70b-instruct" || candidates[2].UpstreamModel != "upstream-2" {
		t.Fatalf("unexpected candidates order: %+v", candidates)
	}

	// Advance cycle on failure of custom/one
	r.AdvanceCycle("custom/one")

	// Sticky index should now point to candidate 1: meta/llama-3.1-70b-instruct (Fallthrough)
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.Kind != Fallthrough || route.UpstreamModel != "meta/llama-3.1-70b-instruct" {
		t.Fatalf("after advance 1: %+v err=%v", route, err)
	}
	candidates, err = r.ResolveCandidates(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 3 || candidates[0].UpstreamModel != "meta/llama-3.1-70b-instruct" {
		t.Fatalf("candidates after advance 1: %+v", candidates)
	}

	// Non-matching model fails to advance
	r.AdvanceCycle("wrong-model")
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.UpstreamModel != "meta/llama-3.1-70b-instruct" {
		t.Fatalf("advance on wrong model mutated index: %+v err=%v", route, err)
	}

	// Advance past candidate 1 -> candidate 2: custom/two
	r.AdvanceCycle("meta/llama-3.1-70b-instruct")
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "upstream-2" {
		t.Fatalf("after advance 2: %+v err=%v", route, err)
	}

	// Advance past candidate 2 -> wraps around to candidate 0: custom/one
	r.AdvanceCycle("custom/two")
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "upstream-1" {
		t.Fatalf("after wrap: %+v err=%v", route, err)
	}

	// Config change resets cycle index to 0
	r.AdvanceCycle("custom/one") // advance to 1
	r.UpdateVirtual(VirtualOptions{
		CycleModels: []string{"custom/two", "custom/one"},
	})
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.UpstreamModel != "upstream-2" {
		t.Fatalf("config change should reset to index 0, got: %+v", route)
	}

	// Hidden models in cycle are skipped
	r.UpdateVirtual(VirtualOptions{
		CycleModels: []string{"custom/hidden", "custom/one"},
	})
	candidates, err = r.ResolveCandidates(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 1 || candidates[0].UpstreamModel != "upstream-1" {
		t.Fatalf("hidden candidate should be skipped, got: %+v err=%v", candidates, err)
	}

	// Empty cycle returns service unavailable
	r.UpdateVirtual(VirtualOptions{
		CycleModels: nil,
	})
	_, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	var gwErr *gwerr.Error
	if !errors.As(err, &gwErr) || gwErr.Kind != gwerr.KindOverloaded {
		t.Fatalf("expected overloaded (503), got: %v", err)
	}

	// Automode mapped to cycle
	r.UpdateVirtual(VirtualOptions{
		AutoModel:   "cycle",
		CycleModels: []string{"custom/one", "custom/two"},
	})
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil || route.UpstreamModel != "upstream-1" {
		t.Fatalf("automode mapped to cycle resolve: %+v err=%v", route, err)
	}
	candidates, err = r.ResolveCandidates(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("automode mapped to cycle candidates: %+v err=%v", candidates, err)
	}
}

func TestLegacyResolverVirtualModels(t *testing.T) {
	legacyList := []provider.LegacyProvider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Model: "upstream-1", Alias: "custom/one", MessagesOnly: true},
		{Name: "p2", BaseURL: "https://p2.example/v1", Model: "upstream-2", Alias: "custom/two"},
	}
	r, err := NewLegacyResolver(legacyList, nil)
	if err != nil {
		t.Fatal(err)
	}

	r.UpdateVirtual(VirtualOptions{
		AutoModel:              "custom/one",
		AutoModelChatResponses: false,
		CycleModels:            []string{"custom/one", "custom/two"},
	})

	// Automode Anthropic
	route, err := r.Resolve(t.Context(), "automode", ir.ProtocolAnthropicMessages)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "upstream-1" {
		t.Fatalf("legacy automode: %+v err=%v", route, err)
	}

	// Automode Chat disabled
	route, err = r.Resolve(t.Context(), "automode", ir.ProtocolOpenAIChat)
	if err != nil || route.Kind != Fallthrough {
		t.Fatalf("legacy automode chat should fallthrough: %+v", route)
	}

	// Cycle
	candidates, err := r.ResolveCandidates(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("legacy cycle candidates: %+v err=%v", candidates, err)
	}

	r.AdvanceCycle("custom/one")
	route, err = r.Resolve(t.Context(), "cycle", ir.ProtocolAnthropicMessages)
	if err != nil || route.UpstreamModel != "upstream-2" {
		t.Fatalf("legacy cycle advance: %+v err=%v", route, err)
	}
}

func makeTestProvider(id, name, baseURL string, models []provider.ModelMapping) provider.Provider {
	return provider.Provider{
		ID:       id,
		Name:     name,
		BaseURL:  baseURL,
		Protocol: ir.ProtocolOpenAIChat,
		Enabled:  true,
		Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-" + id},
		Models:   models,
	}
}

func TestRegistryResolverPrecedenceTable(t *testing.T) {
	pExact := makeTestProvider("prov-exact", "prov-exact", "https://exact.example.com/v1", []provider.ModelMapping{
		{Alias: "exact-model", UpstreamModel: "upstream-exact"},
		{Alias: "openrouter/special", UpstreamModel: "upstream-special"},
		{Alias: "shared-name", UpstreamModel: "upstream-shared-alias"},
		{Alias: "hidden-alias", UpstreamModel: "upstream-hidden", Hidden: true},
	})
	pWild := makeTestProvider("prov-wild", "prov-wild", "https://wild.example.com/v1", []provider.ModelMapping{
		{Alias: "openrouter/*", UpstreamModel: ""},
		{Alias: "vendor/prefix/*", UpstreamModel: "mapped/*"},
		{Alias: "fixed/*", UpstreamModel: "fixed-target"},
		{Alias: "vendor/special/*", UpstreamModel: "specific-target"},
		{Alias: "hidden-wild/*", UpstreamModel: "", Hidden: true},
	})
	pBare := makeTestProvider("prov-bare", "prov-bare", "https://bare.example.com/v1", []provider.ModelMapping{
		{Alias: "other-alias", UpstreamModel: "bare-only-model"},
		{Alias: "conflict-bare-alias", UpstreamModel: "shared-name"},
		{Alias: "openrouter-m-alias", UpstreamModel: "openrouter/m"},
	})

	providers := []provider.Provider{pExact, pWild, pBare}
	r, err := NewRegistryResolver(providers, []string{"masked-model"})
	if err != nil {
		t.Fatalf("NewRegistryResolver failed: %v", err)
	}

	tests := []struct {
		name         string
		clientModel  string
		wantKind     Kind
		wantProvider string
		wantUpstream string
		wantErrKind  gwerr.Kind
		expectError  bool
	}{
		// 1. Exact alias matches
		{
			name:         "exact alias match",
			clientModel:  "exact-model",
			wantKind:     Custom,
			wantProvider: "prov-exact",
			wantUpstream: "upstream-exact",
		},
		// 2. Exact alias beats wildcard
		{
			name:         "exact alias beats wildcard",
			clientModel:  "openrouter/special",
			wantKind:     Custom,
			wantProvider: "prov-exact",
			wantUpstream: "upstream-special",
		},
		// 3. Exact alias beats bare model
		{
			name:         "exact alias beats bare model",
			clientModel:  "shared-name",
			wantKind:     Custom,
			wantProvider: "prov-exact",
			wantUpstream: "upstream-shared-alias",
		},
		// 4. Wildcard suffix match (openrouter/* -> upstream model suffix)
		{
			name:         "wildcard match with suffix",
			clientModel:  "openrouter/anthropic/claude-3",
			wantKind:     Custom,
			wantProvider: "prov-wild",
			wantUpstream: "anthropic/claude-3",
		},
		// 5. Wildcard with upstream prefix pattern
		{
			name:         "wildcard match with mapped prefix",
			clientModel:  "vendor/prefix/my-model",
			wantKind:     Custom,
			wantProvider: "prov-wild",
			wantUpstream: "mapped/my-model",
		},
		// 6. Wildcard with fixed upstream target
		{
			name:         "wildcard match with fixed target",
			clientModel:  "fixed/any-submodel",
			wantKind:     Custom,
			wantProvider: "prov-wild",
			wantUpstream: "fixed-target",
		},
		// 7. Longest prefix wildcard match (vendor/special/* > vendor/prefix/*)
		{
			name:         "longest prefix wildcard match",
			clientModel:  "vendor/special/fast",
			wantKind:     Custom,
			wantProvider: "prov-wild",
			wantUpstream: "specific-target",
		},
		// 8. Wildcard beats bare upstream model (openrouter/* beats openrouter-m-alias bare model)
		{
			name:         "wildcard beats bare upstream model",
			clientModel:  "openrouter/m",
			wantKind:     Custom,
			wantProvider: "prov-wild",
			wantUpstream: "m",
		},
		// 9. Bare upstream-name match (fallback after exact and wildcard)
		{
			name:         "bare upstream-name match",
			clientModel:  "bare-only-model",
			wantKind:     Custom,
			wantProvider: "prov-bare",
			wantUpstream: "bare-only-model",
		},
		// 10. Hidden exact alias returns 404
		{
			name:        "hidden exact alias returns 404",
			clientModel: "hidden-alias",
			wantErrKind: gwerr.KindNotFound,
			expectError: true,
		},
		// 11. Hidden wildcard returns 404
		{
			name:        "hidden wildcard returns 404",
			clientModel: "hidden-wild/test",
			wantErrKind: gwerr.KindNotFound,
			expectError: true,
		},
		// 12. Model in hidden list returns 404
		{
			name:        "model in hidden list returns 404",
			clientModel: "masked-model",
			wantErrKind: gwerr.KindNotFound,
			expectError: true,
		},
		// 13. Unknown model falls through
		{
			name:        "unknown model falls through",
			clientModel: "some-unknown/model",
			wantKind:    Fallthrough,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route, err := r.Resolve(t.Context(), tc.clientModel, ir.ProtocolOpenAIChat)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected error for %s, got route %+v", tc.clientModel, route)
				}
				var gwErr *gwerr.Error
				if !errors.As(err, &gwErr) || gwErr.Kind != tc.wantErrKind {
					t.Fatalf("expected error kind %v, got %v", tc.wantErrKind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.clientModel, err)
			}
			if route.Kind != tc.wantKind {
				t.Fatalf("expected kind %v, got %v", tc.wantKind, route.Kind)
			}
			if tc.wantKind == Custom {
				if route.Provider == nil || route.Provider.ID != tc.wantProvider {
					t.Fatalf("expected provider %q, got %+v", tc.wantProvider, route.Provider)
				}
				if route.UpstreamModel != tc.wantUpstream {
					t.Fatalf("expected upstream model %q, got %q", tc.wantUpstream, route.UpstreamModel)
				}
			}
		})
	}
}

func TestRegistryResolverDisableBareModels(t *testing.T) {
	p := makeTestProvider("prov-bare", "prov-bare", "https://bare.example.com/v1", []provider.ModelMapping{
		{Alias: "custom-alias", UpstreamModel: "upstream-bare"},
	})
	r, err := NewRegistryResolver([]provider.Provider{p}, nil, WithDisableBareModels(true))
	if err != nil {
		t.Fatal(err)
	}

	// Exact alias resolves
	route, err := r.Resolve(t.Context(), "custom-alias", ir.ProtocolOpenAIChat)
	if err != nil || route.Kind != Custom || route.UpstreamModel != "upstream-bare" {
		t.Fatalf("exact alias failed: route=%+v err=%v", route, err)
	}

	// Bare model lookup must fall through because bare matching is disabled
	route, err = r.Resolve(t.Context(), "upstream-bare", ir.ProtocolOpenAIChat)
	if err != nil || route.Kind != Fallthrough {
		t.Fatalf("expected fallthrough for bare model when disabled: route=%+v err=%v", route, err)
	}
}

func TestRegistryResolverInvalidAndCanceledRequests(t *testing.T) {
	r, err := NewRegistryResolver(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Empty client model
	for _, empty := range []string{"", "   ", "\t\n"} {
		_, err := r.Resolve(t.Context(), empty, ir.ProtocolOpenAIChat)
		var gwErr *gwerr.Error
		if !errors.As(err, &gwErr) || gwErr.Kind != gwerr.KindInvalidRequest {
			t.Fatalf("expected invalid request for empty model %q, got %v", empty, err)
		}
	}

	// Canceled context
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Resolve(ctx, "model", ir.ProtocolOpenAIChat); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestRegistryResolverSaveTimeCollisions(t *testing.T) {
	tests := []struct {
		name        string
		providers   []provider.Provider
		opts        []ResolverOption
		expectError bool
		wantKind    string
	}{
		{
			name: "exact alias collision between two providers",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "shared-alias", UpstreamModel: "m1"},
				}),
				makeTestProvider("p2", "p2", "https://p2.example.com/v1", []provider.ModelMapping{
					{Alias: "shared-alias", UpstreamModel: "m2"},
				}),
			},
			expectError: true,
			wantKind:    "alias",
		},
		{
			name: "wildcard collision between two providers",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "openrouter/*", UpstreamModel: ""},
				}),
				makeTestProvider("p2", "p2", "https://p2.example.com/v1", []provider.ModelMapping{
					{Alias: "openrouter/*", UpstreamModel: ""},
				}),
			},
			expectError: true,
			wantKind:    "wildcard",
		},
		{
			name: "exact alias shadows NVIDIA model without allow_shadow",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "nvidia/nemotron-3.5-lightning-30b-a3b", UpstreamModel: "m"},
				}),
			},
			expectError: true,
			wantKind:    "shadow",
		},
		{
			name: "exact alias shadows NVIDIA model with allow_shadow true",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "nvidia/nemotron-3.5-lightning-30b-a3b", UpstreamModel: "m", AllowShadow: true},
				}),
			},
			expectError: false,
		},
		{
			name: "wildcard alias shadows NVIDIA model without allow_shadow",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "nvidia/*", UpstreamModel: ""},
				}),
			},
			expectError: true,
			wantKind:    "shadow",
		},
		{
			name: "wildcard alias shadows NVIDIA model with allow_shadow true",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "nvidia/*", UpstreamModel: "", AllowShadow: true},
				}),
			},
			expectError: false,
		},
		{
			name: "global WithAllowNvidiaShadow permits shadowing",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "nvidia/nemotron-3.5-lightning-30b-a3b", UpstreamModel: "m"},
				}),
			},
			opts:        []ResolverOption{WithAllowNvidiaShadow(true)},
			expectError: false,
		},
		{
			name: "disabled provider does not cause collision",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "shared", UpstreamModel: "m1"},
				}),
				func() provider.Provider {
					p := makeTestProvider("p2", "p2", "https://p2.example.com/v1", []provider.ModelMapping{
						{Alias: "shared", UpstreamModel: "m2"},
					})
					p.Enabled = false
					return p
				}(),
			},
			expectError: false,
		},
		{
			name: "clean non-colliding providers",
			providers: []provider.Provider{
				makeTestProvider("p1", "p1", "https://p1.example.com/v1", []provider.ModelMapping{
					{Alias: "alias-1", UpstreamModel: "m1"},
				}),
				makeTestProvider("p2", "p2", "https://p2.example.com/v1", []provider.ModelMapping{
					{Alias: "alias-2", UpstreamModel: "m2"},
				}),
			},
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCollisions(tc.providers, tc.opts...)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected collision error, got nil")
				}
				var colErr *CollisionError
				if !errors.As(err, &colErr) {
					t.Fatalf("expected *CollisionError, got %T (%v)", err, err)
				}
				if colErr.Kind != tc.wantKind {
					t.Fatalf("expected collision kind %q, got %q", tc.wantKind, colErr.Kind)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRegistryResolverLoadTimeCollisionsAndDeterministicWinner(t *testing.T) {
	t.Run("exact alias collision picks first provider as deterministic winner", func(t *testing.T) {
		p1 := makeTestProvider("prov-1", "prov-1", "https://p1.example.com/v1", []provider.ModelMapping{
			{Alias: "shared-model", UpstreamModel: "upstream-1"},
		})
		p2 := makeTestProvider("prov-2", "prov-2", "https://p2.example.com/v1", []provider.ModelMapping{
			{Alias: "shared-model", UpstreamModel: "upstream-2"},
			{Alias: "p2-unique", UpstreamModel: "upstream-p2"},
		})

		var warned []string
		r, err := NewRegistryResolver([]provider.Provider{p1, p2}, nil, WithWarningFunc(func(w string) {
			warned = append(warned, w)
		}))
		if err != nil {
			t.Fatalf("load time should succeed despite collision: %v", err)
		}

		if len(warned) == 0 {
			t.Fatalf("expected collision warnings, got none")
		}

		// First provider wins deterministic tie-break
		route, err := r.Resolve(t.Context(), "shared-model", ir.ProtocolOpenAIChat)
		if err != nil || route.Provider.ID != "prov-1" || route.UpstreamModel != "upstream-1" {
			t.Fatalf("expected prov-1 to win deterministic tie-break, got %+v err=%v", route, err)
		}

		// Other models of p2 still resolve
		route, err = r.Resolve(t.Context(), "p2-unique", ir.ProtocolOpenAIChat)
		if err != nil || route.Provider.ID != "prov-2" || route.UpstreamModel != "upstream-p2" {
			t.Fatalf("p2-unique should resolve: %+v err=%v", route, err)
		}
	})

	t.Run("wildcard collision picks first provider as deterministic winner", func(t *testing.T) {
		p1 := makeTestProvider("prov-1", "prov-1", "https://p1.example.com/v1", []provider.ModelMapping{
			{Alias: "openrouter/*", UpstreamModel: "p1/*"},
		})
		p2 := makeTestProvider("prov-2", "prov-2", "https://p2.example.com/v1", []provider.ModelMapping{
			{Alias: "openrouter/*", UpstreamModel: "p2/*"},
		})

		r, err := NewRegistryResolver([]provider.Provider{p1, p2}, nil)
		if err != nil {
			t.Fatal(err)
		}

		route, err := r.Resolve(t.Context(), "openrouter/model", ir.ProtocolOpenAIChat)
		if err != nil || route.Provider.ID != "prov-1" || route.UpstreamModel != "p1/model" {
			t.Fatalf("expected prov-1 to win wildcard, got %+v err=%v", route, err)
		}
	})

	t.Run("NVIDIA model ID protected against custom alias shadowing without allow_shadow", func(t *testing.T) {
		const nvidiaModel = "nvidia/nemotron-3.5-lightning-30b-a3b"
		p := makeTestProvider("prov-custom", "prov-custom", "https://custom.example.com/v1", []provider.ModelMapping{
			{Alias: nvidiaModel, UpstreamModel: "custom-upstream", AllowShadow: false},
			{Alias: "legit-model", UpstreamModel: "custom-upstream"},
		})

		r, err := NewRegistryResolver([]provider.Provider{p}, nil)
		if err != nil {
			t.Fatal(err)
		}

		// Custom alias matching NVIDIA model must fall through so NVIDIA backend handles it
		route, err := r.Resolve(t.Context(), nvidiaModel, ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Fallthrough {
			t.Fatalf("expected Fallthrough for protected NVIDIA model, got %+v err=%v", route, err)
		}

		// Legit custom model resolves to custom provider
		route, err = r.Resolve(t.Context(), "legit-model", ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Custom || route.Provider.ID != "prov-custom" {
			t.Fatalf("expected custom route for legit model: %+v err=%v", route, err)
		}
	})

	t.Run("NVIDIA model ID protected against wildcard shadowing without allow_shadow", func(t *testing.T) {
		const nvidiaModel = "nvidia/nemotron-3.5-lightning-30b-a3b"
		p := makeTestProvider("prov-custom", "prov-custom", "https://custom.example.com/v1", []provider.ModelMapping{
			{Alias: "nvidia/*", UpstreamModel: "", AllowShadow: false},
		})

		r, err := NewRegistryResolver([]provider.Provider{p}, nil)
		if err != nil {
			t.Fatal(err)
		}

		// Real NVIDIA model under nvidia/* must fall through
		route, err := r.Resolve(t.Context(), nvidiaModel, ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Fallthrough {
			t.Fatalf("expected Fallthrough for protected NVIDIA model under wildcard, got %+v err=%v", route, err)
		}

		// Non-NVIDIA model under wildcard routes to custom provider
		route, err = r.Resolve(t.Context(), "nvidia/non-existent-playground-model-xyz", ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Custom || route.Provider.ID != "prov-custom" {
			t.Fatalf("expected custom route for non-NVIDIA model under wildcard, got %+v err=%v", route, err)
		}
	})

	t.Run("NVIDIA model ID shadows when explicitly allowed", func(t *testing.T) {
		const nvidiaModel = "nvidia/nemotron-3.5-lightning-30b-a3b"
		p := makeTestProvider("prov-custom", "prov-custom", "https://custom.example.com/v1", []provider.ModelMapping{
			{Alias: nvidiaModel, UpstreamModel: "custom-upstream", AllowShadow: true},
		})

		r, err := NewRegistryResolver([]provider.Provider{p}, nil)
		if err != nil {
			t.Fatal(err)
		}

		route, err := r.Resolve(t.Context(), nvidiaModel, ir.ProtocolOpenAIChat)
		if err != nil || route.Kind != Custom || route.Provider.ID != "prov-custom" || route.UpstreamModel != "custom-upstream" {
			t.Fatalf("expected custom route when AllowShadow is true, got %+v err=%v", route, err)
		}
	})
}

func TestRegistryResolverConcurrentPublicationAndResolve(t *testing.T) {
	makeProviders := func(tag string) []provider.Provider {
		return []provider.Provider{
			makeTestProvider("prov-a", "prov-a", "https://a.example.com/v1", []provider.ModelMapping{
				{Alias: "exact-alias", UpstreamModel: "upstream-exact-" + tag},
				{Alias: "openrouter/*", UpstreamModel: "or-" + tag + "/*"},
				{Alias: "bare-prov", UpstreamModel: "bare-model-" + tag},
			}),
		}
	}

	r, err := NewRegistryResolver(makeProviders("init"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(5)

	// Writer goroutine
	go func() {
		defer wg.Done()
		for i := range 200 {
			tag := "A"
			if i%2 == 1 {
				tag = "B"
			}
			if err := r.UpdateProviders(makeProviders(tag), nil); err != nil {
				t.Errorf("concurrent update failed: %v", err)
				return
			}
		}
	}()

	// Reader goroutines
	for range 4 {
		go func() {
			defer wg.Done()
			for range 500 {
				// Exact resolve
				route, err := r.Resolve(t.Context(), "exact-alias", ir.ProtocolOpenAIChat)
				if err != nil || route.Kind != Custom || !strings.HasPrefix(route.UpstreamModel, "upstream-exact-") {
					t.Errorf("torn exact route: %+v err=%v", route, err)
					return
				}

				// Wildcard resolve
				route, err = r.Resolve(t.Context(), "openrouter/llama-3", ir.ProtocolOpenAIChat)
				if err != nil || route.Kind != Custom || !strings.HasPrefix(route.UpstreamModel, "or-") || !strings.HasSuffix(route.UpstreamModel, "llama-3") {
					t.Errorf("torn wildcard route: %+v err=%v", route, err)
					return
				}

				// Fallthrough resolve
				route, err = r.Resolve(t.Context(), "unknown-model", ir.ProtocolOpenAIChat)
				if err != nil || route.Kind != Fallthrough {
					t.Errorf("torn fallthrough route: %+v err=%v", route, err)
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestResolverFallbacksResolution(t *testing.T) {
	p1 := makeTestProvider("prov-1", "prov-1", "https://p1.example.com/v1", []provider.ModelMapping{
		{
			Alias:         "primary-model",
			UpstreamModel: "m1",
			Fallbacks:     []string{"backup-model", "tertiary-model", "hidden-model"},
		},
		{
			Alias:         "backup-model",
			UpstreamModel: "m2",
		},
	})
	p2 := makeTestProvider("prov-2", "prov-2", "https://p2.example.com/v1", []provider.ModelMapping{
		{
			Alias:         "tertiary-model",
			UpstreamModel: "m3",
			Fallbacks:     []string{"primary-model"}, // cycle test
		},
		{
			Alias:         "hidden-model",
			UpstreamModel: "m-hidden",
			Hidden:        true,
		},
	})

	r, err := NewRegistryResolver([]provider.Provider{p1, p2}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver failed: %v", err)
	}

	route, err := r.Resolve(t.Context(), "primary-model", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if route.Kind != Custom || route.Provider.ID != "prov-1" || route.UpstreamModel != "m1" {
		t.Fatalf("unexpected primary route: %+v", route)
	}
	if len(route.Fallbacks) != 2 {
		t.Fatalf("expected 2 fallbacks (backup, tertiary), got %d: %+v", len(route.Fallbacks), route.Fallbacks)
	}
	if route.Fallbacks[0].Alias != "backup-model" || route.Fallbacks[0].UpstreamModel != "m2" || route.Fallbacks[0].Provider.ID != "prov-1" {
		t.Errorf("unexpected fallback 0: %+v", route.Fallbacks[0])
	}
	if route.Fallbacks[1].Alias != "tertiary-model" || route.Fallbacks[1].UpstreamModel != "m3" || route.Fallbacks[1].Provider.ID != "prov-2" {
		t.Errorf("unexpected fallback 1: %+v", route.Fallbacks[1])
	}
}

func TestResolverMultiHopFallbacksResolution(t *testing.T) {
	// A -> B -> C -> D with cycle back to A
	p1 := makeTestProvider("prov-1", "prov-1", "https://p1.example.com/v1", []provider.ModelMapping{
		{
			Alias:            "model-a",
			UpstreamModel:    "ma",
			Fallbacks:        []string{"model-b"},
			FallbackStatuses: []int{502, 503},
		},
		{
			Alias:         "model-b",
			UpstreamModel: "mb",
			Fallbacks:     []string{"model-c"},
		},
		{
			Alias:         "model-c",
			UpstreamModel: "mc",
			Fallbacks:     []string{"model-d", "model-a"}, // includes cycle to A
		},
		{
			Alias:         "model-d",
			UpstreamModel: "md",
		},
	})

	r, err := NewRegistryResolver([]provider.Provider{p1}, nil)
	if err != nil {
		t.Fatalf("NewRegistryResolver failed: %v", err)
	}

	route, err := r.Resolve(t.Context(), "model-a", ir.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if route.Alias != "model-a" || route.UpstreamModel != "ma" {
		t.Fatalf("unexpected route: %+v", route)
	}
	if len(route.FallbackStatuses) != 2 || route.FallbackStatuses[0] != 502 || route.FallbackStatuses[1] != 503 {
		t.Errorf("expected FallbackStatuses [502 503], got %v", route.FallbackStatuses)
	}
	if len(route.Fallbacks) != 3 {
		t.Fatalf("expected 3 fallbacks (model-b, model-c, model-d), got %d: %+v", len(route.Fallbacks), route.Fallbacks)
	}
	expectedAliases := []string{"model-b", "model-c", "model-d"}
	for i, want := range expectedAliases {
		if route.Fallbacks[i].Alias != want {
			t.Errorf("fallback %d alias = %q, want %q", i, route.Fallbacks[i].Alias, want)
		}
	}
}
