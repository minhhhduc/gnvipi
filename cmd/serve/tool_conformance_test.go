package main

import (
	"bufio"
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

	"github.com/gin-gonic/gin"
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
func buildTurn1Request(proto ir.Protocol, model string, stream bool, tools []toolParamDef, toolChoice any) (path string, payload []byte, err error) {
	switch proto {
	case ir.ProtocolOpenAIChat:
		req := map[string]any{
			"model":  model,
			"stream": stream,
			"messages": []map[string]any{
				{"role": "user", "content": "What is the weather?"},
			},
		}
		if len(tools) > 0 {
			var wireTools []map[string]any
			for _, t := range tools {
				wireTools = append(wireTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        t.Name,
						"description": t.Description,
						"parameters":  t.Schema,
					},
				})
			}
			req["tools"] = wireTools
		}
		if toolChoice != nil {
			req["tool_choice"] = toolChoice
		}
		data, marshalErr := json.Marshal(req)
		return "/v1/chat/completions", data, marshalErr

	case ir.ProtocolAnthropicMessages:
		req := map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": 1024,
			"messages": []map[string]any{
				{"role": "user", "content": "What is the weather?"},
			},
		}
		if len(tools) > 0 {
			var wireTools []map[string]any
			for _, t := range tools {
				wireTools = append(wireTools, map[string]any{
					"name":         t.Name,
					"description":  t.Description,
					"input_schema": t.Schema,
				})
			}
			req["tools"] = wireTools
		}
		if toolChoice != nil {
			req["tool_choice"] = toolChoice
		}
		data, marshalErr := json.Marshal(req)
		return "/v1/messages", data, marshalErr

	case ir.ProtocolOpenAIResponses:
		req := map[string]any{
			"model":  model,
			"stream": stream,
			"input": []map[string]any{
				{"role": "user", "content": "What is the weather?"},
			},
		}
		if len(tools) > 0 {
			var wireTools []map[string]any
			for _, t := range tools {
				wireTools = append(wireTools, map[string]any{
					"type":        "function",
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Schema,
				})
			}
			req["tools"] = wireTools
		}
		if toolChoice != nil {
			req["tool_choice"] = toolChoice
		}
		data, marshalErr := json.Marshal(req)
		return "/v1/responses", data, marshalErr

	default:
		return "", nil, fmt.Errorf("unsupported protocol %s", proto)
	}
}

