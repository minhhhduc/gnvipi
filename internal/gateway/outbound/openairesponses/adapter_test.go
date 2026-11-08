package openairesponses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	inboundopenairesponses "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
)

var _ adapter.Outbound = openairesponses.New()

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func compareJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var values [2]any
	for i, body := range [][]byte{got, want} {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatalf("decode JSON %d: %v\n%s", i, err, body)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			t.Fatalf("JSON %d has trailing data: %v", i, err)
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func loadRequest(t *testing.T, name string) *ir.Request {
	t.Helper()
	var request ir.Request
	if err := json.Unmarshal(fixture(t, name), &request); err != nil {
		t.Fatal(err)
	}
	return &request
}

func TestAdapterContract(t *testing.T) {
	out := openairesponses.New()
	if out == nil || out.Protocol() != ir.ProtocolOpenAIResponses {
		t.Fatalf("New/Protocol = %#v/%q", out, out.Protocol())
	}
	want := adapter.Capabilities{Streaming: true, Tools: true, ParallelToolCalls: true, Images: adapter.ImageCapabilityBoth, Reasoning: true, StructuredOutput: true}
	if got := out.Capabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities = %+v, want %+v", got, want)
	}
}

func TestBuildRequestGoldens(t *testing.T) {
	for _, name := range []string{"legacy-common", "instructions", "mixed-instructions"} {
		t.Run(name, func(t *testing.T) {
			headers := http.Header{
				"Authorization": {"Bearer provider-credential"},
				"X-Trace":       {"first", "second"},
			}
			original := headers.Clone()
			target := adapter.Target{BaseURL: "https://provider.invalid/v1", Model: "resolved-model", Headers: headers}
			input := loadRequest(t, name+".request.ir.json")
			before, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request, err := openairesponses.New().BuildRequest(t.Context(), input, target)
			if err != nil {
				t.Fatal(err)
			}
			after, marshalErr := json.Marshal(input)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("BuildRequest mutated IR: before %s, after %s", before, after)
			}
			defer func() {
				if err := request.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if !reflect.DeepEqual(headers, original) {
				t.Fatalf("BuildRequest mutated Target.Headers: got %v, want %v", headers, original)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(struct {
				Method  string          `json:"method"`
				URL     string          `json:"url"`
				Headers http.Header     `json:"headers"`
				Body    json.RawMessage `json:"body"`
			}{request.Method, request.URL.String(), request.Header, body})
			if err != nil {
				t.Fatal(err)
			}
			compareJSON(t, got, fixture(t, name+".request.golden.json"))
		})
	}
}

func TestProviderEndpointSemantics(t *testing.T) {
	request := &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}}
	for base, want := range map[string]string{
		"https://provider.invalid":                   "https://provider.invalid/v1/responses",
		"https://provider.invalid/v1":                "https://provider.invalid/v1/responses",
		"https://provider.invalid/v1/responses":      "https://provider.invalid/v1/responses",
		"https://provider.invalid/prefix":            "https://provider.invalid/prefix/v1/responses",
		"https://provider.invalid/prefix/responses/": "https://provider.invalid/prefix/responses",
	} {
		t.Run(base, func(t *testing.T) {
			got, err := openairesponses.New().BuildRequest(t.Context(), request, adapter.Target{BaseURL: base, Model: "upstream"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := got.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if got.URL.String() != want {
				t.Fatalf("URL = %q, want %q", got.URL, want)
			}
		})
	}
}

func TestBuildRequestRejectsMalformedOrUnsupportedIR(t *testing.T) {
	validTarget := adapter.Target{BaseURL: "https://provider.invalid", Model: "upstream"}
	invalidImage := ir.NewImageBlockURL("")
	badSchema := ir.ResponseFormat{Type: ir.ResponseFormatJSONSchema, JSONSchema: &ir.JSONSchema{Schema: json.RawMessage(`{"type":"object"}`)}}
	for _, tc := range []struct {
		name    string
		request *ir.Request
	}{
		{"nil request", nil},
		{"unsupported role", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.Role("system"), ir.NewTextBlock("hello"))}}},
		{"invalid image", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, invalidImage)}}},
		{"invalid response format", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}, ResponseFormat: &badSchema}},
		{"tool without name", &ir.Request{Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}, Tools: []ir.Tool{{Name: ""}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := openairesponses.New().BuildRequest(t.Context(), tc.request, validTarget)
			if request != nil && request.Body != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err == nil {
				t.Fatal("BuildRequest accepted malformed or unsupported IR")
			}
		})
	}
}

