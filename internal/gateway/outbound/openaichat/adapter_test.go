package openaichat_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
)

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestContract(t *testing.T) {
	adaptertest.RunOutboundContract(t, openaichat.New(), "testdata")
}

func TestConfiguredRequestUsesNormalizedRoutingAndContent(t *testing.T) {
	out := openaichat.New()
	out.InstructionRole = ir.InstructionDeveloper
	out.UseMaxCompletionTokens = true
	maxTokens := 64
	extensions := map[string]json.RawMessage{
		"model":          json.RawMessage(`"extension-model"`),
		"messages":       json.RawMessage(`[{"role":"user","content":"extension content"}]`),
		"stream":         json.RawMessage(`true`),
		"vendor_integer": json.RawMessage(`900719925474099312345678901`),
	}
	input := &ir.Request{
		Model:        "client-alias",
		Instructions: []ir.Instruction{ir.NewTextInstruction(ir.InstructionSystem, "Follow policy")},
		Messages:     []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("normalized content"))},
		Sampling:     ir.Sampling{MaxOutputTokens: &maxTokens},
		Extensions:   extensions,
	}
	headers := http.Header{"Authorization": {"Bearer fixture-only"}, "X-Fixture": {"one", "two"}}
	wantHeaders := headers.Clone()

	request, err := out.BuildRequest(t.Context(), input, adapter.Target{
		BaseURL: "https://upstream.invalid/v1/chat/completions",
		Model:   "resolved-model",
		Headers: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "resolved-model" {
		t.Fatalf("model = %q, %v", model, err)
	}
	var stream bool
	if err := json.Unmarshal(fields["stream"], &stream); err != nil || stream {
		t.Fatalf("stream = %v, %v", stream, err)
	}
	if _, found := fields["max_tokens"]; found {
		t.Fatal("max_tokens emitted with UseMaxCompletionTokens")
	}
	var gotMax int
	if err := json.Unmarshal(fields["max_completion_tokens"], &gotMax); err != nil || gotMax != maxTokens {
		t.Fatalf("max_completion_tokens = %d, %v", gotMax, err)
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(fields["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "developer" || string(messages[0].Content) != `"Follow policy"` || messages[1].Role != "user" || string(messages[1].Content) != `"normalized content"` {
		t.Fatalf("messages = %s", fields["messages"])
	}
	if got := string(fields["vendor_integer"]); got != "900719925474099312345678901" {
		t.Fatalf("vendor_integer = %s", got)
	}
	if request.URL.String() != "https://upstream.invalid/v1/chat/completions" {
		t.Fatalf("URL = %s", request.URL)
	}
	if request.Header.Get("Content-Type") != "application/json" || !reflect.DeepEqual(headers, wantHeaders) {
		t.Fatalf("headers mutated or missing content type: request=%v target=%v", request.Header, headers)
	}
	if string(extensions["model"]) != `"extension-model"` || string(extensions["messages"]) != `[{"role":"user","content":"extension content"}]` {
		t.Fatalf("extensions mutated: %v", extensions)
	}
}

func TestResponseAndStreamPreserveBodyOwnership(t *testing.T) {
	out := openaichat.New()
	responseBody := &trackedBody{Reader: strings.NewReader(`{"id":"chatcmpl_owned","object":"chat.completion","model":"vendor/model","choices":[{"index":0,"message":{"role":"assistant","content":"owned"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)}
	response := &http.Response{StatusCode: http.StatusOK, Body: responseBody, Header: make(http.Header)}
	got, err := out.ParseResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if responseBody.closed || got == nil || len(got.Content) != 1 || got.Content[0].Text != "owned" {
		t.Fatalf("response=%+v body.closed=%v", got, responseBody.closed)
	}
	if err := responseBody.Close(); err != nil {
		t.Fatal(err)
	}

	if !out.Capabilities().Streaming {
		t.Fatal("adapter does not advertise streaming")
	}

	streamBody := &trackedBody{Reader: strings.NewReader("data: [DONE]\n\n")}
	stream, err := out.ParseStream(t.Context(), &http.Response{StatusCode: http.StatusOK, Body: streamBody})
	if err != nil {
		t.Fatalf("ParseStream failed: %v", err)
	}
	if streamBody.closed {
		t.Fatal("ParseStream closed body before stream.Close()")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !streamBody.closed {
		t.Fatal("stream.Close() did not close stream body")
	}

	errorBody := &trackedBody{Reader: strings.NewReader(`{"error":{"message":"bad request","type":"invalid_request_error"}}`)}
	errStream, err := out.ParseStream(t.Context(), &http.Response{StatusCode: http.StatusBadRequest, Body: errorBody})
	if errStream != nil {
		t.Fatal("expected nil stream on HTTP 400")
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest {
		t.Fatalf("expected KindInvalidRequest, got %v", err)
	}
	if !errorBody.closed {
		t.Fatal("ParseStream failed to close error response body")
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestBuildRequestToolsMapping(t *testing.T) {
	out := openaichat.New()
	disabled := false
	req := &ir.Request{
		Model: "test-model",
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("call tools")),
			ir.NewMessage(ir.RoleAssistant,
				ir.NewToolUseBlock("call_1", "weather", json.RawMessage(`{"city":"Paris"}`)),
				ir.NewToolUseBlock("call_2", "clock", json.RawMessage(`"{invalid: json"`)),
			),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_1", []ir.Block{ir.NewTextBlock("Sunny")}, false)),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_2", []ir.Block{ir.NewTextBlock("Error: fail")}, true)),
		},
		Tools: []ir.Tool{
			{Name: "weather", Description: "Check weather", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "clock", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: &ir.ToolChoice{
			Mode:                   ir.ToolChoiceTool,
			Name:                   "weather",
			DisableParallelToolUse: &disabled,
		},
	}

	httpReq, err := out.BuildRequest(t.Context(), req, adapter.Target{
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-4o",
	})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	bodyBytes, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Model             string            `json:"model"`
		Tools             []json.RawMessage `json:"tools"`
		ToolChoice        json.RawMessage   `json:"tool_choice"`
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		Messages          []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}

	if len(parsed.Tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(parsed.Tools))
	}
	if parsed.ParallelToolCalls == nil || !*parsed.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want true", parsed.ParallelToolCalls)
	}
	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(parsed.ToolChoice, &tc); err != nil || tc.Type != "function" || tc.Function.Name != "weather" {
		t.Fatalf("tool_choice = %s", parsed.ToolChoice)
	}

	if len(parsed.Messages) != 4 {
		t.Fatalf("messages count = %d, want 4", len(parsed.Messages))
	}
	// Assistant message with parallel tool calls
	asst := parsed.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 2 {
		t.Fatalf("assistant tool_calls = %+v", asst)
	}
	if asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("asst call 0 mismatch: %+v", asst.ToolCalls[0])
	}
	if asst.ToolCalls[1].ID != "call_2" || asst.ToolCalls[1].Function.Arguments != "{invalid: json" {
		t.Fatalf("asst call 1 arguments = %q, want verbatim invalid JSON", asst.ToolCalls[1].Function.Arguments)
	}
	// Tool role messages
	tool1 := parsed.Messages[2]
	if tool1.Role != "tool" || tool1.ToolCallID != "call_1" || tool1.Content != "Sunny" {
		t.Fatalf("tool message 1 mismatch: %+v", tool1)
	}
	tool2 := parsed.Messages[3]
	if tool2.Role != "tool" || tool2.ToolCallID != "call_2" || tool2.Content != "Error: fail" {
		t.Fatalf("tool message 2 (is_error) mismatch: %+v", tool2)
	}
}

func TestBuildRequestToolChoiceModes(t *testing.T) {
	out := openaichat.New()
	for _, tc := range []struct {
		choice *ir.ToolChoice
		want   string
	}{
		{&ir.ToolChoice{Mode: ir.ToolChoiceAuto}, `"auto"`},
		{&ir.ToolChoice{Mode: ir.ToolChoiceNone}, `"none"`},
		{&ir.ToolChoice{Mode: ir.ToolChoiceAny}, `"required"`},
	} {
		req := &ir.Request{
			Model:      "test-model",
			Messages:   []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi"))},
			ToolChoice: tc.choice,
		}
		httpReq, err := out.BuildRequest(t.Context(), req, adapter.Target{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"})
		if err != nil {
			t.Fatalf("mode %s failed: %v", tc.choice.Mode, err)
		}
		bodyBytes, err := io.ReadAll(httpReq.Body)
		if closeErr := httpReq.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
			t.Fatal(err)
		}
		if string(parsed.ToolChoice) != tc.want {
			t.Fatalf("tool_choice for %s = %s, want %s", tc.choice.Mode, parsed.ToolChoice, tc.want)
		}
	}
}

func TestParseResponseTools(t *testing.T) {
	out := openaichat.New()
	raw := `{
		"id": "chatcmpl_tools",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "lookup",
							"arguments": "{\"key\":\"val\"}"
						}
					},
					{
						"id": "call_2",
						"type": "function",
						"function": {
							"name": "broken",
							"arguments": "{not-valid-json"
						}
					}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 20}
	}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(raw)),
	}
	irResp, err := out.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse failed: %v", err)
	}
	if irResp.StopReason != ir.StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", irResp.StopReason)
	}
	if len(irResp.Content) != 2 {
		t.Fatalf("content blocks count = %d, want 2", len(irResp.Content))
	}
	b1 := irResp.Content[0]
	if b1.Type != ir.BlockToolUse || b1.ToolUse.ID != "call_1" || b1.ToolUse.Name != "lookup" {
		t.Fatalf("block 1 mismatch: %+v", b1)
	}
	if string(b1.ToolUse.Input) != `{"key":"val"}` {
		t.Fatalf("block 1 input = %s, want {\"key\":\"val\"}", b1.ToolUse.Input)
	}
	b2 := irResp.Content[1]
	if b2.Type != ir.BlockToolUse || b2.ToolUse.ID != "call_2" || b2.ToolUse.Name != "broken" {
		t.Fatalf("block 2 mismatch: %+v", b2)
	}
	// Verify invalid JSON arguments preserved verbatim
	var decodedArg string
	if err := json.Unmarshal(b2.ToolUse.Input, &decodedArg); err != nil || decodedArg != "{not-valid-json" {
		t.Fatalf("block 2 input = %s, want verbatim {not-valid-json", b2.ToolUse.Input)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	out := openaichat.New()
	rawResp := `{
		"id": "chatcmpl_rt",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_rt_123",
					"type": "function",
					"function": {"name": "query_db", "arguments": "{\"query\":\"SELECT 1\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 10}
	}`
	parsedResp, err := out.ParseResponse(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(rawResp))})
	if err != nil {
		t.Fatalf("ParseResponse failed: %v", err)
	}

	followUpReq := &ir.Request{
		Model: "gpt-4o",
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("run query")),
			ir.NewMessage(ir.RoleAssistant, parsedResp.Content...),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_rt_123", []ir.Block{ir.NewTextBlock("1 row returned")}, false)),
		},
	}
	httpReq, err := out.BuildRequest(t.Context(), followUpReq, adapter.Target{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	bodyBytes, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(wire.Messages))
	}
	if wire.Messages[1].Role != "assistant" || len(wire.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant message roundtrip mismatch: %+v", wire.Messages[1])
	}
	if wire.Messages[1].ToolCalls[0].ID != "call_rt_123" || wire.Messages[1].ToolCalls[0].Function.Name != "query_db" {
		t.Fatalf("assistant tool call roundtrip mismatch: %+v", wire.Messages[1].ToolCalls[0])
	}
	if wire.Messages[2].Role != "tool" || wire.Messages[2].ToolCallID != "call_rt_123" || wire.Messages[2].Content != "1 row returned" {
		t.Fatalf("tool message roundtrip mismatch: %+v", wire.Messages[2])
	}
}