// buildTurn2Request generates the protocol-appropriate Turn 2 payload echoing the previous tool call and providing tool results.
func buildTurn2Request(proto ir.Protocol, model string, stream bool, calls []parsedToolCall, results []toolResultPayload) (path string, payload []byte, err error) {
	switch proto {
	case ir.ProtocolOpenAIChat:
		var toolCallsWire []map[string]any
		for _, c := range calls {
			toolCallsWire = append(toolCallsWire, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Arguments,
				},
			})
		}
		messages := []map[string]any{
			{"role": "user", "content": "What is the weather?"},
			{"role": "assistant", "content": nil, "tool_calls": toolCallsWire},
		}
		for _, r := range results {
			if r.ImageB64 != "" {
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": r.ToolUseID,
					"content": []map[string]any{
						{
							"type": "image_url",
							"image_url": map[string]any{
								"url": "data:image/png;base64," + r.ImageB64,
							},
						},
					},
				})
			} else {
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": r.ToolUseID,
					"content":      r.Content,
				})
			}
		}
		data, marshalErr := json.Marshal(map[string]any{
			"model":    model,
			"stream":   stream,
			"messages": messages,
		})
		return "/v1/chat/completions", data, marshalErr

	case ir.ProtocolAnthropicMessages:
		var assistantContent []map[string]any
		for _, c := range calls {
			var parsedArgs any
			if unmarshalErr := json.Unmarshal([]byte(c.Arguments), &parsedArgs); unmarshalErr != nil {
				parsedArgs = map[string]any{}
			}
			assistantContent = append(assistantContent, map[string]any{
				"type":  "tool_use",
				"id":    c.ID,
				"name":  c.Name,
				"input": parsedArgs,
			})
		}
		var userToolResults []map[string]any
		for _, r := range results {
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": r.ToolUseID,
			}
			if r.IsError {
				block["is_error"] = true
			}
			if r.ImageB64 != "" {
				block["content"] = []map[string]any{
					{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       r.ImageB64,
						},
					},
				}
			} else {
				block["content"] = r.Content
			}
			userToolResults = append(userToolResults, block)
		}
		messages := []map[string]any{
			{"role": "user", "content": "What is the weather?"},
			{"role": "assistant", "content": assistantContent},
			{"role": "user", "content": userToolResults},
		}
		data, marshalErr := json.Marshal(map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": 1024,
			"messages":   messages,
		})
		return "/v1/messages", data, marshalErr

	case ir.ProtocolOpenAIResponses:
		var items []map[string]any
		items = append(items, map[string]any{"role": "user", "content": "What is the weather?"})
		for _, c := range calls {
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   c.ID,
				"name":      c.Name,
				"arguments": c.Arguments,
			})
		}
		for _, r := range results {
			if r.ImageB64 != "" {
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": r.ToolUseID,
					"output": []map[string]any{
						{
							"type":      "input_image",
							"image_url": "data:image/png;base64," + r.ImageB64,
						},
					},
				})
			} else {
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": r.ToolUseID,
					"output":  r.Content,
				})
			}
		}
		data, marshalErr := json.Marshal(map[string]any{
			"model":  model,
			"stream": stream,
			"input":  items,
		})
		return "/v1/responses", data, marshalErr

	default:
		return "", nil, fmt.Errorf("unsupported protocol %s", proto)
	}
}

// parseTurn1ToolCalls extracts tool call definitions from Turn 1 response body.
func parseTurn1ToolCalls(t *testing.T, proto ir.Protocol, stream bool, raw []byte) []parsedToolCall {
	t.Helper()
	if !stream {
		return parseTurn1NonStream(t, proto, raw)
	}
	return parseTurn1Stream(t, proto, raw)
}

func parseTurn1NonStream(t *testing.T, proto ir.Protocol, raw []byte) []parsedToolCall {
	t.Helper()
	var calls []parsedToolCall
	switch proto {
	case ir.ProtocolOpenAIChat:
		var resp struct {
			Choices []struct {
				Message struct {
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
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("unmarshal chat tool response: %v, raw: %s", err, string(raw))
		}
		if len(resp.Choices) > 0 {
			for _, tc := range resp.Choices[0].Message.ToolCalls {
				calls = append(calls, parsedToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}

	case ir.ProtocolAnthropicMessages:
		var resp struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("unmarshal anthropic tool response: %v, raw: %s", err, string(raw))
		}
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				calls = append(calls, parsedToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: string(block.Input),
				})
			}
		}

	case ir.ProtocolOpenAIResponses:
		var resp struct {
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("unmarshal responses tool response: %v, raw: %s", err, string(raw))
		}
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				calls = append(calls, parsedToolCall{
					ID:        item.CallID,
					Name:      item.Name,
					Arguments: item.Arguments,
				})
			}
		}
	}
	return calls
}

