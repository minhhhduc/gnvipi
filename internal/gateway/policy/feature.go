// Package policy implements feature capability validation and enforcement for the
// gateway translation pipeline.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Mode determines how unsupported features are handled.
type Mode string

const (
	// ModeLenient drops unsupported non-semantic parameters and reports them via
	// the X-Gateway-Dropped-Params response header.
	ModeLenient Mode = "lenient"

	// ModeStrict returns a 400 Bad Request gwerr.KindInvalidRequest error detailing
	// the unsupported features.
	ModeStrict Mode = "strict"
)

// Header and parameter constants.
const (
	// DroppedParamsHeader is the response header listing dropped parameters in lenient mode.
	DroppedParamsHeader = "X-Gateway-Dropped-Params"

	// Parameter names for dropped-parameter reporting and error messages.
	ParamStream             = "stream"
	ParamTools              = "tools"
	ParamParallelToolCalls  = "parallel_tool_calls"
	ParamImages             = "images"
	ParamReasoning          = "reasoning"
	ParamResponseFormat     = "response_format"
	ParamTopK               = "top_k"
	ParamN                  = "n"
	ParamPreviousResponseID = "previous_response_id"
)

// CapabilityOverrides allows per-provider overrides of adapter.Capabilities.
type CapabilityOverrides struct {
	Streaming         *bool `json:"streaming,omitempty"`
	Tools             *bool `json:"tools,omitempty"`
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	Images            *bool `json:"images,omitempty"`
	Reasoning         *bool `json:"reasoning,omitempty"`
	StructuredOutput  *bool `json:"structured_output,omitempty"`
	TopK              *bool `json:"top_k,omitempty"`
	StatefulResponses *bool `json:"stateful_responses,omitempty"`
	CountTokens       *bool `json:"count_tokens,omitempty"`
}

// ApplyOverrides returns a copy of base with non-nil fields from overrides applied.
func ApplyOverrides(base adapter.Capabilities, overrides *CapabilityOverrides) adapter.Capabilities {
	if overrides == nil {
		return base
	}
	caps := base
	if overrides.Streaming != nil {
		caps.Streaming = *overrides.Streaming
	}
	if overrides.Tools != nil {
		caps.Tools = *overrides.Tools
	}
	if overrides.ParallelToolCalls != nil {
		caps.ParallelToolCalls = *overrides.ParallelToolCalls
	}
	if overrides.Images != nil {
		caps.Images = *overrides.Images
	}
	if overrides.Reasoning != nil {
		caps.Reasoning = *overrides.Reasoning
	}
	if overrides.StructuredOutput != nil {
		caps.StructuredOutput = *overrides.StructuredOutput
	}
	if overrides.TopK != nil {
		caps.TopK = *overrides.TopK
	}
	if overrides.StatefulResponses != nil {
		caps.StatefulResponses = *overrides.StatefulResponses
	}
	if overrides.CountTokens != nil {
		caps.CountTokens = *overrides.CountTokens
	}
	return caps
}

// ResolveMode returns providerOverride if non-empty, otherwise global if non-empty,
// defaulting to ModeLenient.
func ResolveMode(global, providerOverride Mode) Mode {
	if providerOverride != "" {
		return providerOverride
	}
	if global != "" {
		return global
	}
	return ModeLenient
}

// Config manages global and per-provider policy modes and capability overrides.
type Config struct {
	DefaultMode       Mode
	ProviderModes     map[string]Mode
	ProviderOverrides map[string]CapabilityOverrides
}

// ModeForProvider returns the effective Mode for a given provider ID.
func (c *Config) ModeForProvider(providerID string) Mode {
	if c != nil && c.ProviderModes != nil {
		if m, ok := c.ProviderModes[providerID]; ok && m != "" {
			return m
		}
	}
	if c != nil && c.DefaultMode != "" {
		return c.DefaultMode
	}
	return ModeLenient
}

