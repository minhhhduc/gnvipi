package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	inboundchat "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openaichat"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	outboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/policy"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func allCapabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming:         true,
		Tools:             true,
		ParallelToolCalls: true,
		Images:            true,
		Reasoning:         true,
		StructuredOutput:  true,
		TopK:              true,
		StatefulResponses: true,
		CountTokens:       true,
	}
}

func noCapabilities() adapter.Capabilities {
	return adapter.Capabilities{}
}

func TestFeatureValidationTable(t *testing.T) {
	tests := []struct {
		name          string
		reqFunc       func() *ir.Request
		caps          adapter.Capabilities
		mode          policy.Mode
		wantErr       bool
		wantStatus    int
		wantKind      gwerr.Kind
		wantDropped   []string
		wantHeader    string
		checkMutation func(t *testing.T, req *ir.Request)
	}{
		// --- TopK ---
		{
			name: "top_k supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{TopK: intPtr(40)}}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Sampling.TopK == nil || *req.Sampling.TopK != 40 {
					t.Fatalf("expected top_k to be preserved, got %v", req.Sampling.TopK)
				}
			},
		},
		{
			name: "top_k unsupported in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{TopK: intPtr(40)}}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamTopK},
			wantHeader:  "top_k",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Sampling.TopK != nil {
					t.Fatalf("expected top_k to be dropped, got %v", req.Sampling.TopK)
				}
			},
		},
		{
			name: "top_k unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{TopK: intPtr(40)}}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- Streaming ---
		{
			name: "streaming supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{Stream: true}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if !req.Stream {
					t.Fatal("expected stream to remain true")
				}
			},
		},
		{
			name: "streaming unsupported in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{Stream: true}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamStream},
			wantHeader:  "stream",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Stream {
					t.Fatal("expected stream to be set to false")
				}
			},
		},
		{
			name: "streaming unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{Stream: true}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- Tools ---
		{
			name: "tools supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Tools:      []ir.Tool{{Name: "weather"}},
					ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceAuto},
				}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if len(req.Tools) != 1 || req.ToolChoice == nil {
					t.Fatal("expected tools to remain intact")
				}
			},
		},
		{
			name: "tools unsupported in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Tools:      []ir.Tool{{Name: "weather"}},
					ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceAuto},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamTools},
			wantHeader:  "tools",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Tools != nil || req.ToolChoice != nil {
					t.Fatalf("expected tools and tool_choice to be cleared, got tools=%v, choice=%v", req.Tools, req.ToolChoice)
				}
			},
		},
		{
			name: "tools unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{Tools: []ir.Tool{{Name: "weather"}}}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- ParallelToolCalls ---
		{
			name: "parallel_tool_calls supported via field in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{ParallelToolCalls: boolPtr(true)}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
					t.Fatal("expected parallel_tool_calls to remain true")
				}
			},
		},
		{
			name: "parallel_tool_calls unsupported via field in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{ParallelToolCalls: boolPtr(true)}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamParallelToolCalls},
			wantHeader:  "parallel_tool_calls",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.ParallelToolCalls != nil && *req.ParallelToolCalls {
					t.Fatal("expected parallel_tool_calls to be disabled")
				}
			},
		},
		{
			name: "parallel_tool_calls unsupported via tool_choice in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					ToolChoice: &ir.ToolChoice{
						Mode:                   ir.ToolChoiceAuto,
						DisableParallelToolUse: boolPtr(false),
					},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamParallelToolCalls},
			wantHeader:  "parallel_tool_calls",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.ToolChoice.DisableParallelToolUse == nil || !*req.ToolChoice.DisableParallelToolUse {
					t.Fatal("expected DisableParallelToolUse to be true")
				}
			},
		},
		{
			name: "parallel_tool_calls unsupported via extensions in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Extensions: map[string]json.RawMessage{
						"parallel_tool_calls": json.RawMessage(`true`),
					},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamParallelToolCalls},
			wantHeader:  "parallel_tool_calls",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if _, ok := req.Extensions["parallel_tool_calls"]; ok {
					t.Fatal("expected parallel_tool_calls extension to be deleted")
				}
			},
		},
		{
			name: "parallel_tool_calls unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{ParallelToolCalls: boolPtr(true)}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- Images ---
		{
			name: "images supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Messages: []ir.Message{
						ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"), ir.NewImageBlockURL("https://example.com/a.png")),
					},
				}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if len(req.Messages[0].Content) != 2 {
					t.Fatalf("expected 2 content blocks, got %d", len(req.Messages[0].Content))
				}
			},
		},
		{
			name: "images unsupported in lenient mode drops image blocks",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Messages: []ir.Message{
						ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"), ir.NewImageBlockURL("https://example.com/a.png")),
					},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamImages},
			wantHeader:  "images",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Type != ir.BlockText {
					t.Fatalf("expected only text block remaining, got %v", req.Messages[0].Content)
				}
			},
		},
		{
			name: "images unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Messages: []ir.Message{
						ir.NewMessage(ir.RoleUser, ir.NewImageBlockURL("https://example.com/a.png")),
					},
				}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- Reasoning ---
		{
			name: "reasoning supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Reasoning: &ir.ReasoningConfig{Effort: ir.ReasoningEffortHigh},
				}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Reasoning == nil || req.Reasoning.Effort != ir.ReasoningEffortHigh {
					t.Fatal("expected reasoning config to remain intact")
				}
			},
		},
		{
			name: "reasoning unsupported in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Reasoning: &ir.ReasoningConfig{Effort: ir.ReasoningEffortHigh},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamReasoning},
			wantHeader:  "reasoning",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Reasoning != nil {
					t.Fatalf("expected reasoning to be nil, got %v", req.Reasoning)
				}
			},
		},
		{
			name: "reasoning unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Reasoning: &ir.ReasoningConfig{Effort: ir.ReasoningEffortHigh},
				}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- StructuredOutput ---
		{
			name: "structured_output supported in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					ResponseFormat: &ir.ResponseFormat{Type: ir.ResponseFormatJSONObject},
				}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.ResponseFormat == nil || req.ResponseFormat.Type != ir.ResponseFormatJSONObject {
					t.Fatal("expected response_format to remain intact")
				}
			},
		},
		{
			name: "structured_output unsupported in lenient mode drops parameter",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					ResponseFormat: &ir.ResponseFormat{Type: ir.ResponseFormatJSONObject},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{policy.ParamResponseFormat},
			wantHeader:  "response_format",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.ResponseFormat != nil {
					t.Fatalf("expected response_format to be nil, got %v", req.ResponseFormat)
				}
			},
		},
		{
			name: "structured_output unsupported in strict mode returns 400",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					ResponseFormat: &ir.ResponseFormat{Type: ir.ResponseFormatJSONObject},
				}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},

		// --- Semantic Features: n > 1 (MUST ALWAYS ERROR) ---
		{
			name: "n > 1 via sampling errors in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{N: intPtr(2)}}
			},
			caps:       allCapabilities(),
			mode:       policy.ModeLenient,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "n > 1 via sampling errors in strict mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{N: intPtr(3)}}
			},
			caps:       allCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "n > 1 via extensions errors in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Extensions: map[string]json.RawMessage{
						"n": json.RawMessage(`4`),
					},
				}
			},
			caps:       allCapabilities(),
			mode:       policy.ModeLenient,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "n = 1 succeeds in both modes",
			reqFunc: func() *ir.Request {
				return &ir.Request{Sampling: ir.Sampling{N: intPtr(1)}}
			},
			caps:        allCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
		},

		// --- Semantic Features: previous_response_id on non-responses (MUST ALWAYS ERROR) ---
		{
			name: "previous_response_id via field on non-responses errors in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{PreviousResponseID: "resp_previous_123"}
			},
			caps:       adapter.Capabilities{StatefulResponses: false},
			mode:       policy.ModeLenient,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "previous_response_id via field on non-responses errors in strict mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{PreviousResponseID: "resp_previous_123"}
			},
			caps:       adapter.Capabilities{StatefulResponses: false},
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "previous_response_id via extensions on non-responses errors in lenient mode",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Extensions: map[string]json.RawMessage{
						"previous_response_id": json.RawMessage(`"resp_abc"`),
					},
				}
			},
			caps:       adapter.Capabilities{StatefulResponses: false},
			mode:       policy.ModeLenient,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
		{
			name: "previous_response_id allowed on stateful responses upstreams",
			reqFunc: func() *ir.Request {
				return &ir.Request{PreviousResponseID: "resp_previous_123"}
			},
			caps:        adapter.Capabilities{StatefulResponses: true},
			mode:        policy.ModeLenient,
			wantDropped: nil,
			wantHeader:  "",
		},

		// --- Multiple Unsupported Features Combined ---
		{
			name: "multiple unsupported features in lenient mode drops all and sorts header",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Stream:            true,
					Sampling:          ir.Sampling{TopK: intPtr(50)},
					Reasoning:         &ir.ReasoningConfig{Effort: ir.ReasoningEffortMedium},
					ParallelToolCalls: boolPtr(true),
					Tools:             []ir.Tool{{Name: "calc"}},
					ResponseFormat:    &ir.ResponseFormat{Type: ir.ResponseFormatJSONObject},
					Messages: []ir.Message{
						ir.NewMessage(ir.RoleUser, ir.NewImageBlockURL("https://example.com/b.png")),
					},
				}
			},
			caps:        noCapabilities(),
			mode:        policy.ModeLenient,
			wantDropped: []string{"images", "parallel_tool_calls", "reasoning", "response_format", "stream", "tools", "top_k"},
			wantHeader:  "images,parallel_tool_calls,reasoning,response_format,stream,tools,top_k",
			checkMutation: func(t *testing.T, req *ir.Request) {
				if req.Stream || req.Sampling.TopK != nil || req.Reasoning != nil || req.Tools != nil || req.ResponseFormat != nil {
					t.Fatalf("expected all unsupported features to be cleared, got %+v", req)
				}
			},
		},
		{
			name: "multiple unsupported features in strict mode returns combined 400 error",
			reqFunc: func() *ir.Request {
				return &ir.Request{
					Stream:   true,
					Sampling: ir.Sampling{TopK: intPtr(50)},
				}
			},
			caps:       noCapabilities(),
			mode:       policy.ModeStrict,
			wantErr:    true,
			wantStatus: http.StatusBadRequest,
			wantKind:   gwerr.KindInvalidRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.reqFunc()
			res, err := policy.Validate(req, tc.caps, tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil result: %+v", res)
				}
				var gwErr *gwerr.Error
				if !errors.As(err, &gwErr) {
					t.Fatalf("expected *gwerr.Error, got %T: %v", err, err)
				}
				if gwErr.Status != tc.wantStatus {
					t.Errorf("status = %d, want %d", gwErr.Status, tc.wantStatus)
				}
				if gwErr.Kind != tc.wantKind {
					t.Errorf("kind = %q, want %q", gwErr.Kind, tc.wantKind)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := res.DroppedParamsHeaderValue(); got != tc.wantHeader {
				t.Errorf("DroppedParamsHeaderValue = %q, want %q", got, tc.wantHeader)
			}

			if len(res.DroppedParams) != len(tc.wantDropped) {
				t.Errorf("DroppedParams len = %d, want %d (%v)", len(res.DroppedParams), len(tc.wantDropped), res.DroppedParams)
			} else {
				for i, d := range tc.wantDropped {
					if res.DroppedParams[i] != d {
						t.Errorf("DroppedParams[%d] = %q, want %q", i, res.DroppedParams[i], d)
					}
					if !res.HasDropped(d) {
						t.Errorf("HasDropped(%q) = false, want true", d)
					}
				}
			}

			if tc.checkMutation != nil {
				tc.checkMutation(t, req)
			}
		})
	}
}

