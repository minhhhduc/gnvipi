package openaichat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/inbound/openaichat"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

var _ adapter.Inbound = (*openaichat.Adapter)(nil)

func readJSONFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "openaichat", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var values [2]any
	for i, data := range [][]byte{got, want} {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("JSON mismatch\ngot: %s\nwant: %s", got, want)
	}
}

// These goldens catch reordering instructions, dropping image/tool payloads,
// treating optional zero values as absent, and losing the legacy token budget.
func TestDecodeJSONGoldens(t *testing.T) {
	for _, name := range []string{"text", "system-developer", "multimodal", "legacy-max-tokens", "tools-extensions"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", bytes.NewReader(readJSONFixture(t, name+".request.json")))
			got, err := openaichat.New().DecodeRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, data, readJSONFixture(t, name+".ir.json"))
		})
	}
}

func TestCompletionTokenPrecedence(t *testing.T) {
	for _, tc := range []struct {
		fields string
		want   int
	}{
		{`"max_tokens":12,"max_completion_tokens":0`, 0},
		{`"max_tokens":0`, 0},
		{`"max_tokens":12,"max_completion_tokens":null`, 12},
		{`"max_tokens":12,"max_completion_tokens":7`, 7},
	} {
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}],`+tc.fields+`}`))
		got, err := openaichat.New().DecodeRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if got.Sampling.MaxOutputTokens == nil || *got.Sampling.MaxOutputTokens != tc.want {
			t.Fatalf("fields %s: budget %v, want %d", tc.fields, got.Sampling.MaxOutputTokens, tc.want)
		}
	}
}

func TestContentVariantExtrasRemainLossless(t *testing.T) {
	for _, tc := range []struct {
		name string
		part string
		want []ir.Block
	}{
		{"text", `{"type":"text","text":"hello","image_url":7}`, []ir.Block{ir.NewTextBlock("hello")}},
		{"image", `{"type":"image_url","image_url":{"url":"https://images.invalid/a.png","detail":"high"},"text":{"vendor":9007199254740993}}`, []ir.Block{ir.NewImageBlockURL("https://images.invalid/a.png")}},
		{"inline-image", `{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="},"text":{"vendor":true}}`, []ir.Block{ir.NewImageBlockBase64("image/png", "aGVsbG8=")}},
		{"unknown", `{"type":"future_content","text":{"vendor":true},"image_url":7}`, []ir.Block{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []byte(`[{"role":"user", "content":[` + tc.part + `]}]`)
			body := `{"model":"alias","messages":` + string(messages) + `}`
			request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
			got, err := openaichat.New().DecodeRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			want := []ir.Message{ir.NewMessage(ir.RoleUser, tc.want...)}
			if !reflect.DeepEqual(got.Messages, want) {
				t.Fatalf("messages = %+v, want %+v", got.Messages, want)
			}
			if !bytes.Equal(got.Extensions["messages"], messages) {
				t.Fatalf("preserved messages = %s, want %s", got.Extensions["messages"], messages)
			}
		})
	}
}

func TestDecodeRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `null`, `[]`, `{"model":"alias","messages":[]}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"alias","messages":[{"role":"unknown","content":"hi"}]}`,
		`{"model":"alias","messages":[{"role":"user","content":42}]}`,
		`{"model":"alias","messages":[{"role":"user","content":null}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":7,"image_url":{"url":"https://images.invalid/a.png"}}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"image_url","image_url":7,"text":"hello"}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":7}}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":{}}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`,
		`{"model":"alias","messages":[{"role":"tool","content":"result"}]}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"n":2}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"n":0}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"stop":["ok",1]}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"temperature":"hot"}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":-1}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":"yes"}}`,
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]} {}`,
	} {
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		got, err := openaichat.New().DecodeRequest(request)
		var gatewayError *gwerr.Error
		if got != nil || !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest || gwerr.StatusForKind(gatewayError.Kind) != 400 {
			t.Fatalf("body %s: request=%+v, error=%v", body, got, err)
		}
		if strings.Contains(body, `"n":2`) && !strings.Contains(gatewayError.Message, "n") {
			t.Fatalf("multiple choices rejection does not identify n: %v", err)
		}
	}
}

func TestStopShapesAndUnsetSampling(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  []string
	}{
		{``, nil},
		{`,"stop":null`, nil},
		{`,"stop":""`, []string{""}},
		{`,"stop":["END","STOP"]`, []string{"END", "STOP"}},
	} {
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}]`+tc.field+`}`))
		got, err := openaichat.New().DecodeRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Sampling.Stop, tc.want) || got.Sampling.Temperature != nil || got.Sampling.TopP != nil || got.Sampling.Seed != nil || got.Sampling.MaxOutputTokens != nil {
			t.Fatalf("stop %s: sampling=%+v", tc.field, got.Sampling)
		}
	}
}

func TestDecodePreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	got, err := openaichat.New().DecodeRequest(request)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: request=%+v, error=%v", got, err)
	}
}

func TestResponseFormatModes(t *testing.T) {
	for _, mode := range []ir.ResponseFormatType{ir.ResponseFormatText, ir.ResponseFormatJSONObject} {
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"n":1,"response_format":{"type":"`+string(mode)+`","vendor":false}}`))
		got, err := openaichat.New().DecodeRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if got.ResponseFormat == nil || got.ResponseFormat.Type != mode {
			t.Fatalf("format %s: got %+v", mode, got.ResponseFormat)
		}
		assertJSONEqual(t, got.Extensions["response_format"], []byte(`{"type":"`+string(mode)+`","vendor":false}`))
	}
}

func TestEncodeJSONGoldens(t *testing.T) {
	for _, name := range []string{"text", "tool-response", "reasoning-response"} {
		t.Run(name, func(t *testing.T) {
			var response ir.Response
			if err := json.Unmarshal(readJSONFixture(t, name+".response.ir.json"), &response); err != nil {
				t.Fatal(err)
			}
			codec := openaichat.New()
			codec.Created = 1735689600
			got, err := codec.EncodeResponse(&response)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, got, readJSONFixture(t, name+".response.golden.json"))
		})
	}
}

func TestFinishReasonsAndZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		stop ir.StopReason
		want string
	}{
		{ir.StopEndTurn, "stop"}, {ir.StopSequence, "stop"},
		{ir.StopMaxTokens, "length"}, {ir.StopToolUse, "tool_calls"},
		{ir.StopContentFilter, "content_filter"},
	} {
		codec := openaichat.New()
		codec.Created = 1735689600
		got, err := codec.EncodeResponse(&ir.Response{ID: "resp", Model: "client/alias", StopReason: tc.stop})
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, got, []byte(`{"id":"resp","object":"chat.completion","created":1735689600,"model":"client/alias","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"`+tc.want+`"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
	}
}

func TestEncodeRejectsNonCompletionAndInvalidPayload(t *testing.T) {
	codec := openaichat.New()
	for _, response := range []*ir.Response{
		nil,
		{ID: "resp", StopReason: ir.StopError},
		{ID: "resp", StopReason: ir.StopReason("unknown")},
		{ID: "resp", StopReason: ir.StopEndTurn, Content: []ir.Block{{Type: ir.BlockToolUse}}},
		{ID: "resp", StopReason: ir.StopEndTurn, Content: []ir.Block{ir.NewToolUseBlock("call", "f", json.RawMessage(`{"bad":`))}},
	} {
		if got, err := codec.EncodeResponse(response); err == nil || got != nil {
			t.Fatalf("response=%+v: encoded=%s, error=%v", response, got, err)
		}
	}
}

func TestCompletionCreatedReflectsEncodingTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		codec := openaichat.New()
		response := ir.NewTextResponse("resp", "client/alias", "visible", ir.Usage{})
		for range 2 {
			// Advance the fake clock, including before the first completion, so
			// neither construction time nor an earlier completion can leak in.
			time.Sleep(2 * time.Second)
			data, err := codec.EncodeResponse(&response)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Created int64 `json:"created"`
			}
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if want := time.Now().Unix(); wire.Created != want {
				t.Fatalf("completion created = %d, want encoding time %d", wire.Created, want)
			}
		}
	})
}

func TestDisabledReasoningKeepsVisibleContent(t *testing.T) {
	codec := openaichat.New()
	codec.DisableReasoning = true
	response := ir.NewTextResponse("resp", "client/alias", "visible", ir.Usage{})
	response.Content = append(response.Content, ir.NewReasoningBlock("private", ""))
	data, err := codec.EncodeResponse(&response)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Choices) != 1 {
		t.Fatalf("completion choices = %d, want one", len(wire.Choices))
	}
	assertJSONEqual(t, wire.Choices[0].Message, []byte(`{"role":"assistant","content":"visible"}`))
}

