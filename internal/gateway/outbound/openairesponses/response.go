package openairesponses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

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
		if !gatewayoutbound.JSONObject(raw) || json.Unmarshal(raw, &header) != nil || header.Type == "" {
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
		if !gatewayoutbound.JSONObject(rawPart) || json.Unmarshal(rawPart, &part) != nil || part.Type != "output_text" || part.Text == nil {
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
		ID        string  `json:"id"`
		CallID    string  `json:"call_id"`
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] function_call fields are invalid: %w", itemIndex, err)
	}
	callID := item.CallID
	if callID == "" {
		callID = item.ID
	}
	if callID == "" || item.Name == "" || item.Arguments == nil {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] function_call requires call_id, name and arguments", itemIndex)
	}
	arguments := json.RawMessage(*item.Arguments)
	if !gatewayoutbound.JSONObject(arguments) {
		return ir.Block{}, fmt.Errorf("openairesponses: output[%d] function_call arguments must encode a JSON object", itemIndex)
	}
	return ir.NewToolUseBlock(callID, item.Name, arguments), nil
}

func absentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
