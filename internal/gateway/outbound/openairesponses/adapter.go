// Package openairesponses converts normalized requests and responses to and
// from the non-streaming OpenAI Responses wire format.
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

// Adapter is stateless and safe to share between requests.
type Adapter struct{}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns a non-streaming OpenAI Responses outbound adapter.
func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIResponses }

func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Images: true, Reasoning: true, StructuredOutput: true}
}

// BuildRequest renders only normalized IR. Raw preservation composites and
// stateful controls never replace the resolved model or normalized content.
func (*Adapter) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	if request == nil {
		return nil, invalid("request is required")
	}
	if ctx == nil {
		return nil, fmt.Errorf("openairesponses: request context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Stream {
		return nil, unsupported("streaming Responses requests require the streaming adapter")
	}
	if len(request.Tools) != 0 || request.ToolChoice != nil {
		return nil, unsupported("Responses tools require the tool-calling adapter")
	}
	if request.Sampling.TopK != nil || request.Sampling.Seed != nil {
		return nil, unsupported("top_k and seed are not supported by the Responses API")
	}

	fields, err := extensionFields(request.Extensions)
	if err != nil {
		return nil, err
	}
	instructions, instructionItems, err := encodeInstructions(request.Instructions)
	if err != nil {
		return nil, err
	}
	input := make([]any, 0, len(instructionItems)+len(request.Messages))
	input = append(input, instructionItems...)
	for index, message := range request.Messages {
		item, err := encodeMessage(message)
		if err != nil {
			return nil, fmt.Errorf("openairesponses: messages[%d]: %w", index, err)
		}
		input = append(input, item)
	}
	fields["input"] = input
	if instructions != nil {
		fields["instructions"] = *instructions
	}
	if value := request.Sampling.MaxOutputTokens; value != nil {
		if *value < 0 {
			return nil, invalid("max_output_tokens must be non-negative")
		}
		fields["max_output_tokens"] = *value
	}
	if request.Sampling.Temperature != nil {
		fields["temperature"] = *request.Sampling.Temperature
	}
	if request.Sampling.TopP != nil {
		fields["top_p"] = *request.Sampling.TopP
	}
	if len(request.Sampling.Stop) != 0 {
		fields["stop"] = request.Sampling.Stop
	}
	if request.Reasoning != nil {
		reasoning, err := encodeReasoning(request.Reasoning)
		if err != nil {
			return nil, err
		}
		if reasoning != nil {
			fields["reasoning"] = reasoning
		}
	}
	if request.ResponseFormat != nil {
		format, err := encodeFormat(request.ResponseFormat)
		if err != nil {
			return nil, err
		}
		fields["text"] = map[string]any{"format": format}
	}
	if request.Metadata != nil {
		fields["user"] = request.Metadata.UserID
	}
	fields["stream"] = false
	fields["store"] = false
	return gatewayoutbound.NewJSONRequest(ctx, target, "/responses", fields)
}

func extensionFields(extensions map[string]json.RawMessage) (map[string]any, error) {
	fields := make(map[string]any, len(extensions)+8)
	for name, value := range extensions {
		if reservedField(name) {
			continue
		}
		if !json.Valid(value) {
			return nil, invalid(fmt.Sprintf("extension %q contains invalid JSON", name))
		}
		fields[name] = value
	}
	return fields, nil
}

func reservedField(name string) bool {
	switch name {
	case "model", "input", "instructions", "stream", "store",
		"previous_response_id", "conversation", "background",
		"max_output_tokens", "temperature", "top_p", "stop",
		"reasoning", "text", "user", "tools", "tool_choice", "parallel_tool_calls":
		return true
	default:
		return false
	}
}

func encodeInstructions(instructions []ir.Instruction) (instruction *string, items []any, err error) {
	if len(instructions) == 0 {
		return nil, nil, nil
	}
	parts := make([][]string, len(instructions))
	role := instructions[0].Role
	mixed := false
	for index, instruction := range instructions {
		if instruction.Role != ir.InstructionSystem && instruction.Role != ir.InstructionDeveloper {
			return nil, nil, invalid(fmt.Sprintf("instructions[%d] has unsupported role %q", index, instruction.Role))
		}
		if len(instruction.Content) == 0 {
			return nil, nil, invalid(fmt.Sprintf("instructions[%d] has no content", index))
		}
		if instruction.Role != role {
			mixed = true
		}
		parts[index] = make([]string, len(instruction.Content))
		for blockIndex, block := range instruction.Content {
			if block.Type != ir.BlockText {
				return nil, nil, invalid(fmt.Sprintf("instructions[%d].content[%d] must be text", index, blockIndex))
			}
			parts[index][blockIndex] = block.Text
		}
	}
	if !mixed {
		count := 0
		for _, group := range parts {
			count += len(group)
		}
		text := make([]string, 0, count)
		for _, group := range parts {
			text = append(text, group...)
		}
		joined := strings.Join(text, "\n\n")
		return &joined, nil, nil
	}
	items = make([]any, 0, len(instructions))
	for index, instruction := range instructions {
		content := make([]any, 0, len(parts[index]))
		for _, text := range parts[index] {
			content = append(content, map[string]any{"type": "input_text", "text": text})
		}
		items = append(items, map[string]any{"role": instruction.Role, "content": content})
	}
	return nil, items, nil
}