func TestDecodeLegacyFunctionsAndFunctionCall(t *testing.T) {
	t.Run("functions and function_call auto", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"weather","description":"get weather","parameters":{"type":"object"}}],"function_call":"auto"}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		got, err := openaichat.New().DecodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Tools) != 1 || got.Tools[0].Name != "weather" || got.Tools[0].Description != "get weather" {
			t.Fatalf("tools mismatch: %+v", got.Tools)
		}
		if got.ToolChoice == nil || got.ToolChoice.Mode != ir.ToolChoiceAuto {
			t.Fatalf("tool_choice mismatch: %+v", got.ToolChoice)
		}
	})

	t.Run("function_call none", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"weather"}],"function_call":"none"}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		got, err := openaichat.New().DecodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolChoice == nil || got.ToolChoice.Mode != ir.ToolChoiceNone {
			t.Fatalf("tool_choice mismatch: %+v", got.ToolChoice)
		}
	})

	t.Run("function_call named", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"weather"}],"function_call":{"name":"weather"}}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		got, err := openaichat.New().DecodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolChoice == nil || got.ToolChoice.Mode != ir.ToolChoiceTool || got.ToolChoice.Name != "weather" {
			t.Fatalf("tool_choice mismatch: %+v", got.ToolChoice)
		}
	})

	t.Run("invalid function_call string", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"function_call":"invalid"}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		_, err := openaichat.New().DecodeRequest(req)
		if err == nil {
			t.Fatal("expected error for invalid function_call string")
		}
	})

	t.Run("invalid function_call object without name", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"function_call":{"not_name":"weather"}}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		_, err := openaichat.New().DecodeRequest(req)
		if err == nil {
			t.Fatal("expected error for function_call without name")
		}
	})

	t.Run("both tool_choice and function_call present", func(t *testing.T) {
		body := `{
			"model": "gpt-4",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [{"type": "function", "function": {"name": "weather"}}],
			"tool_choice": "auto",
			"functions": [{"name": "stale_weather"}],
			"function_call": "none"
		}`
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/chat/completions", strings.NewReader(body))
		got, err := openaichat.New().DecodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolChoice == nil || got.ToolChoice.Mode != ir.ToolChoiceAuto {
			t.Fatalf("tool_choice mismatch: %+v, want auto", got.ToolChoice)
		}
		if _, ok := got.Extensions["function_call"]; ok {
			t.Fatalf("expected function_call to be stripped from extensions, got: %s", got.Extensions["function_call"])
		}
		if _, ok := got.Extensions["functions"]; ok {
			t.Fatalf("expected functions to be stripped from extensions, got: %s", got.Extensions["functions"])
		}
		if len(got.Tools) != 1 || got.Tools[0].Name != "weather" {
			t.Fatalf("tools mismatch: %+v", got.Tools)
		}
	})
}

func TestEncodeToolResponsePreservesInvalidJSONArguments(t *testing.T) {
	codec := openaichat.New()
	codec.Created = 1700000000
	resp := &ir.Response{
		ID:         "resp_invalid_json",
		Model:      "test-model",
		StopReason: ir.StopToolUse,
		Content: []ir.Block{
			ir.NewToolUseBlock("call_1", "broken_func", json.RawMessage(`"{bad: json"`)),
			ir.NewToolUseBlock("call_2", "valid_func", json.RawMessage(`{"good":"json"}`)),
		},
	}
	data, err := codec.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse failed: %v", err)
	}
	var wire struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("failed to unmarshal encoded response: %v", err)
	}
	if len(wire.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(wire.Choices))
	}
	if wire.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", wire.Choices[0].FinishReason)
	}
	calls := wire.Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool_calls, got %d", len(calls))
	}
	if calls[0].Function.Arguments != "{bad: json" {
		t.Fatalf("call 0 arguments = %q, want {bad: json", calls[0].Function.Arguments)
	}
	if calls[1].Function.Arguments != `{"good":"json"}` {
		t.Fatalf("call 1 arguments = %q, want {\"good\":\"json\"}", calls[1].Function.Arguments)
	}
}
