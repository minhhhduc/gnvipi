package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

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
	if request.Sampling.TopK != nil || request.Sampling.Seed != nil {
		return nil, unsupported("top_k and seed are not supported by the Responses API")
	}

	fields, err := gatewayoutbound.ExtensionFields(request.Extensions, reservedField)
	if err != nil {
		return nil, invalid(err.Error())
	}
	instructions, instructionItems, err := encodeInstructions(request.Instructions)
	if err != nil {
		return nil, err
	}
	input := make([]any, 0, len(instructionItems)+len(request.Messages))
	input = append(input, instructionItems...)
	for index := range request.Messages {
		items, err := encodeMessageItems(&request.Messages[index])
		if err != nil {
			return nil, fmt.Errorf("openairesponses: messages[%d]: %w", index, err)
		}
		input = append(input, items...)
	}
	fields["input"] = input
	if instructions != nil {
		fields["instructions"] = *instructions
	}
	if len(request.Tools) != 0 {
		tools, err := encodeTools(request.Tools)
		if err != nil {
			return nil, err
		}
		fields["tools"] = tools
	}
	if request.ToolChoice != nil {
		choice, err := encodeToolChoice(request.ToolChoice)
		if err != nil {
			return nil, err
		}
		if choice != nil {
			fields["tool_choice"] = choice
		}
	}
	if request.ParallelToolCalls != nil {
		fields["parallel_tool_calls"] = *request.ParallelToolCalls
	} else if request.ToolChoice != nil && request.ToolChoice.DisableParallelToolUse != nil {
		fields["parallel_tool_calls"] = !*request.ToolChoice.DisableParallelToolUse
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
	if request.Reasoning != nil || request.ReasoningEffort != "" || request.BudgetTokens != nil {
		reasoning, err := encodeReasoning(request)
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
	fields["stream"] = request.Stream
	fields["store"] = false
	return gatewayoutbound.NewJSONRequest(ctx, target, "/responses", fields)
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

func encodeMessageItems(message *ir.Message) ([]any, error) {
	if message.Role != ir.RoleUser && message.Role != ir.RoleAssistant && message.Role != ir.RoleTool {
		return nil, invalid(fmt.Sprintf("unsupported role %q", message.Role))
	}
	if len(message.Content) == 0 {
		return nil, invalid("content must not be empty")
	}
	var items []any
	content := make([]any, 0, len(message.Content))

	flushContent := func() {
		if len(content) > 0 {
			items = append(items, map[string]any{"role": message.Role, "content": content})
			content = make([]any, 0, len(message.Content))
		}
	}

	for index, block := range message.Content {
		switch block.Type {
		case ir.BlockText:
			if message.Role == ir.RoleTool {
				return nil, invalid(fmt.Sprintf("content[%d] text block invalid for role tool", index))
			}
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
		case ir.BlockReasoning:
			flushContent()
			contentStr := ""
			if block.Reasoning != nil {
				contentStr = block.Reasoning.Content
			}
			items = append(items, map[string]any{
				"type": "reasoning",
				"summary": []map[string]any{
					{"type": "summary_text", "text": contentStr},
				},
			})
		case ir.BlockToolUse:
			if message.Role != ir.RoleAssistant {
				return nil, invalid(fmt.Sprintf("content[%d] tool_use requires assistant role", index))
			}
			flushContent()
			fc, err := encodeToolUse(&block)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			items = append(items, fc)
		case ir.BlockToolResult:
			flushContent()
			fco, err := encodeToolResult(&block)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			items = append(items, fco)
		default:
			return nil, invalid(fmt.Sprintf("content[%d] has unsupported block type %q", index, block.Type))
		}
	}
	flushContent()
	return items, nil
}

func encodeToolUse(block *ir.Block) (map[string]any, error) {
	if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
		return nil, invalid("function_call requires call_id and name")
	}
	arguments := "{}"
	if len(block.ToolUse.Input) > 0 {
		trimmed := bytes.TrimSpace(block.ToolUse.Input)
		if len(trimmed) > 0 {
			if !json.Valid(trimmed) {
				return nil, invalid("function_call arguments contain invalid JSON")
			}
			if trimmed[0] == '"' {
				if err := json.Unmarshal(trimmed, &arguments); err != nil {
					return nil, invalid("function_call arguments string is invalid")
				}
			} else {
				arguments = string(trimmed)
			}
		}
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   block.ToolUse.ID,
		"name":      block.ToolUse.Name,
		"arguments": arguments,
	}, nil
}

func encodeToolResult(block *ir.Block) (map[string]any, error) {
	if block.ToolResult == nil || block.ToolResult.ToolUseID == "" {
		return nil, invalid("function_call_output requires call_id")
	}
	// If Content is empty or consists purely of text blocks, emit a string output.
	// In particular, an empty Content slice cleanly evaluates to output = "" per
	// the OpenAI Responses wire specification for empty tool results.
	allText := true
	for _, b := range block.ToolResult.Content {
		if b.Type != ir.BlockText {
			allText = false
			break
		}
	}
	if allText {
		var sb strings.Builder
		for _, b := range block.ToolResult.Content {
			sb.WriteString(b.Text)
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": block.ToolResult.ToolUseID,
			"output":  sb.String(),
		}, nil
	}
	parts := make([]any, 0, len(block.ToolResult.Content))
	for index, b := range block.ToolResult.Content {
		switch b.Type {
		case ir.BlockText:
			parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
		case ir.BlockImage:
			img, err := encodeImage(b.Image)
			if err != nil {
				return nil, fmt.Errorf("tool_result.content[%d]: %w", index, err)
			}
			parts = append(parts, img)
		default:
			return nil, invalid(fmt.Sprintf("tool_result.content[%d] has unsupported block type %q", index, b.Type))
		}
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": block.ToolResult.ToolUseID,
		"output":  parts,
	}, nil
}

func encodeTools(tools []ir.Tool) ([]any, error) {
	result := make([]any, 0, len(tools))
	for index, tool := range tools {
		if tool.Name == "" {
			return nil, invalid(fmt.Sprintf("tools[%d] requires a name", index))
		}
		item := map[string]any{
			"type": "function",
			"name": tool.Name,
		}
		if tool.Description != "" {
			item["description"] = tool.Description
		}
		if len(tool.Schema) > 0 {
			if !gatewayoutbound.JSONObject(tool.Schema) {
				return nil, invalid(fmt.Sprintf("tools[%d] schema must be a JSON object", index))
			}
			item["parameters"] = tool.Schema
		}
		result = append(result, item)
	}
	return result, nil
}

func encodeToolChoice(choice *ir.ToolChoice) (any, error) {
	if choice == nil {
		return nil, nil
	}
	switch choice.Mode {
	case ir.ToolChoiceAuto:
		return "auto", nil
	case ir.ToolChoiceNone:
		return "none", nil
	case ir.ToolChoiceAny:
		return "required", nil
	case ir.ToolChoiceTool:
		if choice.Name == "" {
			return nil, invalid("named tool_choice requires a tool name")
		}
		return map[string]any{
			"type": "function",
			"name": choice.Name,
		}, nil
	case "":
		if choice.Name != "" {
			return map[string]any{
				"type": "function",
				"name": choice.Name,
			}, nil
		}
		return nil, nil
	default:
		return nil, invalid(fmt.Sprintf("unsupported tool_choice mode %q", choice.Mode))
	}
}

func encodeImage(image *ir.Image) (map[string]any, error) {
	if image == nil {
		return nil, invalid("image payload is required")
	}
	base64Data := image.Base64Data()
	switch {
	case image.URL != "" && base64Data == "":
		return map[string]any{"type": "input_image", "image_url": image.URL}, nil
	case image.URL == "" && base64Data != "" && strings.HasPrefix(image.MediaType, "image/"):
		return map[string]any{"type": "input_image", "image_url": "data:" + image.MediaType + ";base64," + base64Data}, nil
	default:
		return nil, invalid("image must contain either one URL or base64 data with an image media type")
	}
}

func encodeReasoning(request *ir.Request) (map[string]any, error) {
	if request == nil {
		return nil, nil
	}
	reasoning := request.Reasoning
	if reasoning == nil && request.ReasoningEffort == "" && request.BudgetTokens == nil {
		return nil, nil
	}
	if reasoning != nil && reasoning.Enabled != nil && !*reasoning.Enabled {
		return nil, nil
	}
	effort := request.GetReasoningEffort()
	if effort == "" {
		if reasoning != nil && reasoning.Enabled != nil && *reasoning.Enabled {
			effort = ir.ReasoningEffortMedium
		} else {
			return nil, nil
		}
	}
	switch effort {
	case ir.ReasoningEffortLow, ir.ReasoningEffortMedium, ir.ReasoningEffortHigh:
		return map[string]any{"effort": string(effort)}, nil
	default:
		return nil, invalid(fmt.Sprintf("unsupported reasoning effort %q", effort))
	}
}

func encodeFormat(format *ir.ResponseFormat) (map[string]any, error) {
	switch format.Type {
	case ir.ResponseFormatText, ir.ResponseFormatJSONObject:
		return map[string]any{"type": format.Type}, nil
	case ir.ResponseFormatJSONSchema:
		if format.JSONSchema == nil || format.JSONSchema.Name == "" || !gatewayoutbound.JSONObject(format.JSONSchema.Schema) {
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
