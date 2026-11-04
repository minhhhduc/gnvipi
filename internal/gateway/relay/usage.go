package relay

import (
	"encoding/json"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Keep the existing statistics scraper's positive json.Number convention,
// including upstreams reporting token counts as strings. Nested message and
// response envelopes carry Anthropic's start usage and Responses' final usage.
// Anthropic deltas omit input tokens, so reported fields merge instead of
// replacing the earlier start event. These are cumulative counts, not sums.
type usageWire struct {
	PromptTokens     json.Number `json:"prompt_tokens"`
	CompletionTokens json.Number `json:"completion_tokens"`
	InputTokens      json.Number `json:"input_tokens"`
	OutputTokens     json.Number `json:"output_tokens"`
	CacheReadTokens  json.Number `json:"cache_read_input_tokens"`
	CacheWriteTokens json.Number `json:"cache_creation_input_tokens"`
	PromptDetails    struct {
		CachedTokens json.Number `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		CachedTokens json.Number `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens json.Number `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputDetails struct {
		ReasoningTokens json.Number `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func scrapeUsageObject(raw []byte, usage ir.Usage) ir.Usage {
	var object struct {
		Usage   *usageWire `json:"usage"`
		Message struct {
			Usage *usageWire `json:"usage"`
		} `json:"message"`
		Response struct {
			Usage *usageWire `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &object) != nil {
		return usage
	}
	for _, wire := range []*usageWire{object.Message.Usage, object.Response.Usage, object.Usage} {
		if wire == nil {
			continue
		}
		for _, field := range []struct {
			number json.Number
			target *int
		}{
			{wire.PromptTokens, &usage.InputTokens}, {wire.InputTokens, &usage.InputTokens},
			{wire.CompletionTokens, &usage.OutputTokens}, {wire.OutputTokens, &usage.OutputTokens},
			{wire.PromptDetails.CachedTokens, &usage.CacheReadTokens}, {wire.InputDetails.CachedTokens, &usage.CacheReadTokens},
			{wire.CacheReadTokens, &usage.CacheReadTokens}, {wire.CacheWriteTokens, &usage.CacheWriteTokens},
			{wire.CompletionDetails.ReasoningTokens, &usage.ReasoningTokens}, {wire.OutputDetails.ReasoningTokens, &usage.ReasoningTokens},
		} {
			if value, err := field.number.Int64(); err == nil && value > 0 && int64(int(value)) == value {
				*field.target = int(value)
			}
		}
	}
	return usage
}
