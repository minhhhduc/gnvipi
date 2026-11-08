package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

// ParseResponse converts one complete Anthropic Messages response into IR. It
// reads but never closes the caller-owned response body.
func (a *Adapter) ParseResponse(response *http.Response) (*ir.Response, error) {
	if response == nil || response.Body == nil {
		return nil, &gwerr.Error{Kind: gwerr.KindInternal, Message: "Anthropic response body is required"}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, responseReadError(response, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, gatewayoutbound.UpstreamError(response, body)
	}
	return a.parseMessage(response, body)
}

type wireResponse struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (a *Adapter) parseMessage(response *http.Response, body []byte) (*ir.Response, error) {
	var message wireResponse
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, malformedResponse(response, "Anthropic response must be a JSON object")
	}
	if message.Type != "message" || message.Role != "assistant" || message.ID == "" || message.Model == "" {
		return nil, malformedResponse(response, "Anthropic response is missing required message fields")
	}
	content := make([]ir.Block, 0, len(message.Content))
	for _, raw := range message.Content {
		block, err := a.parseContentBlock(response, raw)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
	}
	stopReason, err := parseStopReason(response, message.StopReason)
	if err != nil {
		return nil, err
	}
	if a.ToolEmulation && stopReason == ir.StopToolUse {
		stopReason = ir.StopEndTurn
	}
	result := &ir.Response{
		ID: message.ID, Model: message.Model, Content: content, StopReason: stopReason,
		Usage: ir.Usage{
			InputTokens: message.Usage.InputTokens, OutputTokens: message.Usage.OutputTokens,
			CacheReadTokens: message.Usage.CacheReadInputTokens, CacheWriteTokens: message.Usage.CacheCreationInputTokens,
		},
	}
	if stopReason == ir.StopSequence {
		result.StopSequence = message.StopSequence
	}
	return result, nil
}

func (a *Adapter) parseContentBlock(response *http.Response, raw json.RawMessage) (ir.Block, error) {
	var kind struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &kind) != nil || kind.Type == "" {
		return ir.Block{}, malformedResponse(response, "Anthropic response contains an invalid content block")
	}
	switch kind.Type {
	case "text":
		var block struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal(raw, &block) != nil || block.Text == nil {
			return ir.Block{}, malformedResponse(response, "Anthropic text block is missing text")
		}
		return ir.NewTextBlock(*block.Text), nil
	case "thinking":
		var block struct {
			Thinking  *string `json:"thinking"`
			Signature string  `json:"signature"`
		}
		if json.Unmarshal(raw, &block) != nil || block.Thinking == nil {
			return ir.Block{}, malformedResponse(response, "Anthropic thinking block is missing thinking")
		}
		return ir.NewReasoningBlock(*block.Thinking, block.Signature), nil
	case "tool_use":
		var block struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(raw, &block) != nil || block.ID == "" || block.Name == "" || !gatewayoutbound.JSONObject(block.Input) {
			return ir.Block{}, malformedResponse(response, "Anthropic tool_use block is invalid")
		}
		if a.ToolEmulation {
			return ir.NewTextBlock(string(block.Input)), nil
		}
		return ir.NewToolUseBlock(block.ID, block.Name, bytes.Clone(block.Input)), nil
	default:
		return ir.Block{}, malformedResponse(response, fmt.Sprintf("Anthropic content block type %q is not supported", kind.Type))
	}
}

func parseStopReason(response *http.Response, reason string) (ir.StopReason, error) {
	switch reason {
	case "end_turn":
		return ir.StopEndTurn, nil
	case "max_tokens", "model_context_window_exceeded":
		return ir.StopMaxTokens, nil
	case "stop_sequence":
		return ir.StopSequence, nil
	case "tool_use":
		return ir.StopToolUse, nil
	case "refusal":
		return ir.StopContentFilter, nil
	default:
		return "", malformedResponse(response, fmt.Sprintf("Anthropic stop reason %q is not supported", reason))
	}
}

func responseReadError(response *http.Response, err error) *gwerr.Error {
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	return &gwerr.Error{Kind: gwerr.KindFromErr(err), Message: "read Anthropic response: " + err.Error(), UpstreamStatus: status}
}

func malformedResponse(response *http.Response, message string) *gwerr.Error {
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	return &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: message, UpstreamStatus: status}
}
