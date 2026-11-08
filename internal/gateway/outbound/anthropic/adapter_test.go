package anthropic_test

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
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestOutboundContract(t *testing.T) {
	a := anthropic.New()
	adaptertest.RunOutboundContract(t, a, "testdata")
	if a.Protocol() != ir.ProtocolAnthropicMessages {
		t.Fatalf("Protocol = %q, want %q", a.Protocol(), ir.ProtocolAnthropicMessages)
	}
	wantCapabilities := adapter.Capabilities{Streaming: true, Tools: true, ParallelToolCalls: true, Images: adapter.ImageCapabilityBoth, Reasoning: true, TopK: true}
	if got := a.Capabilities(); !reflect.DeepEqual(got, wantCapabilities) {
		t.Fatalf("Capabilities = %+v, want %+v", got, wantCapabilities)
	}
}

func TestDefaultMaxTokensIsConfigurable(t *testing.T) {
	a := anthropic.New()
	a.DefaultMaxTokens = 8192
	request, err := a.BuildRequest(t.Context(), &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
	}, adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-resolved"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want configured default 8192", body.MaxTokens)
	}
}

func TestBuildRequestAllowsStreaming(t *testing.T) {
	a := anthropic.New()
	request, err := a.BuildRequest(t.Context(), &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
		Stream:   true,
	}, adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-resolved"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Stream {
		t.Fatalf("stream = %v, want true", body.Stream)
	}
}

func TestBuildRequestRejectsUnrepresentableInput(t *testing.T) {
	tests := []struct {
		name    string
		request ir.Request
		message string
	}{
		{
			name: "assistant first",
			request: ir.Request{Messages: []ir.Message{
				ir.NewMessage(ir.RoleAssistant, ir.NewTextBlock("prefill")),
			}},
			message: "first message",
		},
		{
			name: "structured output unsupported by default",
			request: ir.Request{
				Messages:       []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				ResponseFormat: &ir.ResponseFormat{Type: ir.ResponseFormatJSONObject},
			},
			message: "structured output",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := anthropic.New().BuildRequest(t.Context(), &test.request, adapter.Target{
				BaseURL: "https://upstream.invalid", Model: "claude-resolved",
			})
			if request != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
				t.Fatal("BuildRequest returned a request alongside an error")
			}
			var gatewayError *gwerr.Error
			if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest ||
				gatewayError.Code != "unsupported_feature" || !strings.Contains(strings.ToLower(gatewayError.Message), test.message) {
				t.Fatalf("error = %#v, want unsupported %s error", err, test.message)
			}
		})
	}
}

func TestAnthropicToolEmulation(t *testing.T) {
	a := &anthropic.Adapter{ToolEmulation: true}
	if !a.Capabilities().StructuredOutput {
		t.Fatal("expected StructuredOutput capability to be true when ToolEmulation is enabled")
	}

	// 1. BuildRequest with JSON Schema
	schemaReq := &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("extract info"))},
		ResponseFormat: &ir.ResponseFormat{
			Type: ir.ResponseFormatJSONSchema,
			JSONSchema: &ir.JSONSchema{
				Name:   "person",
				Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
			},
		},
	}
	req, err := a.BuildRequest(t.Context(), schemaReq, adapter.Target{
		BaseURL: "https://upstream.invalid", Model: "claude-test",
	})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	defer func() {
		if err := req.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	var reqBody struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.NewDecoder(req.Body).Decode(&reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "person" {
		t.Fatalf("expected 1 tool named 'person', got %+v", reqBody.Tools)
	}
	if reqBody.ToolChoice.Type != "tool" || reqBody.ToolChoice.Name != "person" {
		t.Fatalf("expected tool_choice forcing 'person', got %+v", reqBody.ToolChoice)
	}

	// 2. ParseResponse: tool_use content block unwrapped to text, stop_reason changed to end_turn
	cannedResponse := `{
		"id": "msg_emulated",
		"type": "message",
		"role": "assistant",
		"model": "claude-test",
		"content": [
			{
				"type": "tool_use",
				"id": "toolu_123",
				"name": "person",
				"input": {"name": "Alice"}
			}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 15, "output_tokens": 8}
	}`
	httpResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(cannedResponse)),
	}
	parsed, err := a.ParseResponse(httpResp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if parsed.StopReason != ir.StopEndTurn {
		t.Fatalf("stop_reason = %q, want %q", parsed.StopReason, ir.StopEndTurn)
	}
	if len(parsed.Content) != 1 || parsed.Content[0].Type != ir.BlockText {
		t.Fatalf("expected 1 BlockText, got %+v", parsed.Content)
	}
	if !strings.Contains(parsed.Content[0].Text, `"Alice"`) {
		t.Fatalf("expected unwrapped JSON in text block, got %q", parsed.Content[0].Text)
	}
}

