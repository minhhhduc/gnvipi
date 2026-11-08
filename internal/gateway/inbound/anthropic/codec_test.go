package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestInboundContract(t *testing.T) {
	adaptertest.RunInboundContract(t, New(), filepath.Join("testdata", "anthropic"))
}

func TestInvalidRequestsProduceAnthropicErrors(t *testing.T) {
	valid := `{"model":"client-alias","max_tokens":2048,"messages":[{"role":"user","content":"Hello"}]}`
	cases := map[string]string{
		"empty body":            "",
		"malformed JSON":        `{"model":`,
		"trailing JSON":         valid + `{}`,
		"nonobject":             `[]`,
		"null body":             `null`,
		"missing model":         `{"max_tokens":2048,"messages":[{"role":"user","content":"Hello"}]}`,
		"blank model":           strings.Replace(valid, `"client-alias"`, `" "`, 1),
		"missing max tokens":    strings.Replace(valid, `"max_tokens":2048,`, "", 1),
		"null max tokens":       strings.Replace(valid, `2048`, `null`, 1),
		"negative max tokens":   strings.Replace(valid, `2048`, `-1`, 1),
		"fractional max tokens": strings.Replace(valid, `2048`, `1.5`, 1),
		"empty messages":        strings.Replace(valid, `[{"role":"user","content":"Hello"}]`, `[]`, 1),
		"invalid role":          strings.Replace(valid, `"user"`, `"system"`, 1),
		"null message":          strings.Replace(valid, `{"role":"user","content":"Hello"}`, `null`, 1),
		"null content":          strings.Replace(valid, `"Hello"`, `null`, 1),
		"object content":        strings.Replace(valid, `"Hello"`, `{}`, 1),
		"missing text":          strings.Replace(valid, `"Hello"`, `[{"type":"text"}]`, 1),
		"null text":             strings.Replace(valid, `"Hello"`, `[{"type":"text","text":null}]`, 1),
		"invalid block":         strings.Replace(valid, `"Hello"`, `[1]`, 1),
		"missing block type":    strings.Replace(valid, `"Hello"`, `[{"text":"Hello"}]`, 1),
		"invalid image":         strings.Replace(valid, `"Hello"`, `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not base64"}}]`, 1),
		"image without source":  strings.Replace(valid, `"Hello"`, `[{"type":"image"}]`, 1),
	}
	for name, extra := range map[string]string{
		"null temperature":           `"temperature":null`,
		"high temperature":           `"temperature":1.1`,
		"negative top p":             `"top_p":-0.1`,
		"negative top k":             `"top_k":-1`,
		"fractional top k":           `"top_k":1.5`,
		"null stream":                `"stream":null`,
		"invalid stop sequences":     `"stop_sequences":[1]`,
		"null stop sequences":        `"stop_sequences":null`,
		"null stop entry":            `"stop_sequences":[null]`,
		"mixed null stop entries":    `"stop_sequences":["END",null]`,
		"mixed numeric stop entries": `"stop_sequences":["END",1]`,
		"invalid metadata":           `"metadata":[]`,
		"invalid user id":            `"metadata":{"user_id":123}`,
		"object user id":             `"metadata":{"user_id":{}}`,
		"array user id":              `"metadata":{"user_id":[]}`,
		"boolean user id":            `"metadata":{"user_id":false}`,
		"invalid system":             `"system":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]`,
		"invalid thinking type":      `"thinking":{"type":"invalid"}`,
		"missing thinking budget":    `"thinking":{"type":"enabled"}`,
		"null thinking budget":       `"thinking":{"type":"enabled","budget_tokens":null}`,
		"small thinking budget":      `"thinking":{"type":"enabled","budget_tokens":1023}`,
		"fractional thinking budget": `"thinking":{"type":"enabled","budget_tokens":1024.5}`,
		"invalid tools":              `"tools":{}`,
		"invalid tool choice":        `"tool_choice":[]`,
	} {
		cases[name] = valid[:len(valid)-1] + "," + extra + "}"
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := New().DecodeRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body)))
			var normalized *gwerr.Error
			if got != nil || !errors.As(err, &normalized) || normalized.Kind != gwerr.KindInvalidRequest {
				t.Fatalf("request=%+v error=%v; want normalized invalid request", got, err)
			}
			encoded, status, err := gwerr.Encode(gwerr.ProtocolAnthropicMessages, normalized)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			if status != http.StatusBadRequest || envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" {
				t.Fatalf("status=%d body=%s", status, encoded)
			}
		})
	}
}

func TestDecodeCancellationAndReadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	if got, err := New().DecodeRequest(request); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("request=%+v error=%v; want context cancellation", got, err)
	}
	request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", http.NoBody)
	request.Body = io.NopCloser(failedJSONReader{})
	got, err := New().DecodeRequest(request)
	var normalized *gwerr.Error
	if got != nil || !errors.As(err, &normalized) || normalized.Kind != gwerr.KindInvalidRequest {
		t.Fatalf("request=%+v error=%v; want invalid request", got, err)
	}
}

type failedJSONReader struct{}

func (failedJSONReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestOptionalZeroAndAbsentFieldsRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   string
		present bool
	}{
		{name: "absent"},
		{name: "explicit zero", extra: `,"temperature":0,"top_p":0,"top_k":0`, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"alias","max_tokens":1,"messages":[{"role":"user","content":""}]` + tc.extra + `}`
			got, err := New().DecodeRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if err != nil {
				t.Fatal(err)
			}
			if (got.Sampling.Temperature != nil) != tc.present || (got.Sampling.TopP != nil) != tc.present || (got.Sampling.TopK != nil) != tc.present {
				t.Fatalf("optional presence changed: %+v", got.Sampling)
			}
			if tc.present && (*got.Sampling.Temperature != 0 || *got.Sampling.TopP != 0 || *got.Sampling.TopK != 0) {
				t.Fatalf("zeros changed: %+v", got.Sampling)
			}
		})
	}
}

func TestCachePrewarmingPreservesZeroBudgetAndEmptyResponse(t *testing.T) {
	system := `[{"type":"text","text":"You are an expert software engineer with deep knowledge of distributed systems...","cache_control":{"type":"ephemeral"}}]`
	body := `{"model":"claude-opus-5-5","max_tokens":0,"system":` + system + `,"messages":[{"role":"user","content":"warmup"}]}`
	a := New()
	request, err := a.DecodeRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if request.Sampling.MaxOutputTokens == nil || *request.Sampling.MaxOutputTokens != 0 || request.Stream {
		t.Fatalf("cache prewarm budget changed: %+v", request)
	}
	if string(request.Extensions["system"]) != system {
		t.Fatalf("cache breakpoint lost: %s", request.Extensions["system"])
	}
	if _, exists := request.Extensions["max_tokens"]; exists {
		t.Fatal("represented max_tokens retained as a raw extension")
	}
	response := ir.Response{
		ID: "msg-prewarm", Model: request.Model, StopReason: ir.StopMaxTokens,
		Usage: ir.Usage{InputTokens: 8, CacheWriteTokens: 5120},
	}
	encoded, err := a.EncodeResponse(&response)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      map[string]int    `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	wantUsage := map[string]int{"input_tokens": 8, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 5120}
	if got.Content == nil || len(got.Content) != 0 || got.StopReason != "max_tokens" || !reflect.DeepEqual(got.Usage, wantUsage) {
		t.Fatalf("incorrect cache prewarm response: %s", encoded)
	}
}

func TestInterleavedThinkingBudgetsAreNotLimitedByOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget string
		want   int
	}{
		{name: "minimum", budget: "1024", want: 1024},
		{name: "equal output budget", budget: "4096", want: 4096},
		{name: "exceeds output budget", budget: "8192", want: 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":` + tc.budget + `},"messages":[{"role":"user","content":"Solve this problem"}]}`
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
			request.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
			got, err := New().DecodeRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			if got.Sampling.MaxOutputTokens == nil || *got.Sampling.MaxOutputTokens != 4096 || got.Reasoning == nil || got.Reasoning.Enabled == nil || !*got.Reasoning.Enabled || got.Reasoning.MaxTokens == nil || *got.Reasoning.MaxTokens != tc.want {
				t.Fatalf("interleaved budgets changed: %+v", got)
			}
			if _, exists := got.Extensions["thinking"]; exists {
				t.Fatal("represented thinking budget retained as a raw extension")
			}
		})
	}
}

