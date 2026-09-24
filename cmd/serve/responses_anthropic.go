package main

// The Responses API and Anthropic Messages API are deliberately adapted here,
// at the provider-format boundary.  A configured responses_only provider is
// therefore usable by every inbound API, not only by OpenAI Responses clients.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func anthropicResponsesPassthrough(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/messages" {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
			return
		}
		var in anthropicRequest
		if err := json.Unmarshal(body, &in); err != nil || in.Model == "" || len(in.Messages) == 0 {
			if err == nil {
				err = fmt.Errorf("model and messages are required")
			}
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
			return
		}
		prov := lookupCustomProvider(p, in.Model)
		if prov == nil && customModelHidden(p, in.Model) {
			writeAnthropicError(c, http.StatusNotFound, "not_found_error", "model is hidden", "")
			return
		}
		if prov == nil {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Next()
			return
		}
		if !prov.ResponsesOnly {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Next()
			return
		}

		out, err := anthropicToResponses(in, prov.Model)
		if err != nil {
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
			return
		}
		encoded, _ := json.Marshal(out)
		start := time.Now()
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, providerEndpoint(prov.BaseURL, "/responses"), bytes.NewReader(encoded))
		if err != nil {
			statFail(in.Model, start)
			writeAnthropicError(c, http.StatusInternalServerError, "api_error", err.Error(), "")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
		forwardOpenCodeHeaders(req, c.Request)
		resp, err := compatClient.Do(req)
		if err != nil {
			statFail(in.Model, start)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "upstream: "+err.Error(), "")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			up, _ := io.ReadAll(resp.Body)
			nvidiaRecord(in.Model, up, start, false, true)
			writeAnthropicUpstreamError(c, resp.StatusCode, up, resp.Header.Get("x-request-id"), prov.APIKey)
			return
		}
		if in.Stream {
			streamResponsesAsAnthropic(c, resp.Body, in.Model, start)
		} else {
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				statFail(in.Model, start)
				writeAnthropicError(c, http.StatusBadGateway, "api_error", "upstream read: "+err.Error(), "")
				return
			}
			var upstream map[string]any
			if err := json.Unmarshal(raw, &upstream); err != nil {
				writeAnthropicError(c, http.StatusBadGateway, "api_error", "invalid upstream Responses payload", "")
				return
			}
			result := responsesToAnthropic(upstream, in.Model)
			c.JSON(http.StatusOK, result)
			nvidiaRecord(in.Model, raw, start, false, false)
		}
		c.Abort()
	}
}