// CapabilitiesForProvider returns effective capabilities with per-provider overrides applied.
func (c *Config) CapabilitiesForProvider(providerID string, base adapter.Capabilities) adapter.Capabilities {
	if c != nil && c.ProviderOverrides != nil {
		if o, ok := c.ProviderOverrides[providerID]; ok {
			return ApplyOverrides(base, &o)
		}
	}
	return base
}

// Result is the outcome of feature validation and policy enforcement.
type Result struct {
	// DroppedParams contains the sorted list of dropped parameter names in lenient mode.
	DroppedParams []string
}

// DroppedParamsHeaderValue returns a comma-separated list of dropped parameters,
// or empty string if no parameters were dropped.
func (r *Result) DroppedParamsHeaderValue() string {
	if r == nil || len(r.DroppedParams) == 0 {
		return ""
	}
	return strings.Join(r.DroppedParams, ",")
}

// HasDropped reports whether the named parameter was dropped.
func (r *Result) HasDropped(param string) bool {
	if r == nil {
		return false
	}
	for _, p := range r.DroppedParams {
		if p == param {
			return true
		}
	}
	return false
}

// Validate validates req against caps using mode.
// In ModeLenient (the default), unsupported non-semantic features are stripped from req,
// and their names are collected in Result.DroppedParams (sorted deterministically).
// In ModeStrict, any unsupported feature returns a 400 gwerr.Error (KindInvalidRequest).
// Semantic changes (n > 1, stateful conversation on non-responses) always return a 400 error.
func Validate(req *ir.Request, caps adapter.Capabilities, mode Mode) (*Result, error) {
	if req == nil {
		return &Result{}, nil
	}

	if mode == "" {
		mode = ModeLenient
	}
	if mode != ModeLenient && mode != ModeStrict {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("policy: unknown feature mode %q", mode),
		}
	}

	// Semantic changes that MUST always error regardless of mode.
	if hasNMultiple(req) {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: "multiple choices (n > 1) are unsupported",
		}
	}

	if hasPreviousResponseID(req) && !caps.StatefulResponses {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: "stateful conversation (previous_response_id) is not supported by target upstream",
		}
	}

	if (len(req.Tools) > 0 || req.ToolChoice != nil) && !caps.Tools {
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: "tools are not supported by target upstream",
		}
	}

	// Evaluate non-semantic features against capabilities.
	var unsupported []string

	if req.Stream && !caps.Streaming {
		unsupported = append(unsupported, ParamStream)
	}

	if hasParallelToolCalls(req) && !caps.ParallelToolCalls {
		unsupported = append(unsupported, ParamParallelToolCalls)
	}

	if hasImages(req) && !caps.Images {
		unsupported = append(unsupported, ParamImages)
	}

	if (req.Reasoning != nil || req.ReasoningEffort != "" || req.BudgetTokens != nil) && !caps.Reasoning {
		unsupported = append(unsupported, ParamReasoning)
	}

	if req.ResponseFormat != nil && !caps.StructuredOutput {
		unsupported = append(unsupported, ParamResponseFormat)
	}

	if req.Sampling.TopK != nil && !caps.TopK {
		unsupported = append(unsupported, ParamTopK)
	}

	if len(unsupported) == 0 {
		return &Result{}, nil
	}

	// ModeStrict: return 400 InvalidRequest error detailing all unsupported features.
	if mode == ModeStrict {
		sort.Strings(unsupported)
		var msg string
		if len(unsupported) == 1 {
			msg = fmt.Sprintf("unsupported feature for target upstream: %s", unsupported[0])
		} else {
			msg = fmt.Sprintf("unsupported features for target upstream: %s", strings.Join(unsupported, ", "))
		}
		return nil, &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: msg,
		}
	}

	// ModeLenient: drop unsupported non-semantic parameters from req.
	var dropped []string
	for _, feature := range unsupported {
		switch feature {
		case ParamStream:
			req.Stream = false
			dropped = append(dropped, ParamStream)
		case ParamParallelToolCalls:
			if req.ParallelToolCalls != nil {
				disabled := false
				req.ParallelToolCalls = &disabled
			}
			if req.ToolChoice != nil {
				disabled := true
				req.ToolChoice.DisableParallelToolUse = &disabled
			}
			if req.Extensions != nil {
				delete(req.Extensions, "parallel_tool_calls")
			}
			dropped = append(dropped, ParamParallelToolCalls)
		case ParamImages:
			if err := dropImages(req); err != nil {
				return nil, err
			}
			dropped = append(dropped, ParamImages)
		case ParamReasoning:
			req.Reasoning = nil
			req.ReasoningEffort = ""
			req.BudgetTokens = nil
			if req.Extensions != nil {
				delete(req.Extensions, "reasoning")
				delete(req.Extensions, "reasoning_effort")
			}
			dropped = append(dropped, ParamReasoning)
		case ParamResponseFormat:
			req.ResponseFormat = nil
			if req.Extensions != nil {
				delete(req.Extensions, "response_format")
			}
			dropped = append(dropped, ParamResponseFormat)
		case ParamTopK:
			req.Sampling.TopK = nil
			if req.Extensions != nil {
				delete(req.Extensions, "top_k")
			}
			dropped = append(dropped, ParamTopK)
		}
	}

	sort.Strings(dropped)
	log.Printf("[gateway] dropped unsupported parameters: %s", strings.Join(dropped, ", "))
	return &Result{DroppedParams: dropped}, nil
}