func TestMetadataUserIDPresenceAndNullablePreservation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		metadata   string
		wantUserID string
		wantRaw    string
	}{
		{name: "absent metadata"},
		{name: "absent user id", metadata: `{}`},
		{name: "null user id", metadata: `{"user_id": null}`, wantRaw: `{"user_id": null}`},
		{name: "empty user id", metadata: `{"user_id":""}`},
		{name: "nonempty user id", metadata: `{"user_id":"opaque-id"}`, wantUserID: "opaque-id"},
		{name: "null with nested extras", metadata: `{"user_id":null,"future":{"large":9007199254740993}}`, wantRaw: `{"user_id":null,"future":{"large":9007199254740993}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"alias","max_tokens":1,"messages":[{"role":"user","content":"Hello"}]`
			if tc.metadata != "" {
				body += `,"metadata":` + tc.metadata
			}
			body += `}`
			got, err := New().DecodeRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if err != nil {
				t.Fatal(err)
			}
			if tc.metadata == "" {
				if got.Metadata != nil {
					t.Fatalf("absent metadata became present: %+v", got.Metadata)
				}
			} else if got.Metadata == nil || got.Metadata.UserID != tc.wantUserID {
				t.Fatalf("metadata changed: %+v", got.Metadata)
			}
			if string(got.Extensions["metadata"]) != tc.wantRaw {
				t.Fatalf("metadata extension=%s; want %s", got.Extensions["metadata"], tc.wantRaw)
			}
		})
	}
}

func TestStopSequenceStringsPreserveEmptyAndEscapedValues(t *testing.T) {
	body := `{"model":"alias","max_tokens":1,"stop_sequences":["","END","\n"],"messages":[{"role":"user","content":"Hello"}]}`
	got, err := New().DecodeRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sampling.Stop, []string{"", "END", "\n"}) {
		t.Fatalf("stop strings changed: %#v", got.Sampling.Stop)
	}
	if _, exists := got.Extensions["stop_sequences"]; exists {
		t.Fatal("represented stop sequences retained as a raw extension")
	}
}