func encodeMessage(message ir.Message) (map[string]any, error) {
	if message.Role != ir.RoleUser && message.Role != ir.RoleAssistant {
		if message.Role == ir.RoleTool {
			return nil, unsupported("function call outputs require the tool-calling adapter")
		}
		return nil, invalid(fmt.Sprintf("unsupported role %q", message.Role))
	}
	if len(message.Content) == 0 {
		return nil, invalid("content must not be empty")
	}
	content := make([]any, 0, len(message.Content))
	for index, block := range message.Content {
		switch block.Type {
		case ir.BlockText:
			kind := "input_text"
			if message.Role == ir.RoleAssistant {
				kind = "output_text"
			}
			content = append(content, map[string]any{"type": kind, "text": block.Text})
		case ir.BlockImage:
			if message.Role != ir.RoleUser {
				return nil, invalid(fmt.Sprintf("content[%d] images require the user role", index))
			}
			image, err := encodeImage(block.Image)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			content = append(content, image)
		case ir.BlockToolUse, ir.BlockToolResult:
			return nil, unsupported("function call history requires the tool-calling adapter")
		default:
			return nil, invalid(fmt.Sprintf("content[%d] has unsupported block type %q", index, block.Type))
		}
	}
	return map[string]any{"role": message.Role, "content": content}, nil
}

func encodeImage(image *ir.Image) (map[string]any, error) {
	if image == nil {
		return nil, invalid("image payload is required")
	}
	switch {
	case image.URL != "" && image.Base64 == "":
		return map[string]any{"type": "input_image", "image_url": image.URL}, nil
	case image.URL == "" && image.Base64 != "" && strings.HasPrefix(image.MediaType, "image/"):
		return map[string]any{"type": "input_image", "image_url": "data:" + image.MediaType + ";base64," + image.Base64}, nil
	default:
		return nil, invalid("image must contain either one URL or base64 data with an image media type")
	}
}

func encodeReasoning(reasoning *ir.ReasoningConfig) (map[string]any, error) {
	if reasoning.Enabled != nil || reasoning.MaxTokens != nil {
		return nil, unsupported("Responses reasoning translation supports effort only")
	}
	if reasoning.Effort == "" {
		return nil, nil
	}
	switch reasoning.Effort {
	case ir.ReasoningEffortLow, ir.ReasoningEffortMedium, ir.ReasoningEffortHigh:
		return map[string]any{"effort": reasoning.Effort}, nil
	default:
		return nil, invalid(fmt.Sprintf("unsupported reasoning effort %q", reasoning.Effort))
	}
}

func encodeFormat(format *ir.ResponseFormat) (map[string]any, error) {
	switch format.Type {
	case ir.ResponseFormatText, ir.ResponseFormatJSONObject:
		return map[string]any{"type": format.Type}, nil
	case ir.ResponseFormatJSONSchema:
		if format.JSONSchema == nil || format.JSONSchema.Name == "" || !jsonObject(format.JSONSchema.Schema) {
			return nil, invalid("JSON schema response format requires a name and object schema")
		}
		return map[string]any{
			"type":   format.Type,
			"name":   format.JSONSchema.Name,
			"strict": format.JSONSchema.Strict,
			"schema": format.JSONSchema.Schema,
		}, nil
	default:
		return nil, invalid(fmt.Sprintf("unsupported response format %q", format.Type))
	}
}

// ParseResponse reads but never closes the caller-owned response body.
func (*Adapter) ParseResponse(response *http.Response) (*ir.Response, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("openairesponses: response body is required")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("openairesponses: read response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, gatewayoutbound.UpstreamError(response, body)
	}
	var wire responseWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("openairesponses: response must be one JSON object: %w", err)
	}
	if !absentJSON(wire.Error) {
		return nil, gatewayoutbound.UpstreamError(response, body)
	}
	if wire.ID == "" || wire.Model == "" {
		return nil, fmt.Errorf("openairesponses: response requires id and model")
	}
	if wire.Object != "" && wire.Object != "response" {
		return nil, fmt.Errorf("openairesponses: unexpected response object %q", wire.Object)
	}

	stopReason, err := responseStopReason(wire.Status, wire.IncompleteDetails)
	if err != nil {
		return nil, err
	}
	content, toolCall, err := decodeOutput(wire.Output)
	if err != nil {
		return nil, err
	}
	if stopReason == ir.StopEndTurn && toolCall {
		stopReason = ir.StopToolUse
	}
	return &ir.Response{
		ID:         wire.ID,
		Model:      wire.Model,
		Content:    content,
		StopReason: stopReason,
		Usage: ir.Usage{
			InputTokens:      wire.Usage.InputTokens,
			OutputTokens:     wire.Usage.OutputTokens,
			CacheReadTokens:  wire.Usage.InputTokensDetails.CachedTokens,
			CacheWriteTokens: wire.Usage.InputTokensDetails.CacheWriteTokens,
			ReasoningTokens:  wire.Usage.OutputTokensDetails.ReasoningTokens,
		},
	}, nil
}

