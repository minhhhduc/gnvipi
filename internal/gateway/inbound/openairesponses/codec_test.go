package openairesponses_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

var _ adapter.Inbound = (*openairesponses.Adapter)(nil)

func codecFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "openairesponses", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compareCodecJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var values [2]any
	for i, data := range [][]byte{got, want} {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatal(err)
		}
		// Function arguments are JSON encoded inside a wire string. Clients
		// parse them; indentation in an IR fixture is not observable semantics.
		if document, ok := values[i].(map[string]any); ok && document["object"] == "response" {
			items, _ := document["output"].([]any)
			for _, value := range items {
				item, ok := value.(map[string]any)
				if !ok || item["type"] != "function_call" {
					continue
				}
				arguments, ok := item["arguments"].(string)
				if !ok {
					t.Fatal("function_call.arguments must be a string")
				}
				argumentDecoder := json.NewDecoder(strings.NewReader(arguments))
				argumentDecoder.UseNumber()
				var parsed any
				if err := argumentDecoder.Decode(&parsed); err != nil {
					t.Fatal(err)
				}
				item["arguments"] = parsed
			}
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("JSON mismatch\ngot: %s\nwant: %s", got, want)
	}
}

// These fixtures catch dropped instructions, reordered conversation items,
// image corruption, lost optional zeros and precision loss in opaque fields.
func TestDecodeResponsesJSONGoldens(t *testing.T) {
	for _, name := range []string{"text", "instructions-images", "tool-reasoning"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", bytes.NewReader(codecFixture(t, name+".request.json")))
			decoded, err := openairesponses.New().DecodeRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			compareCodecJSON(t, data, codecFixture(t, name+".ir.json"))
		})
	}
}

func TestEncodeResponsesJSONGoldens(t *testing.T) {
	for _, name := range []string{"completed", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			var response ir.Response
			if err := json.Unmarshal(codecFixture(t, name+".response.ir.json"), &response); err != nil {
				t.Fatal(err)
			}
			codec := openairesponses.New()
			codec.Created = 1710000000
			encoded, err := codec.EncodeResponse(&response)
			if err != nil {
				t.Fatal(err)
			}
			compareCodecJSON(t, encoded, codecFixture(t, name+".response.golden.json"))
		})
	}
}

func TestResponsesStatefulTranslationIsRejected(t *testing.T) {
	for _, field := range []string{`"previous_response_id":"resp_previous"`, `"store":true`, `"conversation":{"id":"conv_1"}`} {
		t.Run(field, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":"hello",`+field+`}`))
			_, err := openairesponses.New().DecodeRequest(request)
			var gatewayError *gwerr.Error
			if !errors.As(err, &gatewayError) || gatewayError.Code != "unsupported_feature" {
				t.Fatalf("stateful translation error = %v", err)
			}
			out := httptest.NewRecorder()
			if err := gwerr.Write(out, gwerr.ProtocolOpenAIResponses, gatewayError); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if out.Code != http.StatusBadRequest || envelope.Error.Code != "unsupported_feature" || envelope.Error.Type == "" {
				t.Fatalf("Responses error = %d %s", out.Code, out.Body.String())
			}
		})
	}
}

func TestResponsesStatelessMarkersAndOpaqueFieldsSurvive(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello","image_url":17},{"type":"future_part","payload":9007199254740993}]}],"store":false,"previous_response_id":null,"conversation":null,"temperature":null}`))
	decoded, err := openairesponses.New().DecodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Messages, []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}) {
		t.Fatalf("decoded content = %+v", decoded.Messages)
	}
	for field, value := range map[string]string{"store": "false", "previous_response_id": "null", "conversation": "null", "temperature": "null"} {
		if string(decoded.Extensions[field]) != value {
			t.Fatalf("opaque %s = %s, want %s", field, decoded.Extensions[field], value)
		}
	}
	if !bytes.Contains(decoded.Extensions["input"], []byte("9007199254740993")) {
		t.Fatal("opaque future content lost its exact numeric value")
	}
}