func parseTurn1Stream(t *testing.T, proto ir.Protocol, raw []byte) []parsedToolCall {
	t.Helper()
	var calls []parsedToolCall
	scanner := bufio.NewScanner(bytes.NewReader(raw))

	switch proto {
	case ir.ProtocolOpenAIChat:
		toolMap := make(map[int]*parsedToolCall)
		var maxIndex int
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if strings.TrimSpace(payload) == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				idx := tc.Index
				if idx > maxIndex {
					maxIndex = idx
				}
				call, exists := toolMap[idx]
				if !exists {
					call = &parsedToolCall{}
					toolMap[idx] = call
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				call.Arguments += tc.Function.Arguments
			}
		}
		for i := 0; i <= maxIndex; i++ {
			if call, ok := toolMap[i]; ok {
				calls = append(calls, *call)
			}
		}

	case ir.ProtocolAnthropicMessages:
		toolMap := make(map[int]*parsedToolCall)
		var maxIndex int
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			switch currentEvent {
			case "content_block_start":
				var start struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if err := json.Unmarshal([]byte(payload), &start); err == nil && start.ContentBlock.Type == "tool_use" {
					if start.Index > maxIndex {
						maxIndex = start.Index
					}
					toolMap[start.Index] = &parsedToolCall{
						ID:   start.ContentBlock.ID,
						Name: start.ContentBlock.Name,
					}
				}
			case "content_block_delta":
				var delta struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &delta); err == nil && delta.Delta.Type == "input_json_delta" {
					if call, ok := toolMap[delta.Index]; ok {
						call.Arguments += delta.Delta.PartialJSON
					}
				}
			}
		}
		for i := 0; i <= maxIndex; i++ {
			if call, ok := toolMap[i]; ok {
				calls = append(calls, *call)
			}
		}

	case ir.ProtocolOpenAIResponses:
		toolMap := make(map[int]*parsedToolCall)
		var maxIndex int
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			switch currentEvent {
			case "response.output_item.added":
				var added struct {
					OutputIndex int `json:"output_index"`
					Item        struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Name   string `json:"name"`
					} `json:"item"`
				}
				if err := json.Unmarshal([]byte(payload), &added); err == nil && added.Item.Type == "function_call" {
					if added.OutputIndex > maxIndex {
						maxIndex = added.OutputIndex
					}
					toolMap[added.OutputIndex] = &parsedToolCall{
						ID:   added.Item.CallID,
						Name: added.Item.Name,
					}
				}
			case "response.function_call_arguments.delta":
				var delta struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &delta); err == nil {
					if call, ok := toolMap[delta.OutputIndex]; ok {
						call.Arguments += delta.Delta
					}
				}
			}
		}
		for i := 0; i <= maxIndex; i++ {
			if call, ok := toolMap[i]; ok {
				calls = append(calls, *call)
			}
		}
	}
	return calls
}

// parseTurn2FinalText extracts the final response text from Turn 2 response body.
func parseTurn2FinalText(t *testing.T, proto ir.Protocol, stream bool, raw []byte) string {
	t.Helper()
	if !stream {
		switch proto {
		case ir.ProtocolOpenAIChat:
			var resp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal chat text: %v, raw: %s", err, string(raw))
			}
			if len(resp.Choices) > 0 {
				return resp.Choices[0].Message.Content
			}
		case ir.ProtocolAnthropicMessages:
			var resp struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal anthropic text: %v, raw: %s", err, string(raw))
			}
			var sb strings.Builder
			for _, b := range resp.Content {
				if b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		case ir.ProtocolOpenAIResponses:
			var resp struct {
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal responses text: %v, raw: %s", err, string(raw))
			}
			var sb strings.Builder
			for _, item := range resp.Output {
				for _, c := range item.Content {
					if c.Type == "output_text" {
						sb.WriteString(c.Text)
					}
				}
			}
			return sb.String()
		}
		return ""
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	var sb strings.Builder
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(payload) == "[DONE]" {
			break
		}

		switch proto {
		case ir.ProtocolOpenAIChat:
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err == nil && len(chunk.Choices) > 0 {
				sb.WriteString(chunk.Choices[0].Delta.Content)
			}
		case ir.ProtocolAnthropicMessages:
			if currentEvent == "content_block_delta" {
				var delta struct {
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &delta); err == nil && delta.Delta.Type == "text_delta" {
					sb.WriteString(delta.Delta.Text)
				}
			}
		case ir.ProtocolOpenAIResponses:
			if currentEvent == "response.output_text.delta" {
				var delta struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &delta); err == nil {
					sb.WriteString(delta.Delta)
				}
			}
		}
	}
	return sb.String()
}