func TestBuildRequestParallelToolResultsInSingleMessageAndCoalesceAssistant(t *testing.T) {
	out := openaichat.New()
	req := &ir.Request{
		Model: "test-model",
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("run queries")),
			ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock("call_1", "get_a", []byte(`{"a":1}`))),
			ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock("call_2", "get_b", []byte(`{"b":2}`))),
			ir.NewMessage(ir.RoleTool,
				ir.NewToolResultBlock("call_1", []ir.Block{ir.NewTextBlock("res_1")}, false),
				ir.NewToolResultBlock("call_2", []ir.Block{ir.NewTextBlock("res_2")}, false),
			),
		},
	}
	httpReq, err := out.BuildRequest(t.Context(), req, adapter.Target{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	bodyBytes, err := io.ReadAll(httpReq.Body)
	if closeErr := httpReq.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	var wire struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatal(err)
	}

	if len(wire.Messages) != 4 {
		t.Fatalf("expected 4 messages (1 user, 1 coalesced assistant, 2 tool), got %d: %+v", len(wire.Messages), wire.Messages)
	}
	if wire.Messages[1].Role != "assistant" || len(wire.Messages[1].ToolCalls) != 2 {
		t.Fatalf("expected assistant with 2 coalesced tool calls, got: %+v", wire.Messages[1])
	}
	if wire.Messages[2].Role != "tool" || wire.Messages[2].ToolCallID != "call_1" || wire.Messages[2].Content != "res_1" {
		t.Fatalf("tool message 1 mismatch: %+v", wire.Messages[2])
	}
	if wire.Messages[3].Role != "tool" || wire.Messages[3].ToolCallID != "call_2" || wire.Messages[3].Content != "res_2" {
		t.Fatalf("tool message 2 mismatch: %+v", wire.Messages[3])
	}
}