func TestAnthropicJSONModePrompt(t *testing.T) {
	a := &anthropic.Adapter{JSONModePrompt: true}
	if !a.Capabilities().StructuredOutput {
		t.Fatal("expected StructuredOutput capability to be true when JSONModePrompt is enabled")
	}

	jsonObjReq := &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("get data"))},
		ResponseFormat: &ir.ResponseFormat{
			Type: ir.ResponseFormatJSONObject,
		},
	}
	req, err := a.BuildRequest(t.Context(), jsonObjReq, adapter.Target{
		BaseURL: "https://upstream.invalid", Model: "claude-test",
	})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	defer func() {
		if err := req.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	var reqBody struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.NewDecoder(req.Body).Decode(&reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	found := false
	for _, sys := range reqBody.System {
		if strings.Contains(sys.Text, "valid JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected system prompt to contain JSON instruction, got %+v", reqBody.System)
	}

	// Test JSONModePrompt with ResponseFormatJSONSchema
	schemaReq := &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("get schema data"))},
		ResponseFormat: &ir.ResponseFormat{
			Type: ir.ResponseFormatJSONSchema,
			JSONSchema: &ir.JSONSchema{
				Name:   "item_schema",
				Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
			},
		},
	}
	sReq, err := a.BuildRequest(t.Context(), schemaReq, adapter.Target{
		BaseURL: "https://upstream.invalid", Model: "claude-test",
	})
	if err != nil {
		t.Fatalf("BuildRequest with JSONSchema: %v", err)
	}
	defer func() {
		if err := sReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	var sReqBody struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Tools []any `json:"tools"`
	}
	if err := json.NewDecoder(sReq.Body).Decode(&sReqBody); err != nil {
		t.Fatalf("decode schema request body: %v", err)
	}
	if len(sReqBody.Tools) > 0 {
		t.Errorf("expected no tools in JSONModePrompt mode, got %+v", sReqBody.Tools)
	}
	schemaFound := false
	for _, sys := range sReqBody.System {
		if strings.Contains(sys.Text, "matching the following schema") && strings.Contains(sys.Text, `"id"`) {
			schemaFound = true
			break
		}
	}
	if !schemaFound {
		t.Fatalf("expected system prompt to contain schema instruction, got %+v", sReqBody.System)
	}

	// Test default adapter without JSONModePrompt or ToolEmulation fails on JSONSchema
	defaultAdapter := anthropic.New()
	_, err = defaultAdapter.BuildRequest(t.Context(), schemaReq, adapter.Target{
		BaseURL: "https://upstream.invalid", Model: "claude-test",
	})
	if err == nil {
		t.Fatal("expected unsupported error on default adapter for JSONSchema, got nil")
	}
}

func TestAnthropicWithProvider(t *testing.T) {
	base := anthropic.New()
	if base.Capabilities().StructuredOutput {
		t.Fatal("default adapter should have StructuredOutput = false")
	}

	p := &provider.Provider{
		Extensions: map[string]json.RawMessage{
			"tool_emulation": json.RawMessage(`true`),
		},
	}
	configured := base.WithProvider(p)
	if !configured.Capabilities().StructuredOutput {
		t.Fatal("expected configured adapter to have StructuredOutput = true")
	}
}
