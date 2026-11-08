package openairesponses

import (
	"context"
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
	if len(request.Tools) != 0 || request.ToolChoice != nil {
		return nil, unsupported("Responses tools require the tool-calling adapter")
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

func encodeReasoning(reasoning *ir.ReasoningConfig) (map[string]any, error) {
	if reasoning == nil {
		return nil, nil
	}
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
