package main

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
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
			configured, err := gatewayoutbound.Configure(outbound, p)
			if err != nil {
				if configurator, ok := outbound.(interface {
					WithProvider(*gatewayprovider.Provider) adapter.Outbound
				}); ok {
					configured = configurator.WithProvider(p)
				} else {
					configured = outbound
				}
			}
			caps = configured.Capabilities()
		}
	}
	if caps.IsZero() {
		switch p.Protocol {
		case ir.ProtocolAnthropicMessages:
			caps = adapter.Capabilities{
				Streaming:         true,
				Tools:             true,
				ParallelToolCalls: true,
				Images:            adapter.ImageCapabilityBoth,
				Reasoning:         true,
				TopK:              true,
				CountTokens:       true,
			}
			if p != nil && hasStructuredOutputExtension(p.Extensions) {
				caps.StructuredOutput = true
			}
		case ir.ProtocolOpenAIChat:
			caps = adapter.Capabilities{
				Streaming:        true,
				Images:           adapter.ImageCapabilityBoth,
				Reasoning:        true,
				StructuredOutput: true,
				TopK:             true,
			}
		case ir.ProtocolOpenAIResponses:
			caps = adapter.Capabilities{
				Streaming:        true,
				Images:           adapter.ImageCapabilityBoth,
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

func hasStructuredOutputExtension(ext map[string]json.RawMessage) bool {
	if len(ext) == 0 {
		return false
	}
	if raw, ok := ext["tool_emulation"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil && enabled {
			return true
		}
	}
	if raw, ok := ext["anthropic"]; ok {
		var opts struct {
			ToolEmulation  bool `json:"tool_emulation"`
			JSONModePrompt bool `json:"json_mode_prompt"`
		}
		if err := json.Unmarshal(raw, &opts); err == nil && (opts.ToolEmulation || opts.JSONModePrompt) {
			return true
		}
	}
	if raw, ok := ext["structured_output"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil && enabled {
			return true
		}
		var mode string
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch mode {
			case "tool", "emulation", "tool_emulation", "prompt":
				return true
			}
		}
	}
	return false
}

// resolveNvidiaCapabilities derives capabilities for build.nvidia.com playground models.
func resolveNvidiaCapabilities(modelID string) adapter.Capabilities {
	caps := adapter.Capabilities{
		Streaming: true,
	}
	if info, err := models.Lookup(modelID); err == nil && info.Capability != nil {
		caps.Tools = info.Capability.ToolCalling
		caps.StructuredOutput = info.Capability.StructuredOutput
		if info.Capability.Vision {
			caps.Images = adapter.ImageCapabilityBoth
		} else {
			caps.Images = adapter.ImageCapabilityNone
		}
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
	fromNvidia := make(map[string]bool)

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
			fromNvidia[m.ID] = true
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
			type row struct {
				provider *gatewayprovider.Provider
				mapping  *gatewayprovider.ModelMapping
				index    int
			}
			var rows []row
			rowIndex := 0
			for i := range providers {
				p := &providers[i]
				if !p.Enabled {
					continue
				}
				for j := range p.Models {
					rows = append(rows, row{
						provider: p,
						mapping:  &p.Models[j],
						index:    rowIndex,
					})
					rowIndex++
				}
			}

			slices.SortStableFunc(rows, func(a, b row) int {
				first, second := a.mapping.LegacyPosition, b.mapping.LegacyPosition
				if first == 0 && second != 0 {
					return 1
				}
				if second == 0 && first != 0 {
					return -1
				}
				if first != 0 && second != 0 {
					if c := cmp.Compare(first, second); c != 0 {
						return c
					}
				}
				return cmp.Compare(a.index, b.index)
			})

			for _, current := range rows {
				p := current.provider
				m := current.mapping
				alias := m.Alias
				if alias == "" || m.Hidden || prefs.isHidden(alias) {
					continue
				}
				if m.UpstreamModel != "" && !strings.Contains(m.UpstreamModel, "*") && prefs.isHidden(m.UpstreamModel) {
					if !m.AllowShadow || !fromNvidia[alias] {
						continue
					}
				}
				isWildcard := strings.HasSuffix(alias, "/*")
				if isWildcard && cfg.WildcardPolicy == WildcardPolicyOmit {
					continue
				}

				pCaps := resolveProviderCapabilities(p, cfg.Registry)
				ownedBy := p.Name
				if ownedBy == "" {
					ownedBy = p.ID
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
					if m.AllowShadow && fromNvidia[alias] {
						out[idx] = entry
						fromNvidia[alias] = false
					}
					continue
				}

				seen[alias] = len(out)
				out = append(out, entry)
			}
		}
	}

	return out
}