// setupConformanceGateway spins up a Gin test router wired with gatewayV2Middleware.
func setupConformanceGateway(t *testing.T, providers []customProvider, store *gatewayprovider.Store) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	prefs := &modelPrefs{
		providers:     providers,
		providerStore: store,
	}
	dispatcher, err := gatewayV2Middleware(prefs, true, "")
	if err != nil {
		t.Fatalf("gatewayV2Middleware: %v", err)
	}
	engine := gin.New()
	engine.Use(gatewayRequestContext(), dispatcher)
	return engine
}

// createMockUpstreamServer constructs an httptest.Server configured to simulate an upstream speaking the requested protocol.
func createMockUpstreamServer(
	t *testing.T,
	proto ir.Protocol,
	turn1Calls []parsedToolCall,
	fragmentArguments bool,
	expectedResults []toolResultPayload,
	finalAnswer string,
) *httptest.Server {
	t.Helper()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("mock upstream read error: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if encodeErr := json.NewEncoder(w).Encode(v); encodeErr != nil {
				t.Errorf("writeJSON error: %v", encodeErr)
			}
		}

		writeSSE := func(text string) {
			if _, writeErr := io.WriteString(w, text); writeErr != nil {
				t.Errorf("writeSSE error: %v", writeErr)
			}
		}

		isStreaming := strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`)

		// Detect if this is Turn 1 (request with tools) or Turn 2 (request with tool result)
		isTurn2 := false
		switch proto {
		case ir.ProtocolOpenAIChat:
			if strings.Contains(string(body), `"role":"tool"`) || strings.Contains(string(body), `"role": "tool"`) {
				isTurn2 = true
			}
		case ir.ProtocolAnthropicMessages:
			if strings.Contains(string(body), `"tool_result"`) {
				isTurn2 = true
			}
		case ir.ProtocolOpenAIResponses:
			if strings.Contains(string(body), `"function_call_output"`) {
				isTurn2 = true
			}
		}

		if isTurn2 {
			// Verify expected tool IDs and result contents arrived at upstream
			for _, exp := range expectedResults {
				if !strings.Contains(string(body), exp.ToolUseID) {
					t.Errorf("upstream %s did not receive expected tool_use_id %q in Turn 2 body: %s", proto, exp.ToolUseID, string(body))
				}
				if exp.IsError && proto == ir.ProtocolAnthropicMessages {
					if !strings.Contains(string(body), `"is_error":true`) && !strings.Contains(string(body), `"is_error": true`) {
						t.Errorf("upstream Anthropic did not receive is_error in Turn 2 body: %s", string(body))
					}
				}
				if exp.ImageB64 != "" {
					if !strings.Contains(string(body), exp.ImageB64) {
						t.Errorf("upstream %s did not receive expected base64 image in Turn 2 body", proto)
					}
				}
			}

			// Respond with final answer
			if !isStreaming {
				switch proto {
				case ir.ProtocolOpenAIChat:
					writeJSON(map[string]any{
						"id":      "chatcmpl-turn2",
						"object":  "chat.completion",
						"created": 1700000001,
						"model":   "upstream-model",
						"choices": []map[string]any{
							{
								"index":         0,
								"message":       map[string]any{"role": "assistant", "content": finalAnswer},
								"finish_reason": "stop",
							},
						},
						"usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
					})
				case ir.ProtocolAnthropicMessages:
					writeJSON(map[string]any{
						"id":          "msg-turn2",
						"type":        "message",
						"role":        "assistant",
						"model":       "upstream-model",
						"content":     []map[string]any{{"type": "text", "text": finalAnswer}},
						"stop_reason": "end_turn",
						"usage":       map[string]int{"input_tokens": 20, "output_tokens": 10},
					})
				case ir.ProtocolOpenAIResponses:
					writeJSON(map[string]any{
						"id":     "resp-turn2",
						"object": "response",
						"status": "completed",
						"model":  "upstream-model",
						"output": []map[string]any{
							{
								"type": "message",
								"role": "assistant",
								"content": []map[string]any{
									{"type": "output_text", "text": finalAnswer},
								},
							},
						},
						"usage": map[string]int{"input_tokens": 20, "output_tokens": 10},
					})
				}
				return
			}

			// Streaming final answer
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			switch proto {
			case ir.ProtocolOpenAIChat:
				writeSSE("data: " + `{"id":"chatcmpl-stream-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"` + finalAnswer + `"},"finish_reason":null}]}` + "\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				writeSSE("data: " + `{"id":"chatcmpl-stream-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
				writeSSE("data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			case ir.ProtocolAnthropicMessages:
				writeSSE("event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-stream-2","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":20}}}` + "\n\n")
				writeSSE("event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
				writeSSE("event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + finalAnswer + `"}}` + "\n\n")
				writeSSE("event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n")
				writeSSE("event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}` + "\n\n")
				writeSSE("event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			case ir.ProtocolOpenAIResponses:
				writeSSE("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp-stream-2","model":"upstream-model","status":"in_progress"}}` + "\n\n")
				writeSSE("event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","role":"assistant","content":[]}}` + "\n\n")
				writeSSE("event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"delta":"` + finalAnswer + `"}` + "\n\n")
				writeSSE("event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_0","type":"message","role":"assistant","content":[{"type":"output_text","text":"` + finalAnswer + `"}]}}` + "\n\n")
				writeSSE("event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp-stream-2","status":"completed","usage":{"input_tokens":20,"output_tokens":10}}}` + "\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}

		// Turn 1: Respond with tool calls
		if !isStreaming {
			switch proto {
			case ir.ProtocolOpenAIChat:
				var tcList []map[string]any
				for _, c := range turn1Calls {
					tcList = append(tcList, map[string]any{
						"id":   c.ID,
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": c.Arguments,
						},
					})
				}
				writeJSON(map[string]any{
					"id":      "chatcmpl-turn1",
					"object":  "chat.completion",
					"created": 1700000000,
					"model":   "upstream-model",
					"choices": []map[string]any{
						{
							"index": 0,
							"message": map[string]any{
								"role":       "assistant",
								"content":    nil,
								"tool_calls": tcList,
							},
							"finish_reason": "tool_calls",
						},
					},
					"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 15, "total_tokens": 25},
				})

			case ir.ProtocolAnthropicMessages:
				var content []map[string]any
				for _, c := range turn1Calls {
					var parsedArgs any
					if unmarshalErr := json.Unmarshal([]byte(c.Arguments), &parsedArgs); unmarshalErr != nil {
						parsedArgs = map[string]any{}
					}
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    c.ID,
						"name":  c.Name,
						"input": parsedArgs,
					})
				}
				writeJSON(map[string]any{
					"id":          "msg-turn1",
					"type":        "message",
					"role":        "assistant",
					"model":       "upstream-model",
					"content":     content,
					"stop_reason": "tool_use",
					"usage":       map[string]int{"input_tokens": 10, "output_tokens": 15},
				})

			case ir.ProtocolOpenAIResponses:
				var output []map[string]any
				for _, c := range turn1Calls {
					output = append(output, map[string]any{
						"type":      "function_call",
						"id":        "fc_" + c.ID,
						"call_id":   c.ID,
						"name":      c.Name,
						"arguments": c.Arguments,
					})
				}
				writeJSON(map[string]any{
					"id":     "resp-turn1",
					"object": "response",
					"status": "completed",
					"model":  "upstream-model",
					"output": output,
					"usage":  map[string]int{"input_tokens": 10, "output_tokens": 15},
				})
			}
			return
		}

		// Turn 1 Streaming:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		splitArg := func(arg string) []string {
			if !fragmentArguments || len(arg) < 4 {
				return []string{arg}
			}
			mid := len(arg) / 2
			return []string{arg[:mid], arg[mid:]}
		}

		switch proto {
		case ir.ProtocolOpenAIChat:
			for i, c := range turn1Calls {
				// Initial call announcement chunk
				writeSSE(fmt.Sprintf("data: "+`{"id":"chatcmpl-stream-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":%d,"id":"%s","type":"function","function":{"name":"%s","arguments":""}}]},"finish_reason":null}]}`+"\n\n", i, c.ID, c.Name))
				if flusher != nil {
					flusher.Flush()
				}
				// Argument chunks
				chunks := splitArg(c.Arguments)
				for _, chunk := range chunks {
					escaped, marshalErr := json.Marshal(chunk)
					if marshalErr != nil {
						t.Errorf("marshal chunk: %v", marshalErr)
					}
					writeSSE(fmt.Sprintf("data: "+`{"id":"chatcmpl-stream-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]},"finish_reason":null}]}`+"\n\n", i, string(escaped)))
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
			writeSSE("data: " + `{"id":"chatcmpl-stream-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
			writeSSE("data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}

		case ir.ProtocolAnthropicMessages:
			writeSSE("event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-stream-1","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":10}}}` + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			for i, c := range turn1Calls {
				writeSSE(fmt.Sprintf("event: content_block_start\ndata: "+`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`+"\n\n", i, c.ID, c.Name))
				if flusher != nil {
					flusher.Flush()
				}
				chunks := splitArg(c.Arguments)
				for _, chunk := range chunks {
					escaped, marshalErr := json.Marshal(chunk)
					if marshalErr != nil {
						t.Errorf("marshal chunk: %v", marshalErr)
					}
					writeSSE(fmt.Sprintf("event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`+"\n\n", i, string(escaped)))
					if flusher != nil {
						flusher.Flush()
					}
				}
				writeSSE(fmt.Sprintf("event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":%d}`+"\n\n", i))
				if flusher != nil {
					flusher.Flush()
				}
			}
			writeSSE("event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}` + "\n\n")
			writeSSE("event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}

		case ir.ProtocolOpenAIResponses:
			writeSSE("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp-stream-1","model":"upstream-model","status":"in_progress"}}` + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			for i, c := range turn1Calls {
				writeSSE(fmt.Sprintf("event: response.output_item.added\ndata: "+`{"type":"response.output_item.added","output_index":%d,"item":{"id":"fc_%d","type":"function_call","call_id":"%s","name":"%s"}}`+"\n\n", i, i, c.ID, c.Name))
				if flusher != nil {
					flusher.Flush()
				}
				chunks := splitArg(c.Arguments)
				for _, chunk := range chunks {
					escaped, marshalErr := json.Marshal(chunk)
					if marshalErr != nil {
						t.Errorf("marshal chunk: %v", marshalErr)
					}
					writeSSE(fmt.Sprintf("event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":%d,"delta":%s}`+"\n\n", i, string(escaped)))
					if flusher != nil {
						flusher.Flush()
					}
				}
				writeSSE(fmt.Sprintf("event: response.function_call_arguments.done\ndata: "+`{"type":"response.function_call_arguments.done","output_index":%d}`+"\n\n", i))
				writeSSE(fmt.Sprintf("event: response.output_item.done\ndata: "+`{"type":"response.output_item.done","output_index":%d,"item":{"id":"fc_%d","type":"function_call","call_id":"%s","name":"%s","arguments":"%s"}}`+"\n\n", i, i, c.ID, c.Name, c.Arguments))
				if flusher != nil {
					flusher.Flush()
				}
			}
			writeSSE("event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp-stream-1","status":"completed","usage":{"input_tokens":10,"output_tokens":15}}}` + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	return httptest.NewServer(handler)
}

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
