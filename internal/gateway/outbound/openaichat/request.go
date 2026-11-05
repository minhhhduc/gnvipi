package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

func (a *Adapter) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	if request == nil {
		return nil, invalid("OpenAI Chat request is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(request.Messages) == 0 {
		return nil, invalid("OpenAI Chat requires at least one message")
	}
	if len(request.Tools) != 0 || request.ToolChoice != nil {
		return nil, unsupported("OpenAI Chat tool definitions and choices are not supported")
	}
	instructionRole := a.InstructionRole
	if instructionRole == "" {
		instructionRole = ir.InstructionSystem
	}
	if instructionRole != ir.InstructionSystem && instructionRole != ir.InstructionDeveloper {
		return nil, invalid(fmt.Sprintf("OpenAI Chat instruction role %q is invalid", instructionRole))
	}
	messages, err := encodeMessages(request, instructionRole)
	if err != nil {
		return nil, err
	}
	fields, err := outbound.ExtensionFields(request.Extensions, normalizedRequestField)
	if err != nil {
		return nil, invalid(err.Error())
	}
	fields["messages"] = messages
	fields["stream"] = request.Stream
	if request.Sampling.Temperature != nil {
		fields["temperature"] = *request.Sampling.Temperature
	}
	if request.Sampling.TopP != nil {
		fields["top_p"] = *request.Sampling.TopP
	}
	if request.Sampling.TopK != nil {
		fields["top_k"] = *request.Sampling.TopK
	}
	if len(request.Sampling.Stop) != 0 {
		fields["stop"] = request.Sampling.Stop
	}
	if request.Sampling.MaxOutputTokens != nil {
		if *request.Sampling.MaxOutputTokens < 0 {
			return nil, invalid("OpenAI Chat max output tokens must be non-negative")
		}
		name := "max_tokens"
		if a.UseMaxCompletionTokens {
			name = "max_completion_tokens"
		}
		fields[name] = *request.Sampling.MaxOutputTokens
	}
	if request.Sampling.Seed != nil {
		fields["seed"] = *request.Sampling.Seed
	}
	if request.StreamIncludeUsage {
		if !request.Stream {
			return nil, invalid("OpenAI Chat stream usage requires stream=true")
		}
		fields["stream_options"] = map[string]any{"include_usage": true}
	}
	if request.ResponseFormat != nil {
		format, err := encodeResponseFormat(request.ResponseFormat)
		if err != nil {
			return nil, err
		}
		fields["response_format"] = format
	}
	if request.Reasoning != nil {
		if request.Reasoning.Effort != "" {
			fields["reasoning_effort"] = request.Reasoning.Effort
		}
		reasoning := make(map[string]any, 2)
		if request.Reasoning.Enabled != nil {
			reasoning["enabled"] = *request.Reasoning.Enabled
		}
		if request.Reasoning.MaxTokens != nil {
			if *request.Reasoning.MaxTokens < 0 {
				return nil, invalid("OpenAI Chat reasoning max_tokens must be non-negative")
			}
			reasoning["max_tokens"] = *request.Reasoning.MaxTokens
		}
		if len(reasoning) != 0 {
			fields["reasoning"] = reasoning
		}
	}
	if request.Metadata != nil {
		fields["user"] = request.Metadata.UserID
	}
	return outbound.NewJSONRequest(ctx, target, "/chat/completions", fields)
}

