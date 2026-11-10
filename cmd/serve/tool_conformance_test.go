package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewaypolicy "github.com/minhhhduc/gnvipi/internal/gateway/policy"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

type toolParamDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type toolResultPayload struct {
	ToolUseID string
	Content   string
	IsError   bool
	ImageB64  string // if set, image block rather than text
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// buildTurn1Request generates the protocol-appropriate Turn 1 payload containing tool definitions.

// TestTool_FullAgentLoopMatrix verifies a complete 2-turn tool loop across all 9 Inbound x Upstream pairs,
// both streaming and non-streaming (18 total configurations).
func TestTool_FullAgentLoopMatrix(t *testing.T) {
	inboundProtocols := []struct {
		name  string
		proto ir.Protocol
	}{
		{"InboundChat", ir.ProtocolOpenAIChat},
		{"InboundAnthropic", ir.ProtocolAnthropicMessages},
		{"InboundResponses", ir.ProtocolOpenAIResponses},
	}

	upstreamProtocols := []struct {
		name  string
		proto ir.Protocol
	}{
		{"UpstreamChat", ir.ProtocolOpenAIChat},
		{"UpstreamAnthropic", ir.ProtocolAnthropicMessages},
		{"UpstreamResponses", ir.ProtocolOpenAIResponses},
	}

	for _, in := range inboundProtocols {
		for _, up := range upstreamProtocols {
			for _, stream := range []bool{false, true} {
				streamName := "NonStream"
				if stream {
					streamName = "Stream"
				}
				testName := fmt.Sprintf("%s_x_%s_%s", in.name, up.name, streamName)

				t.Run(testName, func(t *testing.T) {
					toolID := "call_weather_42"
					turn1Calls := []parsedToolCall{
						{
							ID:        toolID,
							Name:      "get_weather",
							Arguments: `{"city":"Tokyo"}`,
						},
					}
					toolResult := toolResultPayload{
						ToolUseID: toolID,
						Content:   `{"temperature":22,"condition":"sunny"}`,
					}
					finalAnswer := "The weather in Tokyo is sunny and 22C."

					server := createMockUpstreamServer(t, up.proto, turn1Calls, stream, []toolResultPayload{toolResult}, finalAnswer)
					defer server.Close()

					var providers []customProvider
					switch up.proto {
					case ir.ProtocolOpenAIChat:
						providers = append(providers, customProvider{Name: "prov", Model: "model-x", BaseURL: server.URL})
					case ir.ProtocolAnthropicMessages:
						providers = append(providers, customProvider{Name: "prov", Model: "model-x", BaseURL: server.URL, MessagesOnly: true})
					case ir.ProtocolOpenAIResponses:
						providers = append(providers, customProvider{Name: "prov", Model: "model-x", BaseURL: server.URL, ResponsesOnly: true})
					}

					engine := setupConformanceGateway(t, providers, nil)

					tools := []toolParamDef{
						{
							Name:        "get_weather",
							Description: "Get weather for a city",
							Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
						},
					}

					// Turn 1: Client sends request with tools
					path, body, err := buildTurn1Request(in.proto, "prov/model-x", stream, tools, nil)
					if err != nil {
						t.Fatalf("buildTurn1Request error: %v", err)
					}
					req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
					req1.Header.Set("Content-Type", "application/json")
					rec1 := httptest.NewRecorder()
					engine.ServeHTTP(rec1, req1)

					if rec1.Code != http.StatusOK {
						t.Fatalf("Turn 1 status %d, body: %s", rec1.Code, rec1.Body.String())
					}

					parsedCalls := parseTurn1ToolCalls(t, in.proto, stream, rec1.Body.Bytes())
					if len(parsedCalls) != 1 {
						t.Fatalf("expected 1 tool call, got %d. Body: %s", len(parsedCalls), rec1.Body.String())
					}

					call := parsedCalls[0]
					if call.ID != toolID {
						t.Errorf("tool ID mismatch: got %q, want %q", call.ID, toolID)
					}
					if call.Name != "get_weather" {
						t.Errorf("tool name mismatch: got %q, want get_weather", call.Name)
					}

					// Assert tool arguments are valid JSON
					var argsMap map[string]any
					if err := json.Unmarshal([]byte(call.Arguments), &argsMap); err != nil {
						t.Fatalf("tool arguments is not valid JSON: %v, raw: %q", err, call.Arguments)
					}
					if argsMap["city"] != "Tokyo" {
						t.Errorf("expected city Tokyo, got %v", argsMap["city"])
					}

					// Turn 2: Client sends tool result
					path2, body2, err := buildTurn2Request(in.proto, "prov/model-x", stream, parsedCalls, []toolResultPayload{toolResult})
					if err != nil {
						t.Fatalf("buildTurn2Request error: %v", err)
					}
					req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2))
					req2.Header.Set("Content-Type", "application/json")
					rec2 := httptest.NewRecorder()
					engine.ServeHTTP(rec2, req2)

					if rec2.Code != http.StatusOK {
						t.Fatalf("Turn 2 status %d, body: %s", rec2.Code, rec2.Body.String())
					}

					answer := parseTurn2FinalText(t, in.proto, stream, rec2.Body.Bytes())
					if !strings.Contains(answer, finalAnswer) {
						t.Errorf("final answer text mismatch: got %q, want %q", answer, finalAnswer)
					}
				})
			}
		}
	}
}

