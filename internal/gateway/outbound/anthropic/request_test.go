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

func TestBuildRequestTools(t *testing.T) {
	target := adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-3-5"}
	a := anthropic.New()

	t.Run("tools and tool choice mappings", func(t *testing.T) {
		disableParallel := true
		tests := []struct {
			name        string
			toolChoice  *ir.ToolChoice
			parallel    *bool
			wantType    string
			wantName    string
			wantDisable *bool
		}{
			{
				name:       "auto",
				toolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceAuto},
				wantType:   "auto",
			},
			{
				name:       "any",
				toolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceAny},
				wantType:   "any",
			},
			{
				name:       "tool specific",
				toolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "lookup"},
				wantType:   "tool",
				wantName:   "lookup",
			},
			{
				name:       "none",
				toolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceNone},
				wantType:   "none",
			},
			{
				name:        "auto with disable_parallel_tool_use",
				toolChoice:  &ir.ToolChoice{Mode: ir.ToolChoiceAuto, DisableParallelToolUse: &disableParallel},
				wantType:    "auto",
				wantDisable: &disableParallel,
			},
			{
				name:        "auto with ParallelToolCalls=false fallback",
				parallel:    boolPtr(false),
				wantType:    "auto",
				wantDisable: &disableParallel,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req := &ir.Request{
					Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"))},
					Tools: []ir.Tool{
						{Name: "lookup", Description: "Find info", Schema: json.RawMessage(`{"type":"object"}`)},
					},
					ToolChoice:        tc.toolChoice,
					ParallelToolCalls: tc.parallel,
				}
				httpReq, err := a.BuildRequest(t.Context(), req, target)
				if err != nil {
					t.Fatalf("BuildRequest: %v", err)
				}
				defer func() {
					if err := httpReq.Body.Close(); err != nil {
						t.Error(err)
					}
				}()

				var body struct {
					Tools []struct {
						Name        string          `json:"name"`
						Description string          `json:"description"`
						InputSchema json.RawMessage `json:"input_schema"`
					} `json:"tools"`
					ToolChoice *struct {
						Type                   string `json:"type"`
						Name                   string `json:"name"`
						DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
					} `json:"tool_choice"`
				}
				if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(body.Tools) != 1 || body.Tools[0].Name != "lookup" {
					t.Fatalf("tools = %+v", body.Tools)
				}
				if body.ToolChoice == nil {
					t.Fatalf("expected tool_choice, got nil")
				}
				if body.ToolChoice.Type != tc.wantType {
					t.Errorf("tool_choice.type = %q, want %q", body.ToolChoice.Type, tc.wantType)
				}
				if tc.wantName != "" && body.ToolChoice.Name != tc.wantName {
					t.Errorf("tool_choice.name = %q, want %q", body.ToolChoice.Name, tc.wantName)
				}
				if tc.wantDisable != nil {
					if body.ToolChoice.DisableParallelToolUse == nil || *body.ToolChoice.DisableParallelToolUse != *tc.wantDisable {
						t.Errorf("disable_parallel_tool_use = %v, want %v", body.ToolChoice.DisableParallelToolUse, *tc.wantDisable)
					}
				}
			})
		}
	})

	t.Run("single and parallel tool_use in assistant message", func(t *testing.T) {
		req := &ir.Request{
			Messages: []ir.Message{
				ir.NewMessage(ir.RoleUser, ir.NewTextBlock("call tools")),
				ir.NewMessage(ir.RoleAssistant,
					ir.NewToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
					ir.NewToolUseBlock("call_2", "get_time", json.RawMessage(`{"zone":"UTC"}`)),
				),
			},
		}
		httpReq, err := a.BuildRequest(t.Context(), req, target)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		defer func() {
			if err := httpReq.Body.Close(); err != nil {
				t.Error(err)
			}
		}()

		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2", len(body.Messages))
		}
		assistant := body.Messages[1]
		if assistant.Role != "assistant" || len(assistant.Content) != 2 {
			t.Fatalf("assistant message = %+v", assistant)
		}
		if assistant.Content[0].Type != "tool_use" || assistant.Content[0].ID != "call_1" || assistant.Content[0].Name != "get_weather" {
			t.Errorf("block 0 = %+v", assistant.Content[0])
		}
		if assistant.Content[1].Type != "tool_use" || assistant.Content[1].ID != "call_2" || assistant.Content[1].Name != "get_time" {
			t.Errorf("block 1 = %+v", assistant.Content[1])
		}
	})

	t.Run("tool results with string, image, and error merged into user turn", func(t *testing.T) {
		req := &ir.Request{
			Messages: []ir.Message{
				ir.NewMessage(ir.RoleUser, ir.NewTextBlock("start")),
				ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock("c1", "fn1", nil)),
				ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("c1", []ir.Block{ir.NewTextBlock("result text")}, false)),
				ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("c2", []ir.Block{ir.NewImageBlockBase64("image/png", "AA==")}, false)),
				ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("c3", []ir.Block{ir.NewTextBlock("boom")}, true)),
			},
		}
		httpReq, err := a.BuildRequest(t.Context(), req, target)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		defer func() {
			if err := httpReq.Body.Close(); err != nil {
				t.Error(err)
			}
		}()

		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string          `json:"type"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
					IsError   bool            `json:"is_error"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// 3 turns: user, assistant, user (merged 3 tool results)
		if len(body.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d: %+v", len(body.Messages), body.Messages)
		}
		userTurn := body.Messages[2]
		if userTurn.Role != "user" || len(userTurn.Content) != 3 {
			t.Fatalf("user turn = %+v", userTurn)
		}

		// Result 1: string content
		if userTurn.Content[0].Type != "tool_result" || userTurn.Content[0].ToolUseID != "c1" {
			t.Errorf("r1 = %+v", userTurn.Content[0])
		}
		var strContent string
		if err := json.Unmarshal(userTurn.Content[0].Content, &strContent); err != nil || strContent != "result text" {
			t.Errorf("r1 content = %s, want %q", userTurn.Content[0].Content, "result text")
		}

		// Result 2: image content
		if userTurn.Content[1].Type != "tool_result" || userTurn.Content[1].ToolUseID != "c2" {
			t.Errorf("r2 = %+v", userTurn.Content[1])
		}
		var imgBlocks []struct {
			Type   string `json:"type"`
			Source struct {
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"source"`
		}
		if err := json.Unmarshal(userTurn.Content[1].Content, &imgBlocks); err != nil || len(imgBlocks) != 1 || imgBlocks[0].Type != "image" || imgBlocks[0].Source.Data != "AA==" {
			t.Errorf("r2 content = %s", userTurn.Content[1].Content)
		}

		// Result 3: is_error = true
		if userTurn.Content[2].Type != "tool_result" || userTurn.Content[2].ToolUseID != "c3" || !userTurn.Content[2].IsError {
			t.Errorf("r3 = %+v", userTurn.Content[2])
		}
	})

	t.Run("tool result image fallback when images disabled", func(t *testing.T) {
		// Create adapter without image capability
		noImgAdapter := &anthropic.Adapter{} // Capabilities().Images is ImageCapabilityBoth, so we test URL fallback format
		req := &ir.Request{
			Messages: []ir.Message{
				ir.NewMessage(ir.RoleUser, ir.NewTextBlock("start")),
				ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("c1", []ir.Block{ir.NewImageBlockURL("https://example.com/pic.png")}, false)),
			},
		}
		httpReq, err := noImgAdapter.BuildRequest(t.Context(), req, target)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		defer func() {
			if err := httpReq.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		var body struct {
			Messages []struct {
				Content []struct {
					Content []struct {
						Type   string `json:"type"`
						Source struct {
							Type string `json:"type"`
							URL  string `json:"url"`
						} `json:"source"`
					} `json:"content"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Messages[0].Content[1].Content) != 1 || body.Messages[0].Content[1].Content[0].Source.URL != "https://example.com/pic.png" {
			t.Fatalf("unexpected content: %+v", body.Messages[0].Content[1])
		}
	})

	t.Run("invalid tool configurations rejected", func(t *testing.T) {
		// Tool without name
		_, err := a.BuildRequest(t.Context(), &ir.Request{
			Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"))},
			Tools:    []ir.Tool{{Name: ""}},
		}, target)
		if err == nil {
			t.Fatal("expected error for empty tool name")
		}

		// tool_use block in user message
		_, err = a.BuildRequest(t.Context(), &ir.Request{
			Messages: []ir.Message{
				ir.NewMessage(ir.RoleUser, ir.NewToolUseBlock("c1", "fn", nil)),
			},
		}, target)
		if err == nil {
			t.Fatal("expected error for tool_use in user message")
		}

		// tool_result block in assistant message
		_, err = a.BuildRequest(t.Context(), &ir.Request{
			Messages: []ir.Message{
				ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi")),
				ir.NewMessage(ir.RoleAssistant, ir.NewToolResultBlock("c1", nil, false)),
			},
		}, target)
		if err == nil {
			t.Fatal("expected error for tool_result in assistant message")
		}
	})
}

func boolPtr(b bool) *bool { return &b }