type responseWire struct {
	ID                string                     `json:"id"`
	Object            string                     `json:"object"`
	Model             string                     `json:"model"`
	Status            string                     `json:"status"`
	Error             json.RawMessage            `json:"error"`
	IncompleteDetails *responseIncompleteDetails `json:"incomplete_details"`
	Output            []json.RawMessage          `json:"output"`
	Usage             responseUsage              `json:"usage"`
}

type responseIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responseUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func responseStopReason(status string, details *responseIncompleteDetails) (ir.StopReason, error) {
	switch status {
	case "completed":
		return ir.StopEndTurn, nil
	case "incomplete":
		if details == nil {
			return "", fmt.Errorf("openairesponses: incomplete response has no reason")
		}
		switch details.Reason {
		case "max_output_tokens":
			return ir.StopMaxTokens, nil
		case "content_filter":
			return ir.StopContentFilter, nil
		default:
			return "", fmt.Errorf("openairesponses: unsupported incomplete reason %q", details.Reason)
		}
	default:
		return "", fmt.Errorf("openairesponses: response status %q is not complete", status)
	}
}

func decodeOutput(items []json.RawMessage) ([]ir.Block, bool, error) {
	blocks := make([]ir.Block, 0, len(items))
	toolCall := false
	for index, raw := range items {
		var header struct {
			Type string `json:"type"`
		}
		if !jsonObject(raw) || json.Unmarshal(raw, &header) != nil || header.Type == "" {
			return nil, false, fmt.Errorf("openairesponses: output[%d] must be a typed object", index)
		}
		switch header.Type {
		case "message":
			messageBlocks, err := decodeMessageItem(raw, index)
			if err != nil {
				return nil, false, err
			}
			blocks = append(blocks, messageBlocks...)
		case "reasoning":
			block, err := decodeReasoningItem(raw, index)
			if err != nil {
				return nil, false, err
			}
			blocks = append(blocks, block)
		case "function_call":
			block, err := decodeFunctionCall(raw, index)
			if err != nil {
				return nil, false, err
			}
			blocks = append(blocks, block)
			toolCall = true
		default:
			return nil, false, fmt.Errorf("openairesponses: unsupported output item type %q", header.Type)
		}
	}
	return blocks, toolCall, nil
}

func decodeMessageItem(raw json.RawMessage, itemIndex int) ([]ir.Block, error) {
	var item struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("openairesponses: output[%d] message fields are invalid: %w", itemIndex, err)
	}
	if item.Role != "" && item.Role != "assistant" {
		return nil, fmt.Errorf("openairesponses: output[%d] has unexpected message role %q", itemIndex, item.Role)
	}
	var parts []json.RawMessage
	if absentJSON(item.Content) || json.Unmarshal(item.Content, &parts) != nil {
		return nil, fmt.Errorf("openairesponses: output[%d].content must be an array", itemIndex)
	}
	blocks := make([]ir.Block, 0, len(parts))
	for partIndex, rawPart := range parts {
		var part struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if !jsonObject(rawPart) || json.Unmarshal(rawPart, &part) != nil || part.Type != "output_text" || part.Text == nil {
			return nil, fmt.Errorf("openairesponses: output[%d].content[%d] must be output_text with text", itemIndex, partIndex)
		}
		blocks = append(blocks, ir.NewTextBlock(*part.Text))
	}
	return blocks, nil
}

func decodeReasoningItem(raw json.RawMessage, itemIndex int) (ir.Block, error) {
	var item struct {
		Summary []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"summary"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] reasoning fields are invalid: %w", itemIndex, err)
	}
	var summary strings.Builder
	for partIndex, part := range item.Summary {
		if part.Type != "summary_text" || part.Text == nil {
			return ir.Block{}, fmt.Errorf("openairesponses: output[%d].summary[%d] must be summary_text with text", itemIndex, partIndex)
		}
		summary.WriteString(*part.Text)
	}
	return ir.NewReasoningBlock(summary.String(), item.EncryptedContent), nil
}

func decodeFunctionCall(raw json.RawMessage, itemIndex int) (ir.Block, error) {
	var item struct {
		CallID    string  `json:"call_id"`
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.CallID == "" || item.Name == "" || item.Arguments == nil {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] function_call requires call_id, name and arguments", itemIndex)
	}
	arguments := json.RawMessage(*item.Arguments)
	if !jsonObject(arguments) {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] function_call arguments must encode a JSON object", itemIndex)
	}
	return ir.NewToolUseBlock(item.CallID, item.Name, arguments), nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

func absentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func invalid(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: "openairesponses: " + message}
}

func unsupported(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: "openairesponses: " + message}
}

// ParseStream is intentionally unavailable until issue #28. Rejection does
// not read or close the response body, so ownership remains with the caller.
func (*Adapter) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, unsupported("streaming response parsing is not implemented")
}