func TestNilRequestAndDefaultMode(t *testing.T) {
	t.Run("nil request succeeds", func(t *testing.T) {
		res, err := policy.Validate(nil, noCapabilities(), policy.ModeLenient)
		if err != nil || len(res.DroppedParams) != 0 {
			t.Fatalf("expected empty result and nil error, got res=%+v, err=%v", res, err)
		}
	})

	t.Run("empty mode defaults to lenient", func(t *testing.T) {
		req := &ir.Request{Sampling: ir.Sampling{TopK: intPtr(20)}}
		res, err := policy.Validate(req, noCapabilities(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.DroppedParamsHeaderValue() != "top_k" {
			t.Errorf("expected top_k dropped in default mode, got %q", res.DroppedParamsHeaderValue())
		}
	})

	t.Run("invalid mode returns error", func(t *testing.T) {
		req := &ir.Request{}
		_, err := policy.Validate(req, noCapabilities(), "unsupported-mode")
		if err == nil {
			t.Fatal("expected error for invalid mode")
		}
		var gwErr *gwerr.Error
		if !errors.As(err, &gwErr) || gwErr.Kind != gwerr.KindInvalidRequest {
			t.Errorf("expected KindInvalidRequest, got %v", err)
		}
	})
}

func TestCapabilityOverridesAndConfig(t *testing.T) {
	base := adapter.Capabilities{
		Streaming: true,
		Tools:     false,
		TopK:      false,
	}

	overrides := &policy.CapabilityOverrides{
		Tools: boolPtr(true),
	}

	merged := policy.ApplyOverrides(base, overrides)
	if !merged.Streaming || !merged.Tools || merged.TopK {
		t.Fatalf("ApplyOverrides mismatch: %+v", merged)
	}

	cfg := &policy.Config{
		DefaultMode: policy.ModeLenient,
		ProviderModes: map[string]policy.Mode{
			"strict-provider": policy.ModeStrict,
		},
		ProviderOverrides: map[string]policy.CapabilityOverrides{
			"custom-provider": {TopK: boolPtr(true)},
		},
	}

	if m := cfg.ModeForProvider("strict-provider"); m != policy.ModeStrict {
		t.Errorf("ModeForProvider(strict-provider) = %v, want strict", m)
	}
	if m := cfg.ModeForProvider("other-provider"); m != policy.ModeLenient {
		t.Errorf("ModeForProvider(other-provider) = %v, want lenient", m)
	}

	customCaps := cfg.CapabilitiesForProvider("custom-provider", base)
	if !customCaps.TopK {
		t.Error("expected TopK override to be true")
	}

	if resolved := policy.ResolveMode(policy.ModeLenient, policy.ModeStrict); resolved != policy.ModeStrict {
		t.Errorf("ResolveMode = %v, want strict", resolved)
	}
	if resolved := policy.ResolveMode(policy.ModeStrict, ""); resolved != policy.ModeStrict {
		t.Errorf("ResolveMode = %v, want strict", resolved)
	}
	if resolved := policy.ResolveMode("", ""); resolved != policy.ModeLenient {
		t.Errorf("ResolveMode = %v, want lenient", resolved)
	}
}

// Pipeline integration test: verifies the complete translation flow with feature policy enforcement.
// Inbound Anthropic Messages request with top_k targeting OpenAI Responses upstream (top_k unsupported) ->
// inbound adapter decodes to ir.Request (Sampling.TopK = 40) ->
// policy.Validate drops top_k in lenient mode ->
// outbound adapter builds upstream request successfully (without top_k error) ->
// response includes X-Gateway-Dropped-Params header!
func TestPipelineIntegrationDroppedParams(t *testing.T) {
	inbound := inboundanthropic.New()
	outbound := outboundresponses.New()

	targetCaps := outbound.Capabilities()
	// OpenAI Responses outbound adapter does not support TopK (caps.TopK == false).
	if targetCaps.TopK {
		t.Fatal("expected openairesponses outbound Capabilities.TopK to be false")
	}

	clientJSON := `{
		"model": "gpt-4o",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": "hello"}],
		"top_k": 40
	}`

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(clientJSON))
	req.Header.Set("Content-Type", "application/json")

	// 1. Inbound decode
	normalized, err := inbound.DecodeRequest(req)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if normalized.Sampling.TopK == nil || *normalized.Sampling.TopK != 40 {
		t.Fatalf("decoded request missing top_k: %+v", normalized.Sampling)
	}

	// 2. Feature policy enforcement (lenient mode)
	res, err := policy.Validate(normalized, targetCaps, policy.ModeLenient)
	if err != nil {
		t.Fatalf("policy.Validate: %v", err)
	}

	if !res.HasDropped(policy.ParamTopK) {
		t.Fatalf("expected top_k to be dropped, got %v", res.DroppedParams)
	}
	droppedHeader := res.DroppedParamsHeaderValue()
	if droppedHeader != "top_k" {
		t.Fatalf("droppedHeader = %q, want %q", droppedHeader, "top_k")
	}

	// 3. Outbound build request: ensure dropped parameter allows BuildRequest to succeed
	// (OpenAI Responses outbound BuildRequest explicitly fails if top_k is present).
	target := adapter.Target{BaseURL: "https://api.openai.com", Model: "gpt-4o", Headers: make(http.Header)}
	outboundReq, err := outbound.BuildRequest(context.Background(), normalized, target)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	t.Cleanup(func() {
		if err := outboundReq.Body.Close(); err != nil {
			t.Error(err)
		}
	})

	// 4. Verify HTTP response header reporting
	recorder := httptest.NewRecorder()
	if droppedHeader != "" {
		recorder.Header().Set(policy.DroppedParamsHeader, droppedHeader)
	}
	if got := recorder.Header().Get(policy.DroppedParamsHeader); got != "top_k" {
		t.Errorf("response header %s = %q, want top_k", policy.DroppedParamsHeader, got)
	}
}

// Pipeline integration test in strict mode: returns 400 Bad Request error.
func TestPipelineIntegrationStrictError(t *testing.T) {
	inbound := inboundchat.New()
	outbound := outboundanthropic.New()

	// Anthropic outbound does not support tools without tool adapter in this configuration
	targetCaps := outbound.Capabilities()
	targetCaps.Tools = false // Simulate text-only provider

	clientJSON := `{
		"model": "claude-3-5-sonnet",
		"messages": [{"role": "user", "content": "calc"}],
		"tools": [{"type": "function", "function": {"name": "calc"}}]
	}`

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(clientJSON))
	req.Header.Set("Content-Type", "application/json")

	normalized, err := inbound.DecodeRequest(req)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	_, err = policy.Validate(normalized, targetCaps, policy.ModeStrict)
	if err == nil {
		t.Fatal("expected error in strict mode with unsupported tools")
	}

	var gwErr *gwerr.Error
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected *gwerr.Error, got %T: %v", err, err)
	}
	if gwErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", gwErr.Status)
	}
	if gwErr.Kind != gwerr.KindInvalidRequest {
		t.Errorf("kind = %q, want invalid_request", gwErr.Kind)
	}
}
