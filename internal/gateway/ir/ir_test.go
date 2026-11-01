package ir

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseProtocol(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		for _, want := range []Protocol{
			ProtocolAnthropicMessages,
			ProtocolOpenAIChat,
			ProtocolOpenAIResponses,
		} {
			got, err := ParseProtocol(want.String())
			if err != nil {
				t.Fatalf("ParseProtocol(%q) error: %v", want, err)
			}
			if got != want {
				t.Errorf("ParseProtocol(%q) = %q, want %q", want, got, want)
			}
		}
	})
	t.Run("unknown is typed", func(t *testing.T) {
		_, err := ParseProtocol("graphql")
		var unknown *UnknownProtocolError
		if !errors.As(err, &unknown) {
			t.Fatalf("ParseProtocol(\"graphql\") error = %v, want *UnknownProtocolError", err)
		}
		if unknown.Protocol != "graphql" {
			t.Errorf("UnknownProtocolError.Protocol = %q, want %q", unknown.Protocol, "graphql")
		}
		if msg := err.Error(); msg == "" {
			t.Errorf("error message is empty")
		}
	})
	t.Run("empty is typed", func(t *testing.T) {
		_, err := ParseProtocol("")
		var unknown *UnknownProtocolError
		if !errors.As(err, &unknown) {
			t.Fatalf("ParseProtocol(\"\") error = %v, want *UnknownProtocolError", err)
		}
	})
}

