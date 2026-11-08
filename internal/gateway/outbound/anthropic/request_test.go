package anthropic_test

import (
	"encoding/json"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
)

func TestBuildRequestMaxTokensThinkingBudgetAdjustment(t *testing.T) {
	enabled := true
	disabled := false
	budget4096 := 4096
	budget1024 := 1024
	explicit2048 := 2048
	explicit10000 := 10000

	target := adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-test"}

	tests := []struct {
		name             string
		defaultMaxTokens int
		request          *ir.Request
		wantMaxTokens    int
		wantBudgetTokens int
		wantThinkingType string
	}{
		{
			name: "budget equals default max_tokens bumps max_tokens",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Reasoning: &ir.ReasoningConfig{
					Enabled:   &enabled,
					MaxTokens: &budget4096,
				},
			},
			wantMaxTokens:    8192, // 4096 + 4096
			wantBudgetTokens: 4096,
			wantThinkingType: "enabled",
		},
		{
			name: "high reasoning effort (16384) bumps default max_tokens",
			request: &ir.Request{
				Messages:        []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				ReasoningEffort: ir.ReasoningEffortHigh,
			},
			wantMaxTokens:    20480, // 16384 + 4096
			wantBudgetTokens: 16384,
			wantThinkingType: "enabled",
		},
		{
			name: "budget smaller than default max_tokens does not bump",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Reasoning: &ir.ReasoningConfig{
					Enabled:   &enabled,
					MaxTokens: &budget1024,
				},
			},
			wantMaxTokens:    4096,
			wantBudgetTokens: 1024,
			wantThinkingType: "enabled",
		},
		{
			name: "explicit max_tokens is preserved even if smaller than budget",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Sampling: ir.Sampling{
					MaxOutputTokens: &explicit2048,
				},
				Reasoning: &ir.ReasoningConfig{
					Enabled:   &enabled,
					MaxTokens: &budget4096,
				},
			},
			wantMaxTokens:    2048,
			wantBudgetTokens: 4096,
			wantThinkingType: "enabled",
		},
		{
			name: "explicit max_tokens is preserved when greater than budget",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Sampling: ir.Sampling{
					MaxOutputTokens: &explicit10000,
				},
				Reasoning: &ir.ReasoningConfig{
					Enabled:   &enabled,
					MaxTokens: &budget4096,
				},
			},
			wantMaxTokens:    10000,
			wantBudgetTokens: 4096,
			wantThinkingType: "enabled",
		},
		{
			name:             "custom default max_tokens bumps by configured default",
			defaultMaxTokens: 2000,
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Reasoning: &ir.ReasoningConfig{
					Enabled:   &enabled,
					MaxTokens: &budget4096,
				},
			},
			wantMaxTokens:    6096, // 4096 + 2000
			wantBudgetTokens: 4096,
			wantThinkingType: "enabled",
		},
		{
			name: "disabled thinking does not bump max_tokens",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Reasoning: &ir.ReasoningConfig{
					Enabled: &disabled,
				},
			},
			wantMaxTokens:    4096,
			wantThinkingType: "disabled",
		},
		{
			name: "adaptive thinking without budget does not bump max_tokens",
			request: &ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Reasoning: &ir.ReasoningConfig{
					Enabled: &enabled,
				},
			},
			wantMaxTokens:    4096,
			wantThinkingType: "adaptive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := anthropic.New()
			if tc.defaultMaxTokens > 0 {
				a.DefaultMaxTokens = tc.defaultMaxTokens
			}
			httpReq, err := a.BuildRequest(t.Context(), tc.request, target)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			defer func() {
				if err := httpReq.Body.Close(); err != nil {
					t.Error(err)
				}
			}()

			var body struct {
				MaxTokens int `json:"max_tokens"`
				Thinking  *struct {
					Type         string `json:"type"`
					BudgetTokens int    `json:"budget_tokens"`
				} `json:"thinking"`
			}
			if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			if body.MaxTokens != tc.wantMaxTokens {
				t.Errorf("max_tokens = %d, want %d", body.MaxTokens, tc.wantMaxTokens)
			}
			if tc.wantThinkingType != "" {
				if body.Thinking == nil {
					t.Fatalf("expected thinking object, got nil")
				}
				if body.Thinking.Type != tc.wantThinkingType {
					t.Errorf("thinking.type = %q, want %q", body.Thinking.Type, tc.wantThinkingType)
				}
				if tc.wantBudgetTokens > 0 && body.Thinking.BudgetTokens != tc.wantBudgetTokens {
					t.Errorf("thinking.budget_tokens = %d, want %d", body.Thinking.BudgetTokens, tc.wantBudgetTokens)
				}
			}
		})
	}
}
