package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

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