// TestTool_ParallelInvocations verifies parallel tool calls execution, ID preservation, and argument validity.
func TestTool_ParallelInvocations(t *testing.T) {
	pairs := []struct {
		name    string
		inProto ir.Protocol
		upProto ir.Protocol
	}{
		{"Anthropic_to_Chat", ir.ProtocolAnthropicMessages, ir.ProtocolOpenAIChat},
		{"Chat_to_Anthropic", ir.ProtocolOpenAIChat, ir.ProtocolAnthropicMessages},
		{"Responses_to_Anthropic", ir.ProtocolOpenAIResponses, ir.ProtocolAnthropicMessages},
		{"Responses_to_Chat", ir.ProtocolOpenAIResponses, ir.ProtocolOpenAIChat},
	}

	for _, p := range pairs {
		for _, stream := range []bool{false, true} {
			sName := "NonStream"
			if stream {
				sName = "Stream"
			}
			t.Run(fmt.Sprintf("%s_%s", p.name, sName), func(t *testing.T) {
				turn1Calls := []parsedToolCall{
					{ID: "call_w_1", Name: "get_weather", Arguments: `{"city":"Tokyo"}`},
					{ID: "call_t_2", Name: "get_time", Arguments: `{"city":"Tokyo"}`},
				}
				results := []toolResultPayload{
					{ToolUseID: "call_w_1", Content: `{"temp":22}`},
					{ToolUseID: "call_t_2", Content: `{"time":"12:00"}`},
				}
				finalAnswer := "Weather is 22C and time is 12:00."

				server := createMockUpstreamServer(t, p.upProto, turn1Calls, stream, results, finalAnswer)
				defer server.Close()

				var providers []customProvider
				if p.upProto == ir.ProtocolAnthropicMessages {
					providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL, MessagesOnly: true})
				} else {
					providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL})
				}

				engine := setupConformanceGateway(t, providers, nil)

				tools := []toolParamDef{
					{Name: "get_weather", Description: "Get weather", Schema: json.RawMessage(`{"type":"object"}`)},
					{Name: "get_time", Description: "Get time", Schema: json.RawMessage(`{"type":"object"}`)},
				}

				path1, body1, err := buildTurn1Request(p.inProto, "prov/m1", stream, tools, nil)
				if err != nil {
					t.Fatal(err)
				}
				req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path1, bytes.NewReader(body1))
				rec1 := httptest.NewRecorder()
				engine.ServeHTTP(rec1, req1)

				if rec1.Code != http.StatusOK {
					t.Fatalf("Turn 1 status %d, body: %s", rec1.Code, rec1.Body.String())
				}

				parsed := parseTurn1ToolCalls(t, p.inProto, stream, rec1.Body.Bytes())
				if len(parsed) != 2 {
					t.Fatalf("expected 2 tool calls, got %d", len(parsed))
				}
				if parsed[0].ID != "call_w_1" || parsed[0].Name != "get_weather" {
					t.Errorf("call 0 mismatch: %+v", parsed[0])
				}
				if parsed[1].ID != "call_t_2" || parsed[1].Name != "get_time" {
					t.Errorf("call 1 mismatch: %+v", parsed[1])
				}

				// Validate JSON
				for _, c := range parsed {
					if !json.Valid([]byte(c.Arguments)) {
						t.Errorf("call %s has invalid JSON arguments: %q", c.ID, c.Arguments)
					}
				}

				path2, body2, err := buildTurn2Request(p.inProto, "prov/m1", stream, parsed, results)
				if err != nil {
					t.Fatal(err)
				}
				req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2))
				rec2 := httptest.NewRecorder()
				engine.ServeHTTP(rec2, req2)

				if rec2.Code != http.StatusOK {
					t.Fatalf("Turn 2 status %d, body: %s", rec2.Code, rec2.Body.String())
				}

				ans := parseTurn2FinalText(t, p.inProto, stream, rec2.Body.Bytes())
				if !strings.Contains(ans, finalAnswer) {
					t.Errorf("final answer mismatch: got %q, want %q", ans, finalAnswer)
				}
			})
		}
	}
}