func TestBuildRequestRejectsInvalidJSONToolArguments(t *testing.T) {
	out := openaichat.New()
	req := &ir.Request{
		Model: "test-model",
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hi")),
			ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock("call_1", "lookup", json.RawMessage(`{invalid: json`))),
		},
	}
	_, err := out.BuildRequest(t.Context(), req, adapter.Target{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected error for invalid JSON tool arguments, got nil")
	}
	if !strings.Contains(err.Error(), "tool arguments contain invalid JSON") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestBuildRequestCoalesceAssistantMultipartContent(t *testing.T) {
	out := openaichat.New()
	req := &ir.Request{
		Model: "test-model",
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("run")),
			// msg 1: multipart (2 text blocks)
			ir.NewMessage(ir.RoleAssistant, ir.NewTextBlock("part 1"), ir.NewTextBlock("part 2")),
			// msg 2: single string (1 text block)
			ir.NewMessage(ir.RoleAssistant, ir.NewTextBlock("part 3")),
			// msg 3: multipart (2 text blocks) + tool call
			ir.NewMessage(ir.RoleAssistant,
				ir.NewTextBlock("part 4"),
				ir.NewTextBlock("part 5"),
				ir.NewToolUseBlock("call_1", "lookup", []byte(`{"q":"test"}`)),
			),
		},
	}
	httpReq, err := out.BuildRequest(t.Context(), req, adapter.Target{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	bodyBytes, err := io.ReadAll(httpReq.Body)
	if closeErr := httpReq.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	var wire struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatalf("failed to unmarshal request body: %v\nbody: %s", err, string(bodyBytes))
	}

	if len(wire.Messages) != 2 {
		t.Fatalf("expected 2 messages (1 user, 1 coalesced assistant), got %d", len(wire.Messages))
	}
	asst := wire.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("expected assistant role, got %s", asst.Role)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Fatalf("expected tool call preserved, got %+v", asst.ToolCalls)
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(asst.Content, &parts); err != nil {
		t.Fatalf("failed to unmarshal assistant content parts: %v\ncontent: %s", err, string(asst.Content))
	}
	if len(parts) != 5 {
		t.Fatalf("expected 5 content parts, got %d: %+v", len(parts), parts)
	}
	for i, want := range []string{"part 1", "part 2", "part 3", "part 4", "part 5"} {
		if parts[i].Type != "text" || parts[i].Text != want {
			t.Fatalf("part %d mismatch: got %+v, want text %q", i, parts[i], want)
		}
	}
}