func TestResponseStopReasons(t *testing.T) {
	for _, tc := range []struct {
		reason ir.StopReason
		want   string
	}{
		{ir.StopEndTurn, "end_turn"},
		{ir.StopMaxTokens, "max_tokens"},
		{ir.StopSequence, "stop_sequence"},
		{ir.StopToolUse, "tool_use"},
		{ir.StopContentFilter, "refusal"},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			sequence := "END"
			response := ir.NewTextResponse("msg-id", "client-alias", "Hello", ir.Usage{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 7, CacheWriteTokens: 11})
			response.StopReason = tc.reason
			response.StopSequence = &sequence
			encoded, err := New().EncodeResponse(&response)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Model        string  `json:"model"`
				StopReason   string  `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
				Usage        struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
					Read   int `json:"cache_read_input_tokens"`
					Write  int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got.Model != "client-alias" || got.StopReason != tc.want || got.Usage.Input != 3 || got.Usage.Output != 2 || got.Usage.Read != 7 || got.Usage.Write != 11 {
				t.Fatalf("incorrect response: %s", encoded)
			}
			if tc.reason == ir.StopSequence {
				if got.StopSequence == nil || *got.StopSequence != sequence {
					t.Fatalf("missing stop sequence: %s", encoded)
				}
			} else if got.StopSequence != nil {
				t.Fatalf("stale stop sequence leaked: %s", encoded)
			}
		})
	}
}

func TestMalformedResponsesAreNotSuccessfulMessages(t *testing.T) {
	for _, response := range []*ir.Response{
		nil,
		{ID: "msg-id", Model: "alias", StopReason: ir.StopError},
		{ID: "msg-id", Model: "alias", StopReason: "unknown"},
		{ID: "msg-id", Model: "alias", StopReason: ir.StopEndTurn, Content: []ir.Block{{Type: ir.BlockImage}}},
		{ID: "msg-id", Model: "alias", StopReason: ir.StopToolUse, Content: []ir.Block{{Type: ir.BlockToolUse}}},
		{ID: "msg-id", Model: "alias", StopReason: ir.StopEndTurn, Content: []ir.Block{{Type: ir.BlockReasoning}}},
	} {
		encoded, err := New().EncodeResponse(response)
		var normalized *gwerr.Error
		if encoded != nil || !errors.As(err, &normalized) {
			t.Fatalf("encoded=%s error=%v; want normalized error", encoded, err)
		}
	}
}

func TestDecodeRequestCountTokens(t *testing.T) {
	adapter := New()

	t.Run("without max_tokens succeeds", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		got, err := adapter.DecodeRequest(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Model != "claude-3-5-sonnet" || len(got.Messages) != 1 || got.Sampling.MaxOutputTokens != nil {
			t.Fatalf("unexpected decoded request: %+v", got)
		}
	})

	t.Run("with empty messages succeeds", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","messages":[]}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		got, err := adapter.DecodeRequest(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Model != "claude-3-5-sonnet" || len(got.Messages) != 0 {
			t.Fatalf("unexpected decoded request: %+v", got)
		}
	})

	t.Run("with negative max_tokens fails", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","max_tokens":-1,"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		if _, err := adapter.DecodeRequest(req); err == nil {
			t.Fatal("expected error for negative max_tokens")
		}
	})

	t.Run("standard messages path still requires max_tokens", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
		if _, err := adapter.DecodeRequest(req); err == nil {
			t.Fatal("expected error for missing max_tokens on /v1/messages")
		}
	})
}

func TestThinkingTagsDecodeAndEncode(t *testing.T) {
	// 1. Decode text content with <thinking> tags
	body := `{"model":"alias","max_tokens":100,"messages":[{"role":"assistant","content":"<thinking>step 1: compute</thinking>The answer is 42."}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
	decoded, err := New().DecodeRequest(req)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(decoded.Messages) != 1 || len(decoded.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %+v", decoded.Messages[0].Content)
	}
	if decoded.Messages[0].Content[0].Type != ir.BlockReasoning || decoded.Messages[0].Content[0].Reasoning.Content != "step 1: compute" {
		t.Errorf("expected reasoning block 'step 1: compute', got %+v", decoded.Messages[0].Content[0])
	}
	if decoded.Messages[0].Content[1].Type != ir.BlockText || decoded.Messages[0].Content[1].Text != "The answer is 42." {
		t.Errorf("expected text block 'The answer is 42.', got %+v", decoded.Messages[0].Content[1])
	}

	// 2. Encode with FormatThinkingTags
	res := ir.Response{
		ID:         "resp_think",
		Model:      "model",
		StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			ir.NewReasoningBlock("step 1", ""),
			ir.NewTextBlock("answer"),
		},
	}
	adapterWithTags := &Adapter{
		StreamEncoder: StreamEncoder{FormatThinkingTags: true},
	}
	encoded, err := adapterWithTags.EncodeResponse(&res)
	if err != nil {
		t.Fatalf("EncodeResponse with tags: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	content := raw["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(content))
	}
	firstBlock := content[0].(map[string]any)
	if firstBlock["type"] != "text" || firstBlock["text"] != "<thinking>step 1</thinking>" {
		t.Errorf("expected text block with <thinking> tags, got %+v", firstBlock)
	}

	// 3. Encode with DisableReasoning
	adapterDisabled := &Adapter{
		StreamEncoder: StreamEncoder{DisableReasoning: true},
	}
	encodedDisabled, err := adapterDisabled.EncodeResponse(&res)
	if err != nil {
		t.Fatalf("EncodeResponse with disabled reasoning: %v", err)
	}
	var rawDisabled map[string]any
	if err := json.Unmarshal(encodedDisabled, &rawDisabled); err != nil {
		t.Fatal(err)
	}
	contentDisabled := rawDisabled["content"].([]any)
	if len(contentDisabled) != 1 {
		t.Fatalf("expected 1 block (reasoning omitted), got %d", len(contentDisabled))
	}
	if contentDisabled[0].(map[string]any)["type"] != "text" || contentDisabled[0].(map[string]any)["text"] != "answer" {
		t.Errorf("expected remaining text block 'answer', got %+v", contentDisabled[0])
	}
}

func TestThinkingTagsNotParsedInNonAssistantMessages(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		validate func(t *testing.T, req *ir.Request)
	}{
		{
			name: "user string content containing thinking tags",
			body: `{
				"model": "claude-3",
				"max_tokens": 100,
				"messages": [{"role": "user", "content": "Please answer: <thinking>internal thought</thinking> test prompt"}]
			}`,
			validate: func(t *testing.T, req *ir.Request) {
				if len(req.Messages) != 1 {
					t.Fatalf("expected 1 message, got %d", len(req.Messages))
				}
				msg := req.Messages[0]
				if msg.Role != ir.RoleUser {
					t.Fatalf("expected user role, got %s", msg.Role)
				}
				if len(msg.Content) != 1 {
					t.Fatalf("expected 1 content block, got %d", len(msg.Content))
				}
				if msg.Content[0].Type != ir.BlockText {
					t.Errorf("expected BlockText, got %s", msg.Content[0].Type)
				}
				want := "Please answer: <thinking>internal thought</thinking> test prompt"
				if msg.Content[0].Text != want {
					t.Errorf("expected text %q, got %q", want, msg.Content[0].Text)
				}
			},
		},
		{
			name: "user text block containing thinking tags",
			body: `{
				"model": "claude-3",
				"max_tokens": 100,
				"messages": [{"role": "user", "content": [{"type": "text", "text": "<thinking>pondering</thinking>hello"}]}]
			}`,
			validate: func(t *testing.T, req *ir.Request) {
				if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
					t.Fatalf("expected 1 message with 1 content block, got %+v", req.Messages)
				}
				if req.Messages[0].Content[0].Type != ir.BlockText {
					t.Errorf("expected BlockText, got %s", req.Messages[0].Content[0].Type)
				}
				if req.Messages[0].Content[0].Text != "<thinking>pondering</thinking>hello" {
					t.Errorf("got %q", req.Messages[0].Content[0].Text)
				}
			},
		},
		{
			name: "system prompt containing thinking tags",
			body: `{
				"model": "claude-3",
				"max_tokens": 100,
				"system": "You are a bot. <thinking>do not think</thinking>",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			validate: func(t *testing.T, req *ir.Request) {
				if len(req.Instructions) != 1 || len(req.Instructions[0].Content) != 1 {
					t.Fatalf("expected 1 instruction with 1 block, got %+v", req.Instructions)
				}
				if req.Instructions[0].Content[0].Type != ir.BlockText {
					t.Errorf("expected BlockText, got %s", req.Instructions[0].Content[0].Type)
				}
				want := "You are a bot. <thinking>do not think</thinking>"
				if req.Instructions[0].Content[0].Text != want {
					t.Errorf("expected %q, got %q", want, req.Instructions[0].Content[0].Text)
				}
			},
		},
		{
			name: "tool_result containing thinking tags",
			body: `{
				"model": "claude-3",
				"max_tokens": 100,
				"messages": [{
					"role": "user",
					"content": [{
						"type": "tool_result",
						"tool_use_id": "tool_123",
						"content": "<thinking>secret observation</thinking>result data"
					}]
				}]
			}`,
			validate: func(t *testing.T, req *ir.Request) {
				if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
					t.Fatalf("expected 1 message with 1 block, got %+v", req.Messages)
				}
				block := req.Messages[0].Content[0]
				if block.Type != ir.BlockToolResult {
					t.Fatalf("expected BlockToolResult, got %s", block.Type)
				}
				if len(block.ToolResult.Content) != 1 {
					t.Fatalf("expected 1 content block inside tool_result, got %d", len(block.ToolResult.Content))
				}
				inner := block.ToolResult.Content[0]
				if inner.Type != ir.BlockText {
					t.Errorf("expected tool_result inner block to be BlockText, got %s", inner.Type)
				}
				want := "<thinking>secret observation</thinking>result data"
				if inner.Text != want {
					t.Errorf("expected tool_result text %q, got %q", want, inner.Text)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			decoded, err := New().DecodeRequest(req)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			tc.validate(t, decoded)
		})
	}
}