// TestTool_ErrorResultRoundTrip tests tool execution error round-trip.
func TestTool_ErrorResultRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		sName := "NonStream"
		if stream {
			sName = "Stream"
		}
		t.Run(sName, func(t *testing.T) {
			turn1Calls := []parsedToolCall{
				{ID: "call_err_1", Name: "exec", Arguments: `{"cmd":"exit 1"}`},
			}
			results := []toolResultPayload{
				{ToolUseID: "call_err_1", Content: "command failed with code 1", IsError: true},
			}
			finalAnswer := "The tool failed as expected."

			server := createMockUpstreamServer(t, ir.ProtocolAnthropicMessages, turn1Calls, stream, results, finalAnswer)
			defer server.Close()

			engine := setupConformanceGateway(t, []customProvider{
				{Name: "prov", Model: "m1", BaseURL: server.URL, MessagesOnly: true},
			}, nil)

			tools := []toolParamDef{
				{Name: "exec", Description: "Exec cmd", Schema: json.RawMessage(`{"type":"object"}`)},
			}

			path1, body1, err := buildTurn1Request(ir.ProtocolAnthropicMessages, "prov/m1", stream, tools, nil)
			if err != nil {
				t.Fatal(err)
			}
			req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path1, bytes.NewReader(body1))
			rec1 := httptest.NewRecorder()
			engine.ServeHTTP(rec1, req1)

			parsed := parseTurn1ToolCalls(t, ir.ProtocolAnthropicMessages, stream, rec1.Body.Bytes())
			if len(parsed) != 1 || parsed[0].ID != "call_err_1" {
				t.Fatalf("unexpected calls: %+v", parsed)
			}

			path2, body2, err := buildTurn2Request(ir.ProtocolAnthropicMessages, "prov/m1", stream, parsed, results)
			if err != nil {
				t.Fatal(err)
			}
			req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2))
			rec2 := httptest.NewRecorder()
			engine.ServeHTTP(rec2, req2)

			if rec2.Code != http.StatusOK {
				t.Fatalf("Turn 2 error: %d, body: %s", rec2.Code, rec2.Body.String())
			}
			ans := parseTurn2FinalText(t, ir.ProtocolAnthropicMessages, stream, rec2.Body.Bytes())
			if !strings.Contains(ans, finalAnswer) {
				t.Fatalf("expected final answer %q, got %q", finalAnswer, ans)
			}
		})
	}
}

