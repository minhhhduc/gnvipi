package openaichat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   chatCompletionUsage    `json:"usage"`
	Error   json.RawMessage        `json:"error"`
}

type chatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      chatCompletionMessage `json:"message"`
	FinishReason *string               `json:"finish_reason"`
}

type chatCompletionMessage struct {
	Role             string                   `json:"role"`
	Content          json.RawMessage          `json:"content"`
	ReasoningContent *string                  `json:"reasoning_content"`
	Reasoning        *string                  `json:"reasoning"`
	ToolCalls        []chatCompletionToolCall `json:"tool_calls"`
}

type chatCompletionToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatCompletionUsage struct {
	PromptTokens            int                    `json:"prompt_tokens"`
	CompletionTokens        int                    `json:"completion_tokens"`
	PromptTokensDetails     *chatPromptDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *chatCompletionDetails `json:"completion_tokens_details"`
}

type chatPromptDetails struct {
	CachedTokens        int `json:"cached_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

type chatCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (*Adapter) ParseResponse(response *http.Response) (*ir.Response, error) {
	if response == nil || response.Body == nil {
		return nil, &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "OpenAI Chat upstream response body is required"}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		gatewayError := outbound.UpstreamError(response, nil)
		gatewayError.Kind = gwerr.KindFromErr(err)
		gatewayError.Message = fmt.Sprintf("read OpenAI Chat upstream response: %v", err)
		return nil, gatewayError
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, outbound.UpstreamError(response, body)
	}
	var wire chatCompletion
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, malformedResponse(response, fmt.Sprintf("OpenAI Chat upstream returned invalid JSON: %v", err))
	}
	if !absentRaw(wire.Error) {
		return nil, outbound.UpstreamError(response, body)
	}
	if wire.ID == "" || wire.Model == "" {
		return nil, malformedResponse(response, "OpenAI Chat upstream response requires id and model")
	}
	if len(wire.Choices) != 1 {
		return nil, malformedResponse(response, "OpenAI Chat upstream response must contain exactly one choice")
	}
	choice := wire.Choices[0]
	if choice.Message.Role != "" && choice.Message.Role != "assistant" {
		return nil, malformedResponse(response, fmt.Sprintf("OpenAI Chat upstream returned message role %q", choice.Message.Role))
	}
	if choice.FinishReason == nil {
		return nil, malformedResponse(response, "OpenAI Chat upstream response has no finish reason")
	}
	stopReason, err := decodeFinishReason(*choice.FinishReason)
	if err != nil {
		return nil, malformedResponse(response, err.Error())
	}
	content, err := decodeResponseContent(choice.Message.Content)
	if err != nil {
		return nil, malformedResponse(response, err.Error())
	}
	if choice.Message.ReasoningContent != nil {
		content = append([]ir.Block{ir.NewReasoningBlock(*choice.Message.ReasoningContent, "")}, content...)
	}
	if choice.Message.Reasoning != nil && (choice.Message.ReasoningContent == nil || *choice.Message.Reasoning != *choice.Message.ReasoningContent) {
		content = append([]ir.Block{ir.NewReasoningBlock(*choice.Message.Reasoning, "")}, content...)
	}
	for i, call := range choice.Message.ToolCalls {
		if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
			return nil, malformedResponse(response, fmt.Sprintf("OpenAI Chat upstream tool call %d is invalid", i))
		}
		var input json.RawMessage
		if call.Function.Arguments != "" {
			raw := json.RawMessage(call.Function.Arguments)
			if json.Valid(raw) {
				input = raw
			} else {
				var err error
				input, err = json.Marshal(call.Function.Arguments)
				if err != nil {
					return nil, malformedResponse(response, fmt.Sprintf("OpenAI Chat upstream tool call %d arguments: %v", i, err))
				}
			}
		}
		content = append(content, ir.NewToolUseBlock(call.ID, call.Function.Name, input))
	}
	usage := ir.Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens}
	if wire.Usage.PromptTokensDetails != nil {
		usage.CacheReadTokens = wire.Usage.PromptTokensDetails.CachedTokens
		usage.CacheWriteTokens = wire.Usage.PromptTokensDetails.CacheCreationTokens
	}
	if wire.Usage.CompletionTokensDetails != nil {
		usage.ReasoningTokens = wire.Usage.CompletionTokensDetails.ReasoningTokens
	}
	return &ir.Response{ID: wire.ID, Model: wire.Model, Content: content, StopReason: stopReason, Usage: usage}, nil
}

func decodeResponseContent(raw json.RawMessage) ([]ir.Block, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []ir.Block{ir.NewTextBlock(text)}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("OpenAI Chat upstream message content must be text, null, or text parts")
	}
	blocks := make([]ir.Block, 0, len(parts))
	for i, part := range parts {
		if part.Type != "text" && part.Type != "output_text" {
			return nil, fmt.Errorf("OpenAI Chat upstream content part %d has unsupported type %q", i, part.Type)
		}
		blocks = append(blocks, ir.NewTextBlock(part.Text))
	}
	return blocks, nil
}

func decodeFinishReason(reason string) (ir.StopReason, error) {
	switch reason {
	case "stop":
		return ir.StopEndTurn, nil
	case "length":
		return ir.StopMaxTokens, nil
	case "tool_calls", "function_call":
		return ir.StopToolUse, nil
	case "content_filter":
		return ir.StopContentFilter, nil
	case "error":
		return ir.StopError, nil
	default:
		return "", fmt.Errorf("OpenAI Chat upstream returned unsupported finish reason %q", reason)
	}
}

func absentRaw(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func malformedResponse(response *http.Response, message string) *gwerr.Error {
	gatewayError := outbound.UpstreamError(response, nil)
	gatewayError.Kind = gwerr.KindUpstreamError
	gatewayError.Message = message
	return gatewayError
}