// Kept small and JSON-shaped because OpenAI adds fields frequently. Unknown
// fields are intentionally ignored, while supported Anthropic fields map 1:1.
type anthropicRequest struct {
	Model       string             `json:"model"`
	System      any                `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature any                `json:"temperature"`
	TopP        any                `json:"top_p"`
	Stop        []string           `json:"stop_sequences"`
	Tools       []map[string]any   `json:"tools"`
	ToolChoice  any                `json:"tool_choice"`
	Stream      bool               `json:"stream"`
}
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func anthropicToResponses(in anthropicRequest, model string) (map[string]any, error) {
	input := make([]any, 0, len(in.Messages)+1)
	if in.System != nil {
		blocks, err := anthropicContent(in.System, "system")
		if err != nil {
			return nil, err
		}
		input = append(input, map[string]any{"role": "system", "content": blocks})
	}
	for _, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
		items, err := anthropicContent(m.Content, m.Role)
		if err != nil {
			return nil, err
		}
		// Function calls and their outputs are top-level Responses input items.
		var content []any
		for _, item := range items {
			if v, ok := item.(map[string]any); ok && (v["type"] == "function_call" || v["type"] == "function_call_output") {
				input = append(input, v)
			} else {
				content = append(content, item)
			}
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"role": m.Role, "content": content})
		}
	}
	out := map[string]any{"model": model, "input": input, "stream": in.Stream}
	if in.MaxTokens > 0 {
		out["max_output_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = in.TopP
	}
	if len(in.Stop) > 0 {
		out["stop"] = in.Stop
	}
	if len(in.Tools) > 0 {
		tools := make([]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]})
		}
		out["tools"] = tools
	}
	if in.ToolChoice != nil {
		out["tool_choice"] = responseToolChoice(in.ToolChoice)
	}
	return out, nil
}

func anthropicContent(content any, role string) ([]any, error) {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": inputTextType(role), "text": s}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be a string or array")
	}
	out := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content block must be an object")
		}
		switch b["type"] {
		case "text":
			out = append(out, map[string]any{"type": inputTextType(role), "text": b["text"]})
		case "image":
			src, _ := b["source"].(map[string]any)
			url, _ := src["url"].(string)
			if url == "" {
				if data, ok := src["data"].(string); ok {
					media, _ := src["media_type"].(string)
					url = "data:" + media + ";base64," + data
				}
			}
			if url != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": url})
			}
		case "tool_use":
			args, _ := json.Marshal(b["input"])
			out = append(out, map[string]any{"type": "function_call", "call_id": b["id"], "name": b["name"], "arguments": string(args)})
		case "tool_result":
			val := b["content"]
			if parts, ok := val.([]any); ok {
				var text strings.Builder
				for _, p := range parts {
					if m, ok := p.(map[string]any); ok && m["type"] == "text" {
						text.WriteString(fmt.Sprint(m["text"]))
					}
				}
				val = text.String()
			}
			out = append(out, map[string]any{"type": "function_call_output", "call_id": b["tool_use_id"], "output": val})
		}
	}
	return out, nil
}
func inputTextType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}
func responseToolChoice(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{"type": "function", "name": m["name"]}
	}
	return v
}

func responsesToAnthropic(r map[string]any, model string) map[string]any {
	content := []any{}
	stop := "end_turn"
	for _, raw := range asSlice(r["output"]) {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "message":
			for _, c := range asSlice(item["content"]) {
				b, _ := c.(map[string]any)
				if b["type"] == "output_text" {
					content = append(content, map[string]any{"type": "text", "text": b["text"]})
				}
			}
		case "function_call":
			content = append(content, map[string]any{"type": "tool_use", "id": item["call_id"], "name": item["name"], "input": jsonObject(item["arguments"])})
			stop = "tool_use"
		}
	}
	if r["status"] == "incomplete" {
		stop = "max_tokens"
	}
	usage := map[string]any{"input_tokens": number(r["usage"], "input_tokens"), "output_tokens": number(r["usage"], "output_tokens")}
	return map[string]any{"id": r["id"], "type": "message", "role": "assistant", "model": firstString(r["model"], model), "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
}
func asSlice(v any) []any { x, _ := v.([]any); return x }
func jsonObject(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var o any
	if json.Unmarshal([]byte(s), &o) == nil {
		return o
	}
	return map[string]any{}
}
func firstString(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
func number(v any, key string) any {
	if m, ok := v.(map[string]any); ok && m[key] != nil {
		return m[key]
	}
	return 0
}

func streamResponsesAsAnthropic(c *gin.Context, body io.Reader, model string, start time.Time) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	emit := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	started, stopped, usedTool := false, false, false
	blocks := map[int]bool{}
	startMessage := func(id string, usage any) {
		if !started {
			emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": number(usage, "input_tokens"), "output_tokens": 0}}})
			started = true
		}
	}
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 4096), 1<<20)
	event, data := "", ""
	for s.Scan() {
		line := s.Text()
		if line == "" {
			streamResponseEvent(event, data, startMessage, emit, blocks, &stopped, &usedTool)
			event, data = "", ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if event != "" {
		streamResponseEvent(event, data, startMessage, emit, blocks, &stopped, &usedTool)
	}
	if !stopped {
		startMessage("", nil)
		stop := "end_turn"
		if usedTool {
			stop = "tool_use"
		}
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	}
	nvidiaRecord(model, nil, start, true, s.Err() != nil)
}

func streamResponseEvent(event, data string, startMessage func(string, any), emit func(string, any), blocks map[int]bool, stopped, usedTool *bool) {
	var d map[string]any
	if data == "[DONE]" || json.Unmarshal([]byte(data), &d) != nil {
		return
	}
	response, _ := d["response"].(map[string]any)
	if response != nil {
		startMessage(firstString(response["id"], ""), response["usage"])
	}
	idx := 0
	if n, ok := d["output_index"].(float64); ok {
		idx = int(n)
	}
	item, _ := d["item"].(map[string]any)
	switch event {
	case "response.created":
		if response != nil {
			startMessage(firstString(response["id"], ""), response["usage"])
		}
	case "response.output_item.added":
		if item == nil {
			return
		}
		typ := item["type"]
		if typ == "message" {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
		} else if typ == "function_call" {
			*usedTool = true
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": item["call_id"], "name": item["name"], "input": map[string]any{}}})
		}
		blocks[idx] = true
	case "response.output_text.delta":
		if blocks[idx] {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": d["delta"]}})
		}
	case "response.function_call_arguments.delta":
		if blocks[idx] {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": d["delta"]}})
		}
	case "response.output_item.done":
		if blocks[idx] {
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			delete(blocks, idx)
		}
	case "response.completed":
		stop := "end_turn"
		if *usedTool {
			stop = "tool_use"
		}
		if response != nil && response["status"] == "incomplete" {
			stop = "max_tokens"
		}
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": number(response["usage"], "output_tokens")}})
		emit("message_stop", map[string]any{"type": "message_stop"})
		*stopped = true
	}
}

func writeAnthropicError(c *gin.Context, status int, typ, message, requestID string) {
	if requestID != "" {
		c.Header("request-id", requestID)
	}
	c.JSON(status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
	c.Abort()
}
func writeAnthropicUpstreamError(c *gin.Context, status int, body []byte, requestID, secret string) {
	providerType := ""
	var x map[string]any
	if json.Unmarshal(body, &x) == nil {
		if e, ok := x["error"].(map[string]any); ok {
			providerType, _ = e["type"].(string)
		}
	}
	if requestID != "" {
		c.Header("request-id", requestID)
	}
	message := upstreamMessage(body)
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	c.JSON(status, map[string]any{"type": "error", "error": map[string]any{
		"type": anthropicErrorType(status), "message": message,
		"upstream_status": status, "provider_error_type": providerType,
	}})
	c.Abort()
}
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	}
	return "api_error"
}
func upstreamMessage(b []byte) string {
	var x map[string]any
	if json.Unmarshal(b, &x) == nil {
		if e, ok := x["error"].(map[string]any); ok {
			if s, ok := e["message"].(string); ok {
				return s
			}
		}
		if s, ok := x["message"].(string); ok {
			return s
		}
	}
	if len(b) > 0 {
		return string(b)
	}
	return "upstream request failed"
}
func nvidiaRecord(model string, raw []byte, start time.Time, stream, failed bool) {
	nvidia.GlobalStats.RecordResponse(model, raw, time.Since(start), stream, failed)
}