// TestTool_ImageResultRoundTrip verifies multimodal image tool result handling across protocols.
func TestTool_ImageResultRoundTrip(t *testing.T) {
	const pixelB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

	t.Run("Anthropic_to_Anthropic_WithImageResult", func(t *testing.T) {
		turn1Calls := []parsedToolCall{
			{ID: "call_snap_1", Name: "take_screenshot", Arguments: `{}`},
		}
		results := []toolResultPayload{
			{ToolUseID: "call_snap_1", ImageB64: pixelB64},
		}
		finalAnswer := "I see a 1x1 image."

		server := createMockUpstreamServer(t, ir.ProtocolAnthropicMessages, turn1Calls, false, results, finalAnswer)
		defer server.Close()

		engine := setupConformanceGateway(t, []customProvider{
			{Name: "prov", Model: "m1", BaseURL: server.URL, MessagesOnly: true},
		}, nil)

		tools := []toolParamDef{
			{Name: "take_screenshot", Description: "Take screenshot", Schema: json.RawMessage(`{"type":"object"}`)},
		}

		path1, body1, err := buildTurn1Request(ir.ProtocolAnthropicMessages, "prov/m1", false, tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec1 := httptest.NewRecorder()
		engine.ServeHTTP(rec1, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path1, bytes.NewReader(body1)))

		parsed := parseTurn1ToolCalls(t, ir.ProtocolAnthropicMessages, false, rec1.Body.Bytes())

		path2, body2, err := buildTurn2Request(ir.ProtocolAnthropicMessages, "prov/m1", false, parsed, results)
		if err != nil {
			t.Fatal(err)
		}
		rec2 := httptest.NewRecorder()
		engine.ServeHTTP(rec2, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2)))

		if rec2.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", rec2.Code, rec2.Body.String())
		}
	})

	t.Run("Anthropic_to_Responses_WithImageResult", func(t *testing.T) {
		turn1Calls := []parsedToolCall{
			{ID: "call_snap_2", Name: "take_screenshot", Arguments: `{}`},
		}
		results := []toolResultPayload{
			{ToolUseID: "call_snap_2", ImageB64: pixelB64},
		}
		finalAnswer := "Image received via responses function_call_output."

		server := createMockUpstreamServer(t, ir.ProtocolOpenAIResponses, turn1Calls, false, results, finalAnswer)
		defer server.Close()

		engine := setupConformanceGateway(t, []customProvider{
			{Name: "prov", Model: "m1", BaseURL: server.URL, ResponsesOnly: true},
		}, nil)

		tools := []toolParamDef{
			{Name: "take_screenshot", Description: "Take screenshot", Schema: json.RawMessage(`{"type":"object"}`)},
		}

		path1, body1, err := buildTurn1Request(ir.ProtocolAnthropicMessages, "prov/m1", false, tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec1 := httptest.NewRecorder()
		engine.ServeHTTP(rec1, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path1, bytes.NewReader(body1)))

		parsed := parseTurn1ToolCalls(t, ir.ProtocolAnthropicMessages, false, rec1.Body.Bytes())

		path2, body2, err := buildTurn2Request(ir.ProtocolAnthropicMessages, "prov/m1", false, parsed, results)
		if err != nil {
			t.Fatal(err)
		}
		rec2 := httptest.NewRecorder()
		engine.ServeHTTP(rec2, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2)))

		if rec2.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", rec2.Code, rec2.Body.String())
		}
	})

	t.Run("Anthropic_to_OpenAIChat_RejectsImageResult", func(t *testing.T) {
		// OpenAI Chat upstream cannot accept images in tool messages; gateway rejects with 400
		turn1Calls := []parsedToolCall{
			{ID: "call_snap_3", Name: "take_screenshot", Arguments: `{}`},
		}
		results := []toolResultPayload{
			{ToolUseID: "call_snap_3", ImageB64: pixelB64},
		}

		server := createMockUpstreamServer(t, ir.ProtocolOpenAIChat, turn1Calls, false, results, "")
		defer server.Close()

		engine := setupConformanceGateway(t, []customProvider{
			{Name: "prov", Model: "m1", BaseURL: server.URL},
		}, nil)

		path2, body2, err := buildTurn2Request(ir.ProtocolAnthropicMessages, "prov/m1", false, turn1Calls, results)
		if err != nil {
			t.Fatal(err)
		}
		rec2 := httptest.NewRecorder()
		engine.ServeHTTP(rec2, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path2, bytes.NewReader(body2)))

		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for OpenAI Chat image tool result, got %d, body: %s", rec2.Code, rec2.Body.String())
		}
	})
}