func hasParallelToolCalls(req *ir.Request) bool {
	if req == nil {
		return false
	}
	if req.ParallelToolCalls != nil {
		return *req.ParallelToolCalls
	}
	if req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse != nil {
		return !*req.ToolChoice.DisableParallelToolUse
	}
	if len(req.Extensions) > 0 {
		if raw, ok := req.Extensions["parallel_tool_calls"]; ok {
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil {
				return b
			}
		}
	}
	return false
}

func hasPreviousResponseID(req *ir.Request) bool {
	if req == nil {
		return false
	}
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		return true
	}
	if len(req.Extensions) > 0 {
		if raw, ok := req.Extensions["previous_response_id"]; ok {
			raw = bytes.TrimSpace(raw)
			if len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
				return true
			}
		}
	}
	return false
}

func hasNMultiple(req *ir.Request) bool {
	if req == nil {
		return false
	}
	if req.Sampling.N != nil && *req.Sampling.N > 1 {
		return true
	}
	if len(req.Extensions) > 0 {
		if raw, ok := req.Extensions["n"]; ok {
			var n int
			if err := json.Unmarshal(raw, &n); err == nil && n > 1 {
				return true
			}
		}
	}
	return false
}

func hasImages(req *ir.Request) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		for _, b := range msg.Content {
			if b.Type == ir.BlockImage || b.Image != nil {
				return true
			}
		}
	}
	for _, inst := range req.Instructions {
		for _, b := range inst.Content {
			if b.Type == ir.BlockImage || b.Image != nil {
				return true
			}
		}
	}
	return false
}

func dropImages(req *ir.Request) error {
	if req == nil {
		return nil
	}
	for i := range req.Messages {
		var filtered []ir.Block
		for _, b := range req.Messages[i].Content {
			if b.Type != ir.BlockImage && b.Image == nil {
				filtered = append(filtered, b)
			}
		}
		if len(req.Messages[i].Content) > 0 && len(filtered) == 0 {
			return &gwerr.Error{
				Kind:    gwerr.KindInvalidRequest,
				Status:  http.StatusBadRequest,
				Message: "message contains only image inputs which are not supported by the target provider",
			}
		}
		req.Messages[i].Content = filtered
	}
	for i := range req.Instructions {
		var filtered []ir.Block
		for _, b := range req.Instructions[i].Content {
			if b.Type != ir.BlockImage && b.Image == nil {
				filtered = append(filtered, b)
			}
		}
		if len(req.Instructions[i].Content) > 0 && len(filtered) == 0 {
			return &gwerr.Error{
				Kind:    gwerr.KindInvalidRequest,
				Status:  http.StatusBadRequest,
				Message: "message contains only image inputs which are not supported by the target provider",
			}
		}
		req.Instructions[i].Content = filtered
	}
	return nil
}