func TestResponsesMalformedRequestBoundaries(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","input":"hello"} {"model":"other"}`,
		`{"model":"alias","input":17}`,
		`{"model":"alias","input":[{"role":"unknown","content":"hello"}]}`,
		`{"model":"alias","input":"hello","max_output_tokens":-1}`,
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, err := openairesponses.New().DecodeRequest(request)
		var gatewayError *gwerr.Error
		if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest {
			t.Fatalf("invalid request %s: %v", body, err)
		}
	}
}

func TestResponsesRequiredItemFields(t *testing.T) {
	for _, tc := range []struct{ name, item string }{
		{"message-content-missing", `{"role":"user"}`},
		{"message-content-null", `{"role":"user","content":null}`},
		{"part-type-missing", `{"role":"user","content":[{"text":"hello"}]}`},
		{"part-type-null", `{"role":"user","content":[{"type":null,"text":"hello"}]}`},
		{"call-arguments-missing", `{"type":"function_call","call_id":"call_a","name":"lookup"}`},
		{"call-arguments-null", `{"type":"function_call","call_id":"call_a","name":"lookup","arguments":null}`},
		{"call-output-missing", `{"type":"function_call_output","call_id":"call_a"}`},
		{"call-output-null", `{"type":"function_call_output","call_id":"call_a","output":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":[`+tc.item+`]}`))
			_, err := openairesponses.New().DecodeRequest(request)
			var invalid *gwerr.Error
			if !errors.As(err, &invalid) || invalid.Kind != gwerr.KindInvalidRequest || invalid.Status != http.StatusBadRequest {
				t.Fatalf("malformed Responses item did not return 400: %s; error=%v", tc.item, err)
			}
		})
	}
}

func TestResponsesEncodingDoesNotSilentlyDropInvalidBlocks(t *testing.T) {
	for _, block := range []ir.Block{ir.NewImageBlockURL("https://images.invalid/a.png"), ir.NewToolUseBlock("", "lookup", json.RawMessage(`{}`))} {
		response := ir.NewTextResponse("resp_invalid", "model", "", ir.Usage{})
		response.Content = []ir.Block{block}
		if _, err := openairesponses.New().EncodeResponse(&response); err == nil {
			t.Fatalf("unrepresentable response block was silently accepted: %+v", block)
		}
	}
}

func TestDecodeResponsesToolsAndToolChoice(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "test",
		"tools": [
			{
				"type": "function",
				"name": "lookup",
				"description": "Lookup value",
				"parameters": {"type": "object", "properties": {"q": {"type": "string"}}},
				"strict": true
			}
		],
		"tool_choice": {"type": "function", "name": "lookup"},
		"parallel_tool_calls": false
	}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
	decoded, err := openairesponses.New().DecodeRequest(req)
	if err != nil {
		t.Fatalf("DecodeRequest error: %v", err)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "lookup" || decoded.Tools[0].Description != "Lookup value" {
		t.Fatalf("tools mismatch: %+v", decoded.Tools)
	}
	if decoded.ToolChoice == nil || decoded.ToolChoice.Mode != ir.ToolChoiceTool || decoded.ToolChoice.Name != "lookup" {
		t.Fatalf("tool_choice mismatch: %+v", decoded.ToolChoice)
	}
	if decoded.ToolChoice.DisableParallelToolUse == nil || !*decoded.ToolChoice.DisableParallelToolUse {
		t.Fatalf("DisableParallelToolUse want true, got %+v", decoded.ToolChoice.DisableParallelToolUse)
	}
	if decoded.ParallelToolCalls == nil || *decoded.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls want false, got %+v", decoded.ParallelToolCalls)
	}
}