// TestTool_ToolChoice verifies tool_choice options (auto, required/forced, none, specific tool) across protocols.
func TestTool_ToolChoice(t *testing.T) {
	testCases := []struct {
		name                 string
		inProto              ir.Protocol
		upProto              ir.Protocol
		clientChoice         any
		expectedUpstreamBody string
	}{
		{
			name:                 "Chat_to_Anthropic_Auto",
			inProto:              ir.ProtocolOpenAIChat,
			upProto:              ir.ProtocolAnthropicMessages,
			clientChoice:         "auto",
			expectedUpstreamBody: `"tool_choice":{"type":"auto"}`,
		},
		{
			name:                 "Chat_to_Anthropic_None",
			inProto:              ir.ProtocolOpenAIChat,
			upProto:              ir.ProtocolAnthropicMessages,
			clientChoice:         "none",
			expectedUpstreamBody: `"tool_choice":{"type":"none"}`,
		},
		{
			name:                 "Chat_to_Anthropic_Required",
			inProto:              ir.ProtocolOpenAIChat,
			upProto:              ir.ProtocolAnthropicMessages,
			clientChoice:         "required",
			expectedUpstreamBody: `"tool_choice":{"type":"any"}`,
		},
		{
			name:                 "Chat_to_Anthropic_SpecificTool",
			inProto:              ir.ProtocolOpenAIChat,
			upProto:              ir.ProtocolAnthropicMessages,
			clientChoice:         map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
			expectedUpstreamBody: `"tool_choice":{"type":"tool","name":"lookup"}`,
		},
		{
			name:                 "Anthropic_to_Chat_Required",
			inProto:              ir.ProtocolAnthropicMessages,
			upProto:              ir.ProtocolOpenAIChat,
			clientChoice:         map[string]any{"type": "any"},
			expectedUpstreamBody: `"tool_choice":"required"`,
		},
		{
			name:                 "Anthropic_to_Chat_None",
			inProto:              ir.ProtocolAnthropicMessages,
			upProto:              ir.ProtocolOpenAIChat,
			clientChoice:         map[string]any{"type": "none"},
			expectedUpstreamBody: `"tool_choice":"none"`,
		},
		{
			name:                 "Responses_to_Chat_Required",
			inProto:              ir.ProtocolOpenAIResponses,
			upProto:              ir.ProtocolOpenAIChat,
			clientChoice:         "required",
			expectedUpstreamBody: `"tool_choice":"required"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var receivedBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read body error: %v", readErr)
				}
				receivedBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				switch tc.upProto {
				case ir.ProtocolOpenAIChat:
					if encodeErr := json.NewEncoder(w).Encode(map[string]any{
						"id":    "c1",
						"model": "upstream-model",
						"choices": []map[string]any{{
							"index":         0,
							"message":       map[string]any{"role": "assistant", "content": "ok"},
							"finish_reason": "stop",
						}},
					}); encodeErr != nil {
						t.Errorf("encode error: %v", encodeErr)
					}
				case ir.ProtocolAnthropicMessages:
					if encodeErr := json.NewEncoder(w).Encode(map[string]any{
						"id":          "m1",
						"type":        "message",
						"role":        "assistant",
						"model":       "upstream-model",
						"content":     []map[string]any{{"type": "text", "text": "ok"}},
						"stop_reason": "end_turn",
					}); encodeErr != nil {
						t.Errorf("encode error: %v", encodeErr)
					}
				}
			}))
			defer server.Close()

			var providers []customProvider
			if tc.upProto == ir.ProtocolAnthropicMessages {
				providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL, MessagesOnly: true})
			} else {
				providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL})
			}

			engine := setupConformanceGateway(t, providers, nil)

			tools := []toolParamDef{
				{Name: "lookup", Description: "Lookup value", Schema: json.RawMessage(`{"type":"object"}`)},
			}

			path, body, err := buildTurn1Request(tc.inProto, "prov/m1", false, tools, tc.clientChoice)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body: %s", rec.Code, rec.Body.String())
			}

			// Clean whitespace before comparing
			compactReceived := strings.ReplaceAll(strings.ReplaceAll(receivedBody, " ", ""), "\n", "")
			compactExpected := strings.ReplaceAll(strings.ReplaceAll(tc.expectedUpstreamBody, " ", ""), "\n", "")
			if !strings.Contains(compactReceived, compactExpected) {
				t.Errorf("upstream received body does not contain expected tool_choice:\ngot: %s\nwant: %s", compactReceived, compactExpected)
			}
		})
	}
}

// TestTool_StreamedFragmentedArguments verifies that fragmented tool call arguments streaming across chunks
// are cleanly reassembled into valid JSON with preserved tool IDs.
func TestTool_StreamedFragmentedArguments(t *testing.T) {
	pairs := []struct {
		name    string
		inProto ir.Protocol
		upProto ir.Protocol
	}{
		{"Anthropic_from_Chat", ir.ProtocolAnthropicMessages, ir.ProtocolOpenAIChat},
		{"Chat_from_Anthropic", ir.ProtocolOpenAIChat, ir.ProtocolAnthropicMessages},
		{"Responses_from_Anthropic", ir.ProtocolOpenAIResponses, ir.ProtocolAnthropicMessages},
		{"Responses_from_Chat", ir.ProtocolOpenAIResponses, ir.ProtocolOpenAIChat},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			toolID := "call_frag_123"
			complexJSON := `{"nested":{"key":"value","items":[1,2,3]},"query":"test"}`
			turn1Calls := []parsedToolCall{
				{ID: toolID, Name: "search_data", Arguments: complexJSON},
			}

			// fragmentArguments = true splits the arguments string across multiple SSE chunks
			server := createMockUpstreamServer(t, p.upProto, turn1Calls, true, nil, "")
			defer server.Close()

			var providers []customProvider
			if p.upProto == ir.ProtocolAnthropicMessages {
				providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL, MessagesOnly: true})
			} else {
				providers = append(providers, customProvider{Name: "prov", Model: "m1", BaseURL: server.URL})
			}

			engine := setupConformanceGateway(t, providers, nil)

			tools := []toolParamDef{
				{Name: "search_data", Description: "Search", Schema: json.RawMessage(`{"type":"object"}`)},
			}

			path, body, err := buildTurn1Request(p.inProto, "prov/m1", true, tools, nil)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body: %s", rec.Code, rec.Body.String())
			}

			parsed := parseTurn1ToolCalls(t, p.inProto, true, rec.Body.Bytes())
			if len(parsed) != 1 {
				t.Fatalf("expected 1 tool call, got %d. Body: %s", len(parsed), rec.Body.String())
			}

			if parsed[0].ID != toolID {
				t.Errorf("tool ID mismatch: got %q, want %q", parsed[0].ID, toolID)
			}
			if parsed[0].Name != "search_data" {
				t.Errorf("tool name mismatch: got %q, want search_data", parsed[0].Name)
			}

			// Validate reassembled JSON
			var parsedObj map[string]any
			if err := json.Unmarshal([]byte(parsed[0].Arguments), &parsedObj); err != nil {
				t.Fatalf("arguments not valid JSON after streaming reassembly: %v, got %q", err, parsed[0].Arguments)
			}
			nested, ok := parsedObj["nested"].(map[string]any)
			if !ok || nested["key"] != "value" {
				t.Errorf("nested JSON content corrupted: %+v", parsedObj)
			}
		})
	}
}

// TestTool_UnsupportedCapabilities verifies documented #34 capability rejections (400 Bad Request).
func TestTool_UnsupportedCapabilities(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "models.json")
	store, err := gatewayprovider.OpenStore(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)); writeErr != nil {
			t.Errorf("write error: %v", writeErr)
		}
	}))
	defer server.Close()

	// Provider with tools unsupported
	noToolsCaps, err := json.Marshal(gatewaypolicy.CapabilityOverrides{
		Tools: func() *bool { b := false; return &b }(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Provider with streaming unsupported
	noStreamCaps, err := json.Marshal(gatewaypolicy.CapabilityOverrides{
		Streaming: func() *bool { b := false; return &b }(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Provider with parallel tool calls unsupported in strict mode
	noParallelCaps, err := json.Marshal(gatewaypolicy.CapabilityOverrides{
		ParallelToolCalls: func() *bool { b := false; return &b }(),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.Update(func(doc *gatewayprovider.Document) error {
		doc.Providers = []gatewayprovider.Provider{
			{
				ID:       "p-notools",
				Name:     "NoTools",
				BaseURL:  server.URL,
				Protocol: ir.ProtocolOpenAIChat,
				Enabled:  true,
				Extensions: map[string]json.RawMessage{
					"capabilities": noToolsCaps,
				},
				Models: []gatewayprovider.ModelMapping{
					{Alias: "p-notools/model", UpstreamModel: "m1"},
				},
			},
			{
				ID:       "p-nostream",
				Name:     "NoStream",
				BaseURL:  server.URL,
				Protocol: ir.ProtocolOpenAIChat,
				Enabled:  true,
				Extensions: map[string]json.RawMessage{
					"capabilities": noStreamCaps,
				},
				Models: []gatewayprovider.ModelMapping{
					{Alias: "p-nostream/model", UpstreamModel: "m2"},
				},
			},
			{
				ID:       "p-noparallel",
				Name:     "NoParallel",
				BaseURL:  server.URL,
				Protocol: ir.ProtocolAnthropicMessages,
				Enabled:  true,
				Extensions: map[string]json.RawMessage{
					"capabilities":   noParallelCaps,
					"feature_policy": json.RawMessage(`"strict"`),
				},
				Models: []gatewayprovider.ModelMapping{
					{Alias: "p-noparallel/model", UpstreamModel: "m3"},
				},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	engine := setupConformanceGateway(t, nil, store)

	t.Run("Reject_Tools_When_Unsupported", func(t *testing.T) {
		tools := []toolParamDef{
			{Name: "test_tool", Schema: json.RawMessage(`{"type":"object"}`)},
		}
		path, body, err := buildTurn1Request(ir.ProtocolAnthropicMessages, "p-notools/model", false, tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "tools are not supported by target upstream") {
			t.Errorf("error body does not mention tools unsupported: %s", rec.Body.String())
		}
	})

	t.Run("Reject_Streaming_When_Unsupported", func(t *testing.T) {
		path, body, err := buildTurn1Request(ir.ProtocolAnthropicMessages, "p-nostream/model", true, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "streaming is not supported by target upstream") {
			t.Errorf("error body does not mention streaming unsupported: %s", rec.Body.String())
		}
	})

	t.Run("Reject_ParallelToolCalls_In_StrictMode", func(t *testing.T) {
		// When inbound requests parallel_tool_calls in strict mode on an upstream without parallel tool calls capability
		reqBody := `{
			"model": "p-noparallel/model",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [{"type": "function", "function": {"name": "test_tool"}}],
			"parallel_tool_calls": true
		}`
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "parallel_tool_calls") {
			t.Errorf("error body does not mention parallel_tool_calls: %s", rec.Body.String())
		}
	})
}
