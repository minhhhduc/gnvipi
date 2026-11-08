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
	if len(request.Tools) != 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, t := range request.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Schema) != 0 {
				fn["parameters"] = t.Schema
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		fields["tools"] = tools
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case ir.ToolChoiceAuto:
			fields["tool_choice"] = "auto"
		case ir.ToolChoiceNone:
			fields["tool_choice"] = "none"
		case ir.ToolChoiceAny:
			fields["tool_choice"] = "required"
		case ir.ToolChoiceTool:
			if request.ToolChoice.Name == "" {
				return nil, invalid("OpenAI Chat tool_choice mode tool requires a function name")
			}
			fields["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": request.ToolChoice.Name},
			}
		default:
			return nil, invalid(fmt.Sprintf("OpenAI Chat tool_choice mode %q is unsupported", request.ToolChoice.Mode))
		}
	}
	if request.ParallelToolCalls != nil {
		fields["parallel_tool_calls"] = *request.ParallelToolCalls
	} else if request.ToolChoice != nil && request.ToolChoice.DisableParallelToolUse != nil {
		fields["parallel_tool_calls"] = !*request.ToolChoice.DisableParallelToolUse
	}
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
	effort := request.GetReasoningEffort()
	if effort != "" {
		fields["reasoning_effort"] = string(effort)
	}
	if request.Reasoning != nil {
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
	case "model", "messages", "tools", "tool_choice", "parallel_tool_calls", "functions", "function_call", "temperature", "top_p", "top_k", "stop", "max_tokens", "max_completion_tokens", "seed", "n", "stream", "stream_options", "response_format", "reasoning", "reasoning_effort", "user":
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
	for i := range request.Messages {
		encodedList, err := encodeMessage(&request.Messages[i])
		if err != nil {
			return nil, invalid(fmt.Sprintf("OpenAI Chat message %d: %s", i, err))
		}
		for _, enc := range encodedList {
			if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" && enc.Role == "assistant" {
				prev := &messages[len(messages)-1]
				prev.ToolCalls = append(prev.ToolCalls, enc.ToolCalls...)
				if prev.Content == nil {
					prev.Content = enc.Content
				} else if enc.Content != nil {
					prevStr, ok1 := prev.Content.(string)
					encStr, ok2 := enc.Content.(string)
					if ok1 && ok2 {
						prev.Content = prevStr + "\n" + encStr
					}
				}
				if enc.ReasoningContent != "" {
					if prev.ReasoningContent != "" {
						prev.ReasoningContent += "\n" + enc.ReasoningContent
					} else {
						prev.ReasoningContent = enc.ReasoningContent
					}
				}
			} else {
				messages = append(messages, enc)
			}
		}
	}
	return messages, nil
}

func encodeMessage(message *ir.Message) ([]chatMessage, error) {
	switch message.Role {
	case ir.RoleUser:
		content, err := encodeVisibleContent(message.Content, true)
		if err != nil {
			return nil, err
		}
		if content == nil {
			return nil, fmt.Errorf("user message has no content")
		}
		return []chatMessage{{Role: string(ir.RoleUser), Content: content}}, nil
	case ir.RoleAssistant:
		encoded := chatMessage{Role: string(ir.RoleAssistant)}
		visible := make([]ir.Block, 0, len(message.Content))
		var reasoning bytes.Buffer
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockText:
				visible = append(visible, block)
			case ir.BlockReasoning:
				if block.Reasoning == nil {
					return nil, fmt.Errorf("reasoning block has no payload")
				}
				reasoning.WriteString(block.Reasoning.Content)
			case ir.BlockToolUse:
				if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
					return nil, fmt.Errorf("tool use requires an ID and name")
				}
				arguments := encodeToolArguments(block.ToolUse.Input)
				encoded.ToolCalls = append(encoded.ToolCalls, chatToolCall{ID: block.ToolUse.ID, Type: "function", Function: chatToolFunction{Name: block.ToolUse.Name, Arguments: arguments}})
			default:
				return nil, fmt.Errorf("block type %q is not valid assistant content", block.Type)
			}
		}
		content, err := encodeVisibleContent(visible, false)
		if err != nil {
			return nil, err
		}
		if content == nil && reasoning.Len() == 0 && len(encoded.ToolCalls) == 0 {
			return nil, fmt.Errorf("assistant message has no content")
		}
		encoded.Content = content
		encoded.ReasoningContent = reasoning.String()
		return []chatMessage{encoded}, nil
	case ir.RoleTool:
		if len(message.Content) == 0 {
			return nil, fmt.Errorf("tool message requires at least one tool result")
		}
		out := make([]chatMessage, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Type != ir.BlockToolResult || block.ToolResult == nil {
				return nil, fmt.Errorf("tool message content must be tool results")
			}
			result := block.ToolResult
			if result.ToolUseID == "" {
				return nil, fmt.Errorf("tool result requires a tool call ID")
			}
			content, err := encodeVisibleContent(result.Content, false)
			if err != nil {
				return nil, err
			}
			if content == nil {
				content = ""
			}
			out = append(out, chatMessage{
				Role:       string(ir.RoleTool),
				Content:    content,
				ToolCallID: result.ToolUseID,
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("role %q is unsupported", message.Role)
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
			if block.Image == nil {
				return nil, fmt.Errorf("image requires exactly one URL or base64 payload")
			}
			base64Data := block.Image.Base64Data()
			if (block.Image.URL == "") == (base64Data == "") {
				return nil, fmt.Errorf("image requires exactly one URL or base64 payload")
			}
			url := block.Image.URL
			if base64Data != "" {
				if block.Image.MediaType == "" {
					return nil, fmt.Errorf("base64 image requires a media type")
				}
				url = "data:" + block.Image.MediaType + ";base64," + base64Data
			}
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
		default:
			return nil, fmt.Errorf("block type %q is not visible content", block.Type)
		}
	}
	return parts, nil
}

func encodeToolArguments(input json.RawMessage) string {
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return "{}"
	}
	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err == nil {
			return text
		}
	}
	return string(input)
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