func TestParseResponseGoldensAndBodyOwnership(t *testing.T) {
	for _, name := range []string{"completed", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			body := newTrackedBody(fixture(t, name+".response.json"))
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
			got, err := openairesponses.New().ParseResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			if body.closed || body.Len() != 0 {
				t.Fatalf("ParseResponse body ownership/read = closed %v, remaining %d", body.closed, body.Len())
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			compareJSON(t, encoded, fixture(t, name+".response.ir.json"))
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseResponseMapsUpstreamErrorAndLeavesBodyOpen(t *testing.T) {
	body := newTrackedBody(fixture(t, "upstream-error.response.json"))
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{
		"Retry-After":  {"30"},
		"X-Request-Id": {"req_responses_123"},
	}, Body: body}
	got, err := openairesponses.New().ParseResponse(response)
	if got != nil {
		t.Fatalf("ParseResponse returned response with error: %+v", got)
	}
	var gatewayError *gwerr.Error
	if !errors.As(err, &gatewayError) {
		t.Fatalf("ParseResponse error = %T %v, want *gwerr.Error", err, err)
	}
	view, err := json.Marshal(struct {
		Kind              gwerr.Kind `json:"kind"`
		Message           string     `json:"message"`
		Status            int        `json:"status,omitempty"`
		Code              string     `json:"code,omitempty"`
		UpstreamStatus    int        `json:"upstream_status,omitempty"`
		UpstreamRequestID string     `json:"upstream_request_id,omitempty"`
		RetryAfterMS      int64      `json:"retry_after_ms,omitempty"`
	}{gatewayError.Kind, gatewayError.Message, gatewayError.Status, gatewayError.Code, gatewayError.UpstreamStatus, gatewayError.UpstreamRequestID, gatewayError.RetryAfter.Milliseconds()})
	if err != nil {
		t.Fatal(err)
	}
	compareJSON(t, view, fixture(t, "upstream-error.golden.json"))
	if body.closed || body.Len() != 0 {
		t.Fatalf("error body ownership/read = closed %v, remaining %d", body.closed, body.Len())
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParseResponseRejectsMalformedWire(t *testing.T) {
	if got, err := openairesponses.New().ParseResponse(nil); err == nil || got != nil {
		t.Fatalf("nil response = %+v, %v", got, err)
	}
	for _, bodyText := range []string{
		`not json`,
		`{"id":"resp","model":"m","status":"completed","output":[]} {}`,
		`{"id":"resp","model":"m","status":"incomplete","output":[]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"future_item"}]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text"}]}]}`,
		`{"id":"resp","model":"m","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{"}]}`,
	} {
		t.Run(bodyText, func(t *testing.T) {
			body := newTrackedBody([]byte(bodyText))
			got, err := openairesponses.New().ParseResponse(&http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body})
			if err == nil || got != nil {
				t.Fatalf("malformed response = %+v, %v", got, err)
			}
			if body.closed {
				t.Fatal("ParseResponse closed caller-owned body on rejection")
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseStreamPreservesBodyOwnership(t *testing.T) {
	body := newTrackedBody([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
	stream, err := openairesponses.New().ParseStream(context.Background(), response)
	if err != nil {
		t.Fatalf("ParseStream failed: %v", err)
	}
	if body.closed {
		t.Fatal("ParseStream closed body before stream.Close()")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("stream.Close() did not close stream body")
	}

	errorBody := newTrackedBody([]byte(`{"error":{"message":"bad request"}}`))
	errResp := &http.Response{StatusCode: http.StatusBadRequest, Body: errorBody}
	errStream, err := openairesponses.New().ParseStream(context.Background(), errResp)
	if errStream != nil {
		t.Fatal("expected nil stream on 400")
	}
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !errorBody.closed {
		t.Fatal("ParseStream failed to close error body")
	}
}

type trackedBody struct {
	*bytes.Reader
	closed bool
}

func newTrackedBody(body []byte) *trackedBody {
	return &trackedBody{Reader: bytes.NewReader(body)}
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestBuildRequestToolsAndChoices(t *testing.T) {
	target := adapter.Target{BaseURL: "https://provider.invalid", Model: "gpt-4o"}
	trueVal := true
	falseVal := false
	req := &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
		Tools: []ir.Tool{
			{Name: "search", Description: "search web", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "calc"},
		},
		ToolChoice:        &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "search"},
		ParallelToolCalls: &falseVal,
	}

	httpReq, err := openairesponses.New().BuildRequest(t.Context(), req, target)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	bodyBytes, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var wire struct {
		Tools             []map[string]any `json:"tools"`
		ToolChoice        any              `json:"tool_choice"`
		ParallelToolCalls *bool            `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(wire.Tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(wire.Tools))
	}
	if wire.Tools[0]["name"] != "search" || wire.Tools[0]["type"] != "function" || wire.Tools[0]["description"] != "search web" {
		t.Errorf("tool 0 = %+v", wire.Tools[0])
	}
	if wire.Tools[1]["name"] != "calc" || wire.Tools[1]["type"] != "function" {
		t.Errorf("tool 1 = %+v", wire.Tools[1])
	}
	choiceMap, ok := wire.ToolChoice.(map[string]any)
	if !ok || choiceMap["type"] != "function" || choiceMap["name"] != "search" {
		t.Errorf("tool_choice = %+v", wire.ToolChoice)
	}
	if wire.ParallelToolCalls == nil || *wire.ParallelToolCalls {
		t.Errorf("parallel_tool_calls want false, got %+v", wire.ParallelToolCalls)
	}

	// Test ToolChoice Any -> "required"
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAny}
	req.ParallelToolCalls = &trueVal
	httpReq2, err := openairesponses.New().BuildRequest(t.Context(), req, target)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	defer func() {
		if err := httpReq2.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	bodyBytes2, err := io.ReadAll(httpReq2.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var wire2 struct {
		ToolChoice        string `json:"tool_choice"`
		ParallelToolCalls *bool  `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(bodyBytes2, &wire2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if wire2.ToolChoice != "required" {
		t.Errorf("tool_choice = %q, want required", wire2.ToolChoice)
	}
	if wire2.ParallelToolCalls == nil || !*wire2.ParallelToolCalls {
		t.Errorf("parallel_tool_calls want true, got %+v", wire2.ParallelToolCalls)
	}
}

func TestBuildRequestFunctionCallAndOutputItems(t *testing.T) {
	target := adapter.Target{BaseURL: "https://provider.invalid", Model: "gpt-4o"}
	req := &ir.Request{
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("get info")),
			ir.NewMessage(ir.RoleAssistant,
				ir.NewTextBlock("Calling tools"),
				ir.NewToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Tokyo"}`)),
				ir.NewToolUseBlock("call_2", "get_time", json.RawMessage(`{"tz":"JST"}`)),
			),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_1", []ir.Block{ir.NewTextBlock("Sunny")}, false)),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_2", []ir.Block{ir.NewTextBlock("15:00")}, false)),
		},
	}

	httpReq, err := openairesponses.New().BuildRequest(t.Context(), req, target)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	bodyBytes, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var wire struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Expected items in input:
	// 0: message (user: "get info")
	// 1: message (assistant: "Calling tools")
	// 2: function_call ("call_1", "get_weather")
	// 3: function_call ("call_2", "get_time")
	// 4: function_call_output ("call_1", "Sunny")
	// 5: function_call_output ("call_2", "15:00")
	if len(wire.Input) != 6 {
		t.Fatalf("input length = %d, want 6: %+v", len(wire.Input), wire.Input)
	}

	if wire.Input[0]["role"] != "user" {
		t.Errorf("input[0] = %+v", wire.Input[0])
	}
	if wire.Input[1]["role"] != "assistant" {
		t.Errorf("input[1] = %+v", wire.Input[1])
	}
	if wire.Input[2]["type"] != "function_call" || wire.Input[2]["call_id"] != "call_1" || wire.Input[2]["name"] != "get_weather" || wire.Input[2]["arguments"] != `{"city":"Tokyo"}` {
		t.Errorf("input[2] = %+v", wire.Input[2])
	}
	if wire.Input[3]["type"] != "function_call" || wire.Input[3]["call_id"] != "call_2" || wire.Input[3]["name"] != "get_time" || wire.Input[3]["arguments"] != `{"tz":"JST"}` {
		t.Errorf("input[3] = %+v", wire.Input[3])
	}
	if wire.Input[4]["type"] != "function_call_output" || wire.Input[4]["call_id"] != "call_1" || wire.Input[4]["output"] != "Sunny" {
		t.Errorf("input[4] = %+v", wire.Input[4])
	}
	if wire.Input[5]["type"] != "function_call_output" || wire.Input[5]["call_id"] != "call_2" || wire.Input[5]["output"] != "15:00" {
		t.Errorf("input[5] = %+v", wire.Input[5])
	}
}

func TestParseResponseParallelFunctionCalls(t *testing.T) {
	raw := `{
		"id": "resp_parallel_test",
		"object": "response",
		"model": "gpt-4o",
		"status": "completed",
		"output": [
			{
				"id": "fc_0",
				"type": "function_call",
				"call_id": "call_weather",
				"name": "get_weather",
				"arguments": "{\"city\":\"Paris\"}"
			},
			{
				"id": "fc_1",
				"type": "function_call",
				"call_id": "call_time",
				"name": "get_time",
				"arguments": "{\"tz\":\"CET\"}"
			}
		],
		"usage": {
			"input_tokens": 15,
			"output_tokens": 10
		}
	}`

	body := newTrackedBody([]byte(raw))
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	got, err := openairesponses.New().ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse error: %v", err)
	}

	if got.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %s, want %s", got.StopReason, ir.StopToolUse)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content length = %d, want 2", len(got.Content))
	}

	tu0 := got.Content[0].ToolUse
	if tu0 == nil || tu0.ID != "call_weather" || tu0.Name != "get_weather" || string(tu0.Input) != `{"city":"Paris"}` {
		t.Errorf("content[0] = %+v", tu0)
	}

	tu1 := got.Content[1].ToolUse
	if tu1 == nil || tu1.ID != "call_time" || tu1.Name != "get_time" || string(tu1.Input) != `{"tz":"CET"}` {
		t.Errorf("content[1] = %+v", tu1)
	}
}

func TestRoundTripFunctionCallOutput(t *testing.T) {
	origReq := &ir.Request{
		Model: "gpt-4o",
		Tools: []ir.Tool{
			{Name: "lookup", Description: "find data", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "lookup"},
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleUser, ir.NewTextBlock("query")),
			ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock("call_xyz", "lookup", json.RawMessage(`{"key":"abc"}`))),
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_xyz", []ir.Block{ir.NewTextBlock("result_123")}, false)),
		},
	}

	target := adapter.Target{BaseURL: "https://provider.invalid", Model: "gpt-4o"}
	httpReq, err := openairesponses.New().BuildRequest(t.Context(), origReq, target)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	inboundReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", httpReq.Body)
	decoded, err := inboundopenairesponses.New().DecodeRequest(inboundReq)
	if err != nil {
		t.Fatalf("DecodeRequest error: %v", err)
	}

	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "lookup" {
		t.Fatalf("round-trip tools: %+v", decoded.Tools)
	}
	if decoded.ToolChoice == nil || decoded.ToolChoice.Name != "lookup" {
		t.Fatalf("round-trip tool_choice: %+v", decoded.ToolChoice)
	}
	if len(decoded.Messages) != 3 {
		t.Fatalf("round-trip messages length = %d, want 3", len(decoded.Messages))
	}

	// Assistant tool call
	tu := decoded.Messages[1].Content[0].ToolUse
	if tu == nil || tu.ID != "call_xyz" || tu.Name != "lookup" || string(tu.Input) != `"{\"key\":\"abc\"}"` {
		t.Errorf("round-trip tool use: %+v", tu)
	}

	// Tool result
	tr := decoded.Messages[2].Content[0].ToolResult
	if tr == nil || tr.ToolUseID != "call_xyz" || len(tr.Content) != 1 || tr.Content[0].Text != "result_123" {
		t.Errorf("round-trip tool result: %+v", tr)
	}
}

func TestEncodeToolResultEmptyContent(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{
			ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock("call_empty", nil, false)),
		},
	}
	target := adapter.Target{BaseURL: "https://provider.invalid", Model: "gpt-4o"}
	httpReq, err := openairesponses.New().BuildRequest(t.Context(), req, target)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	defer func() {
		if err := httpReq.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	bodyBytes, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var wire struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(bodyBytes, &wire); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(wire.Input) != 1 {
		t.Fatalf("input length = %d, want 1", len(wire.Input))
	}
	if wire.Input[0]["type"] != "function_call_output" || wire.Input[0]["call_id"] != "call_empty" || wire.Input[0]["output"] != "" {
		t.Errorf("input[0] = %+v, want empty output string", wire.Input[0])
	}
}