func normalizedRequestField(name string) bool {
	switch name {
	case "model", "messages", "tools", "tool_choice", "parallel_tool_calls", "temperature", "top_p", "top_k", "stop", "max_tokens", "max_completion_tokens", "seed", "n", "stream", "stream_options", "response_format", "reasoning", "reasoning_effort", "user":
		return true
	default:
		return false
	}
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

func encodeMessages(request *ir.Request, instructionRole ir.InstructionRole) ([]chatMessage, error) {
	messages := make([]chatMessage, 0, len(request.Instructions)+len(request.Messages))
	for i, instruction := range request.Instructions {
		content, err := encodeVisibleContent(instruction.Content, false)
		if err != nil {
			return nil, invalid(fmt.Sprintf("OpenAI Chat instruction %d: %s", i, err))
		}
		if content == nil {
			return nil, invalid(fmt.Sprintf("OpenAI Chat instruction %d has no text content", i))
		}
		messages = append(messages, chatMessage{Role: string(instructionRole), Content: content})
	}
	for i, message := range request.Messages {
		encoded, err := encodeMessage(message)
		if err != nil {
			return nil, invalid(fmt.Sprintf("OpenAI Chat message %d: %s", i, err))
		}
		messages = append(messages, encoded)
	}
	return messages, nil
}

func encodeMessage(message ir.Message) (chatMessage, error) {
	encoded := chatMessage{Role: string(message.Role)}
	switch message.Role {
	case ir.RoleUser:
		content, err := encodeVisibleContent(message.Content, true)
		if err != nil {
			return encoded, err
		}
		if content == nil {
			return encoded, fmt.Errorf("user message has no content")
		}
		encoded.Content = content
		return encoded, nil
	case ir.RoleAssistant:
		visible := make([]ir.Block, 0, len(message.Content))
		var reasoning bytes.Buffer
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockText:
				visible = append(visible, block)
			case ir.BlockReasoning:
				if block.Reasoning == nil {
					return encoded, fmt.Errorf("reasoning block has no payload")
				}
				reasoning.WriteString(block.Reasoning.Content)
			case ir.BlockToolUse:
				if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
					return encoded, fmt.Errorf("tool use requires an ID and name")
				}
				arguments, err := encodeToolArguments(block.ToolUse.Input)
				if err != nil {
					return encoded, err
				}
				encoded.ToolCalls = append(encoded.ToolCalls, chatToolCall{ID: block.ToolUse.ID, Type: "function", Function: chatToolFunction{Name: block.ToolUse.Name, Arguments: arguments}})
			default:
				return encoded, fmt.Errorf("block type %q is not valid assistant content", block.Type)
			}
		}
		content, err := encodeVisibleContent(visible, false)
		if err != nil {
			return encoded, err
		}
		if content == nil && reasoning.Len() == 0 && len(encoded.ToolCalls) == 0 {
			return encoded, fmt.Errorf("assistant message has no content")
		}
		encoded.Content = content
		encoded.ReasoningContent = reasoning.String()
		return encoded, nil
	case ir.RoleTool:
		if len(message.Content) != 1 || message.Content[0].Type != ir.BlockToolResult || message.Content[0].ToolResult == nil {
			return encoded, fmt.Errorf("tool message requires exactly one tool result")
		}
		result := message.Content[0].ToolResult
		if result.ToolUseID == "" {
			return encoded, fmt.Errorf("tool result requires a tool call ID")
		}
		if result.IsError {
			return encoded, fmt.Errorf("failed tool results are not representable without full tool support")
		}
		content, err := encodeVisibleContent(result.Content, false)
		if err != nil {
			return encoded, err
		}
		if content == nil {
			content = ""
		}
		encoded.Content = content
		encoded.ToolCallID = result.ToolUseID
		return encoded, nil
	default:
		return encoded, fmt.Errorf("role %q is unsupported", message.Role)
	}
}

func encodeVisibleContent(blocks []ir.Block, allowImages bool) (any, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	if len(blocks) == 1 && blocks[0].Type == ir.BlockText {
		return blocks[0].Text, nil
	}
	parts := make([]chatContentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ir.BlockText:
			text := block.Text
			parts = append(parts, chatContentPart{Type: "text", Text: &text})
		case ir.BlockImage:
			if !allowImages {
				return nil, fmt.Errorf("image content is not valid for this role")
			}
			if block.Image == nil || (block.Image.URL == "") == (block.Image.Base64 == "") {
				return nil, fmt.Errorf("image requires exactly one URL or base64 payload")
			}
			url := block.Image.URL
			if block.Image.Base64 != "" {
				if block.Image.MediaType == "" {
					return nil, fmt.Errorf("base64 image requires a media type")
				}
				url = "data:" + block.Image.MediaType + ";base64," + block.Image.Base64
			}
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
		default:
			return nil, fmt.Errorf("block type %q is not visible content", block.Type)
		}
	}
	return parts, nil
}

func encodeToolArguments(input json.RawMessage) (string, error) {
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return "{}", nil
	}
	if !json.Valid(input) {
		return "", fmt.Errorf("tool arguments contain invalid JSON")
	}
	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return "", fmt.Errorf("tool arguments: %w", err)
		}
		return text, nil
	}
	return string(input), nil
}

func encodeResponseFormat(format *ir.ResponseFormat) (any, error) {
	switch format.Type {
	case ir.ResponseFormatText, ir.ResponseFormatJSONObject:
		return map[string]any{"type": format.Type}, nil
	case ir.ResponseFormatJSONSchema:
		if format.JSONSchema == nil || format.JSONSchema.Name == "" {
			return nil, invalid("OpenAI Chat JSON schema format requires a name")
		}
		if raw := bytes.TrimSpace(format.JSONSchema.Schema); len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
			return nil, invalid("OpenAI Chat JSON schema must be a JSON object")
		}
		return map[string]any{"type": format.Type, "json_schema": map[string]any{
			"name": format.JSONSchema.Name, "strict": format.JSONSchema.Strict, "schema": format.JSONSchema.Schema,
		}}, nil
	default:
		return nil, invalid(fmt.Sprintf("OpenAI Chat response format %q is unsupported", format.Type))
	}
}