func TestEncodeResponseSingleAndParallelToolCalls(t *testing.T) {
	resp := &ir.Response{
		ID:    "resp_tool_test",
		Model: "gpt-4o",
		Content: []ir.Block{
			ir.NewToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Tokyo"}`)),
			ir.NewToolUseBlock("call_2", "get_time", json.RawMessage(`{"tz":"JST"}`)),
		},
		StopReason: ir.StopToolUse,
		Usage: ir.Usage{
			InputTokens:  10,
			OutputTokens: 20,
		},
	}

	adp := openairesponses.New()
	adp.Created = 1710000000
	encoded, err := adp.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse error: %v", err)
	}

	var parsed struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed.Status != "completed" {
		t.Errorf("status = %q, want completed", parsed.Status)
	}
	if len(parsed.Output) != 2 {
		t.Fatalf("output length = %d, want 2", len(parsed.Output))
	}

	// First tool call
	if parsed.Output[0].Type != "function_call" || parsed.Output[0].CallID != "call_1" || parsed.Output[0].Name != "get_weather" {
		t.Errorf("output[0] = %+v", parsed.Output[0])
	}
	if parsed.Output[0].Arguments != `{"city":"Tokyo"}` {
		t.Errorf("output[0].Arguments = %s", parsed.Output[0].Arguments)
	}

	// Second tool call (parallel)
	if parsed.Output[1].Type != "function_call" || parsed.Output[1].CallID != "call_2" || parsed.Output[1].Name != "get_time" {
		t.Errorf("output[1] = %+v", parsed.Output[1])
	}
	if parsed.Output[1].Arguments != `{"tz":"JST"}` {
		t.Errorf("output[1].Arguments = %s", parsed.Output[1].Arguments)
	}
}

func TestDecodeResponsesParallelFunctionCallsAndOutputs(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": [
			{
				"type": "function_call",
				"id": "fc_0",
				"call_id": "call_1",
				"name": "get_weather",
				"arguments": "{\"city\":\"Tokyo\"}"
			},
			{
				"type": "function_call",
				"id": "fc_1",
				"call_id": "call_2",
				"name": "get_time",
				"arguments": "{\"tz\":\"JST\"}"
			},
			{
				"type": "function_call_output",
				"call_id": "call_1",
				"output": "Sunny 22C"
			},
			{
				"type": "function_call_output",
				"call_id": "call_2",
				"output": "14:00"
			}
		]
	}`

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
	decoded, err := openairesponses.New().DecodeRequest(req)
	if err != nil {
		t.Fatalf("DecodeRequest error: %v", err)
	}

	if len(decoded.Messages) != 4 {
		t.Fatalf("messages count = %d, want 4", len(decoded.Messages))
	}

	// Message 0: call_1
	if decoded.Messages[0].Role != ir.RoleAssistant || len(decoded.Messages[0].Content) != 1 {
		t.Fatalf("message[0] invalid: %+v", decoded.Messages[0])
	}
	tu0 := decoded.Messages[0].Content[0].ToolUse
	if tu0 == nil || tu0.ID != "call_1" || tu0.Name != "get_weather" {
		t.Errorf("tu0 = %+v", tu0)
	}

	// Message 1: call_2
	tu1 := decoded.Messages[1].Content[0].ToolUse
	if tu1 == nil || tu1.ID != "call_2" || tu1.Name != "get_time" {
		t.Errorf("tu1 = %+v", tu1)
	}

	// Message 2: call_1 output
	if decoded.Messages[2].Role != ir.RoleTool {
		t.Errorf("message[2].Role = %s, want %s", decoded.Messages[2].Role, ir.RoleTool)
	}
	tr0 := decoded.Messages[2].Content[0].ToolResult
	if tr0 == nil || tr0.ToolUseID != "call_1" || len(tr0.Content) != 1 || tr0.Content[0].Text != "Sunny 22C" {
		t.Errorf("tr0 = %+v", tr0)
	}

	// Message 3: call_2 output
	tr1 := decoded.Messages[3].Content[0].ToolResult
	if tr1 == nil || tr1.ToolUseID != "call_2" || len(tr1.Content) != 1 || tr1.Content[0].Text != "14:00" {
		t.Errorf("tr1 = %+v", tr1)
	}
}