func TestRequestRoundTrip(t *testing.T) {
	maxTok := 4096
	disableParallel := false
	req := Request{
		Model: "glm-5.2",
		Instructions: []Instruction{
			NewTextInstruction(InstructionSystem, "be brief"),
			NewTextInstruction(InstructionDeveloper, "be safe"),
		},
		Messages: []Message{
			NewMessage(RoleUser, NewTextBlock("hi"), NewImageBlockURL("https://example.com/a.png")),
			NewMessage(RoleAssistant, NewToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Berlin"}`))),
			NewMessage(RoleTool, NewToolResultBlock("call_1", []Block{NewTextBlock("18C, rain")}, false)),
		},
		Tools: []Tool{{
			Name:        "get_weather",
			Description: "weather lookup",
			Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAny, DisableParallelToolUse: &disableParallel},
		Sampling:   Sampling{Temperature: nil, TopP: nil, MaxOutputTokens: &maxTok, Stop: []string{"\n\n"}},
		ResponseFormat: &ResponseFormat{Type: ResponseFormatJSONSchema, JSONSchema: &JSONSchema{
			Name:   "out",
			Strict: true,
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Reasoning:          &ReasoningConfig{Enabled: &disableParallel, Effort: ReasoningEffortHigh},
		Stream:             true,
		StreamIncludeUsage: true,
		Metadata:           &Metadata{UserID: "u-42"},
		Extensions:         map[string]json.RawMessage{"cache_control": json.RawMessage(`{"cache_control":{"type":"ephemeral"}}`)},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Model != req.Model || len(got.Messages) != 3 || len(got.Instructions) != 2 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if len(got.Messages[1].Content) != 1 || got.Messages[1].Content[0].Type != BlockToolUse {
		t.Errorf("assistant tool_use message not preserved: %+v", got.Messages[1].Content)
	}
	tu := got.Messages[1].Content[0].ToolUse
	if tu.ID != "call_1" || tu.Name != "get_weather" || string(tu.Input) != `{"city":"Berlin"}` {
		t.Errorf("tool_use payload not verbatim: %+v", tu)
	}
	if len(got.Messages[2].Content) != 1 || got.Messages[2].Content[0].ToolResult.ToolUseID != "call_1" {
		t.Errorf("tool_result message not preserved: %+v", got.Messages[2].Content)
	}
	if got.Sampling.MaxOutputTokens == nil || *got.Sampling.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens not preserved: %v", got.Sampling.MaxOutputTokens)
	}
	if got.Sampling.Temperature != nil {
		t.Errorf("absent Temperature became %v on the wire", *got.Sampling.Temperature)
	}
	if string(got.Extensions["cache_control"]) != `{"cache_control":{"type":"ephemeral"}}` {
		t.Errorf("Extensions not preserved: %s", got.Extensions["cache_control"])
	}
	if got.ResponseFormat.Type != ResponseFormatJSONSchema || got.ResponseFormat.JSONSchema.Name != "out" {
		t.Errorf("ResponseFormat not preserved: %+v", got.ResponseFormat)
	}
	if got.ToolChoice.Mode != ToolChoiceAny || got.ToolChoice.DisableParallelToolUse == nil {
		t.Errorf("ToolChoice not preserved: %+v", got.ToolChoice)
	}
}

func TestAbsentFieldsStayAbsent(t *testing.T) {
	b, err := json.Marshal(Request{Model: "m", Messages: []Message{NewMessage(RoleUser, NewTextBlock("hi"))}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice", "response_format", "reasoning", "stream", "metadata", "extensions"} {
		if _, ok := raw[key]; ok {
			t.Errorf("absent field %q appeared in wire JSON: %v", key, raw[key])
		}
	}
}

func TestResponseRoundTrip(t *testing.T) {
	stop := "stop-42"
	res := Response{
		ID:    "resp_1",
		Model: "glm-5.2",
		Content: []Block{
			NewReasoningBlock("thinking...", "sig"),
			NewTextBlock("hello"),
			NewToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Berlin"}`)),
		},
		StopReason:   StopToolUse,
		StopSequence: &stop,
		Usage:        Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, ReasoningTokens: 5},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Response
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", got.StopReason)
	}
	if got.StopSequence == nil || *got.StopSequence != "stop-42" {
		t.Errorf("StopSequence not preserved: %v", got.StopSequence)
	}
	if len(got.Content) != 3 || got.Content[0].Type != BlockReasoning || got.Content[2].ToolUse.ID != "call_1" {
		t.Errorf("Content not preserved: %+v", got.Content)
	}
	if got.Usage.InputTokens != 10 || got.Usage.ReasoningTokens != 5 {
		t.Errorf("Usage not preserved: %+v", got.Usage)
	}
}

func TestNewTextResponse(t *testing.T) {
	res := NewTextResponse("resp_1", "glm-5.2", "hi", Usage{InputTokens: 1, OutputTokens: 2})
	if res.StopReason != StopEndTurn || len(res.Content) != 1 || res.Content[0].Text != "hi" {
		t.Errorf("NewTextResponse = %+v", res)
	}
}

func exampleStream() []StreamEvent {
	return []StreamEvent{
		NewMessageStart(RoleAssistant, "glm-5.2", Usage{InputTokens: 5}),
		NewBlockStart(0, NewTextBlock("")),
		NewTextDelta(0, "hel"),
		NewTextDelta(0, "lo"),
		NewBlockStop(0),
		NewBlockStart(1, NewToolUseBlock("call_1", "get_weather", nil)),
		NewToolArgumentsDelta(1, "call_1", `{"city":"Ber`),
		NewToolArgumentsDelta(1, "call_1", `lin"}`),
		NewBlockStop(1),
		NewUsageUpdate(Usage{InputTokens: 5, OutputTokens: 9}),
		NewMessageStop(StopToolUse),
	}
}

func TestValidateEventOrder(t *testing.T) {
	tests := []struct {
		name    string
		events  []StreamEvent
		wantErr bool
	}{
		{"canonical tool-call stream", exampleStream(), false},
		{"text-only minimal", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewMessageStop(StopEndTurn),
		}, false},
		{"no blocks, no usage", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewBlockStart(0, NewTextBlock("")),
			NewBlockStop(0),
			NewMessageStop(StopMaxTokens),
		}, false},
		{"error terminal", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewErrorEvent("rate_limit", "slow down"),
		}, false},
		{"usage before message_stop without blocks", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewUsageUpdate(Usage{}),
			NewMessageStop(StopEndTurn),
		}, false},
		{"empty sequence", nil, true},
		{"missing message_start", []StreamEvent{NewMessageStop(StopEndTurn)}, true},
		{"message_start not first", []StreamEvent{
			NewBlockStart(0, NewTextBlock("")),
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewMessageStop(StopEndTurn),
		}, true},
		{"duplicate message_start", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewMessageStop(StopEndTurn),
		}, true},
		{"delta before block_start", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewTextDelta(0, "x"),
			NewMessageStop(StopEndTurn),
		}, true},
		{"block_stop for unopened block", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewBlockStop(0),
			NewMessageStop(StopEndTurn),
		}, true},
		{"duplicate block_start", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewBlockStart(0, NewTextBlock("")),
			NewBlockStart(0, NewTextBlock("")),
			NewBlockStop(0),
			NewMessageStop(StopEndTurn),
		}, true},
		{"delta for closed block", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewBlockStart(0, NewTextBlock("")),
			NewBlockStop(0),
			NewTextDelta(0, "x"),
			NewMessageStop(StopEndTurn),
		}, true},
		{"duplicate usage_update", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewUsageUpdate(Usage{}),
			NewUsageUpdate(Usage{}),
			NewMessageStop(StopEndTurn),
		}, true},
		{"message_stop without reason", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			{Type: EventMessageStop},
		}, true},
		{"event after terminal", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewMessageStop(StopEndTurn),
			NewTextDelta(0, "x"),
		}, true},
		{"error then stop", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewErrorEvent("internal", "boom"),
			NewMessageStop(StopError),
		}, true},
		{"missing terminal", []StreamEvent{
			NewMessageStart(RoleAssistant, "m", Usage{}),
			NewBlockStart(0, NewTextBlock("")),
			NewBlockStop(0),
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEventOrder(tt.events)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateEventOrder(%v) = nil, want error", tt.events)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateEventOrder(%v) = %v, want nil", tt.events, err)
			}
		})
	}
}

func TestStreamEventJSON(t *testing.T) {
	events := exampleStream()
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []StreamEvent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ValidateEventOrder(got); err != nil {
		t.Errorf("round-tripped stream is not valid: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("round trip lost events: %d vs %d", len(got), len(events))
	}
	if got[1].Block.Type != BlockText || got[5].Block.Type != BlockToolUse {
		t.Errorf("block prototypes not preserved: %+v %+v", got[1], got[5])
	}
	if got[6].Delta.Type != DeltaToolArguments || got[6].Delta.PartialJSON != `{"city":"Ber` {
		t.Errorf("tool arguments delta not preserved: %+v", got[6].Delta)
	}
	if got[9].Usage == nil || got[9].Usage.OutputTokens != 9 {
		t.Errorf("usage_update not preserved: %+v", got[9])
	}
}
