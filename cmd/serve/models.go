package main

import (
	"encoding/json"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/policy"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// WildcardPolicy defines how wildcard model aliases are exposed in the /v1/models catalog.
type WildcardPolicy string

const (
	// WildcardPolicyPlaceholder includes wildcard aliases (e.g. "openrouter/*")
	// as documentation placeholder entries so callers can discover supported prefix patterns.
	WildcardPolicyPlaceholder WildcardPolicy = "placeholder"
	// WildcardPolicyOmit excludes wildcard aliases from the advertised model catalog.
	WildcardPolicyOmit WildcardPolicy = "omit"
)

// CatalogOptions configures catalog generation.
type CatalogOptions struct {
	WildcardPolicy WildcardPolicy
	Registry       *adapter.Registry
}

// CatalogOption is a functional option for CatalogOptions.
type CatalogOption func(*CatalogOptions)

// WithWildcardPolicy sets the wildcard documentation entry policy.
func WithWildcardPolicy(p WildcardPolicy) CatalogOption {
	return func(o *CatalogOptions) {
		o.WildcardPolicy = p
	}
}

// WithAdapterRegistry provides an adapter registry for looking up outbound capabilities.
func WithAdapterRegistry(r *adapter.Registry) CatalogOption {
	return func(o *CatalogOptions) {
		o.Registry = r
	}
}

// gatewayModel represents one entry in the /v1/models discovery catalog.
// Extension fields (upstream_protocol, capabilities) are non-breaking and omitted when empty.
type gatewayModel struct {
	Type             string                `json:"type"`
	Object           string                `json:"object"`
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	DisplayName      string                `json:"display_name"`
	CreatedAt        string                `json:"created_at,omitempty"`
	Created          int64                 `json:"created"`
	OwnedBy          string                `json:"owned_by"`
	UpstreamProtocol string                `json:"upstream_protocol,omitempty"`
	Capabilities     *adapter.Capabilities `json:"capabilities,omitempty"`
}

// resolveProviderCapabilities calculates the effective capabilities for a provider,
// combining adapter defaults for its protocol with any provider-level capability overrides.
func resolveProviderCapabilities(p *gatewayprovider.Provider, registry *adapter.Registry) adapter.Capabilities {
	var caps adapter.Capabilities
	if registry != nil {
		if outbound, ok := registry.Outbound(p.Protocol); ok {
			caps = outbound.Capabilities()
		}
	}
	if caps == (adapter.Capabilities{}) {
		switch p.Protocol {
		case ir.ProtocolAnthropicMessages:
			caps = adapter.Capabilities{
				Streaming:         true,
				Tools:             true,
				ParallelToolCalls: true,
				Images:            true,
				Reasoning:         true,
				TopK:              true,
				CountTokens:       true,
			}
		case ir.ProtocolOpenAIChat:
			caps = adapter.Capabilities{
				Streaming:        true,
				Images:           true,
				Reasoning:        true,
				StructuredOutput: true,
				TopK:             true,
			}
		case ir.ProtocolOpenAIResponses:
			caps = adapter.Capabilities{
				Streaming:        true,
				Images:           true,
				Reasoning:        true,
				StructuredOutput: true,
			}
		}
	}
	if len(p.Extensions) > 0 {
		if rawCaps, ok := p.Extensions["capabilities"]; ok && len(rawCaps) > 0 {
			var ov policy.CapabilityOverrides
			if err := json.Unmarshal(rawCaps, &ov); err == nil {
				caps = policy.ApplyOverrides(caps, &ov)
			}
		}
	}
	return caps
}

// resolveNvidiaCapabilities derives capabilities for build.nvidia.com playground models.
func resolveNvidiaCapabilities(modelID string) adapter.Capabilities {
	caps := adapter.Capabilities{
		Streaming: true,
	}
	if info, err := models.Lookup(modelID); err == nil && info.Capability != nil {
		caps.Tools = info.Capability.ToolCalling
		caps.StructuredOutput = info.Capability.StructuredOutput
		caps.Images = info.Capability.Vision
		caps.Reasoning = info.Capability.Reasoning
	}
	return caps
}

// buildCatalog builds the discovery catalog from virtual models, NVIDIA models,
// and visible v2 provider aliases, applying masking and collision precedence.
func buildCatalog(prefs *modelPrefs, getNvidiaModels func() []*cliproxy.ModelInfo, opts ...CatalogOption) []gatewayModel {
	cfg := CatalogOptions{
		WildcardPolicy: WildcardPolicyPlaceholder,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	out := make([]gatewayModel, 0, 64)
	seen := make(map[string]int) // alias -> index in out

	// 1. Virtual models
	virtualModels := []string{"automode", "cycle"}
	for _, id := range virtualModels {
		if prefs != nil && prefs.isHidden(id) {
			continue
		}
		seen[id] = len(out)
		out = append(out, gatewayModel{
			Type:        "virtual",
			Object:      "model",
			ID:          id,
			Name:        id,
			DisplayName: id,
			OwnedBy:     "gateway",
		})
	}

	// 2. NVIDIA models
	if getNvidiaModels != nil {
		for _, m := range getNvidiaModels() {
			if m == nil {
				continue
			}
			if prefs != nil && prefs.isHidden(m.ID) {
				continue
			}
			if _, exists := seen[m.ID]; exists {
				continue
			}
			caps := resolveNvidiaCapabilities(m.ID)
			seen[m.ID] = len(out)
			out = append(out, gatewayModel{
				Type:             "model",
				Object:           "model",
				ID:               m.ID,
				Name:             m.ID,
				DisplayName:      m.ID,
				OwnedBy:          m.OwnedBy,
				Created:          m.Created,
				UpstreamProtocol: string(ir.ProtocolOpenAIChat),
				Capabilities:     &caps,
			})
		}
	}

	// 3. V2 Provider visible aliases
	if prefs != nil {
		providers, err := prefs.listProvidersV2()
		if err == nil {
			for i := range providers {
				p := &providers[i]
				if !p.Enabled {
					continue
				}
				pCaps := resolveProviderCapabilities(p, cfg.Registry)
				ownedBy := p.Name
				if ownedBy == "" {
					ownedBy = p.ID
				}
				for _, m := range p.Models {
					alias := m.Alias
					if alias == "" || m.Hidden || prefs.isHidden(alias) {
						continue
					}
					isWildcard := strings.HasSuffix(alias, "/*")
					if isWildcard && cfg.WildcardPolicy == WildcardPolicyOmit {
						continue
					}

					capsCopy := pCaps
					entry := gatewayModel{
						Type:             "model",
						Object:           "model",
						ID:               alias,
						Name:             alias,
						DisplayName:      alias,
						OwnedBy:          ownedBy,
						UpstreamProtocol: string(p.Protocol),
						Capabilities:     &capsCopy,
					}

					if idx, exists := seen[alias]; exists {
						if m.AllowShadow {
							out[idx] = entry
						}
						continue
					}

					seen[alias] = len(out)
					out = append(out, entry)
				}
			}
		}
	}

	return out
}
