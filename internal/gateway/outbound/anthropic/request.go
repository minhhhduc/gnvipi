package anthropic

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

// BuildRequest converts normalized IR into one Anthropic Messages request.
func (a *Adapter) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	if request == nil {
		return nil, invalidRequest("request is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(request.Tools) > 0 || request.ToolChoice != nil {
		return nil, unsupported("Anthropic tool requests require the tool adapter")
	}
	if request.ResponseFormat != nil {
		return nil, unsupported("Anthropic structured output is not supported by this adapter")
	}
	if request.Sampling.Seed != nil {
		return nil, unsupported("Anthropic Messages does not support the seed sampling option")
	}

	system, err := encodeSystem(request.Instructions)
	if err != nil {
		return nil, err
	}
	messages, err := encodeMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	maxTokens, err := a.maxTokens(request.Sampling.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	if err := validateSampling(request.Sampling); err != nil {
		return nil, err
	}
	thinking, err := encodeThinking(request.Reasoning)
	if err != nil {
		return nil, err
	}

	fields, err := gatewayoutbound.ExtensionFields(request.Extensions, isReservedField)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}
	fields["messages"] = messages
	fields["max_tokens"] = maxTokens
	if len(system) > 0 {
		fields["system"] = system
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
	if request.Sampling.Stop != nil {
		fields["stop_sequences"] = request.Sampling.Stop
	}
	if thinking != nil {
		fields["thinking"] = thinking
	}
	if request.Stream {
		fields["stream"] = true
	}
	if request.Metadata != nil {
		fields["metadata"] = struct {
			UserID string `json:"user_id"`
		}{UserID: request.Metadata.UserID}
	}
	return gatewayoutbound.NewJSONRequest(ctx, target, "/messages", fields)
}

func (a *Adapter) maxTokens(requested *int) (int, error) {
	if requested != nil {
		if *requested <= 0 {
			return 0, invalidRequest("max_tokens must be positive")
		}
		return *requested, nil
	}
	configured := a.DefaultMaxTokens
	if configured == 0 {
		configured = defaultMaxTokens
	}
	if configured < 0 {
		return 0, invalidRequest("default max_tokens must not be negative")
	}
	return configured, nil
}

type wireMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type wireTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireImageBlock struct {
	Type   string          `json:"type"`
	Source wireImageSource `json:"source"`
}

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type wireThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
}

func encodeSystem(instructions []ir.Instruction) ([]any, error) {
	var blocks []any
	for _, instruction := range instructions {
		for _, block := range instruction.Content {
			if block.Type != ir.BlockText {
				return nil, unsupported("Anthropic system instructions support only text blocks")
			}
			blocks = append(blocks, wireTextBlock{Type: "text", Text: block.Text})
		}
	}
	return blocks, nil
}

func encodeMessages(input []ir.Message) ([]wireMessage, error) {
	if len(input) == 0 {
		return nil, invalidRequest("at least one message is required")
	}
	if input[0].Role != ir.RoleUser {
		return nil, unsupported("the first message must have the user role for Anthropic Messages")
	}
	messages := make([]wireMessage, 0, len(input))
	for _, message := range input {
		if message.Role != ir.RoleUser && message.Role != ir.RoleAssistant {
			if message.Role == ir.RoleTool {
				return nil, unsupported("Anthropic tool result messages require the tool adapter")
			}
			return nil, invalidRequest(fmt.Sprintf("unsupported message role %q", message.Role))
		}
		content := make([]any, 0, len(message.Content))
		for _, block := range message.Content {
			encoded, err := encodeContentBlock(block)
			if err != nil {
				return nil, err
			}
			content = append(content, encoded)
		}
		role := string(message.Role)
		last := len(messages) - 1
		if last >= 0 && messages[last].Role == role {
			messages[last].Content = append(messages[last].Content, content...)
		} else {
			messages = append(messages, wireMessage{Role: role, Content: content})
		}
	}
	for _, message := range messages {
		if len(message.Content) == 0 {
			return nil, invalidRequest("messages must contain at least one content block")
		}
	}
	return messages, nil
}

func encodeContentBlock(block ir.Block) (any, error) {
	switch block.Type {
	case ir.BlockText:
		return wireTextBlock{Type: "text", Text: block.Text}, nil
	case ir.BlockImage:
		if block.Image == nil {
			return nil, invalidRequest("image block has no image payload")
		}
		return encodeImage(block.Image)
	case ir.BlockReasoning:
		if block.Reasoning == nil {
			return nil, invalidRequest("reasoning block has no reasoning payload")
		}
		return wireThinkingBlock{Type: "thinking", Thinking: block.Reasoning.Content, Signature: block.Reasoning.Signature}, nil
	case ir.BlockToolUse, ir.BlockToolResult:
		return nil, unsupported("Anthropic tool content requires the tool adapter")
	default:
		return nil, unsupported(fmt.Sprintf("Anthropic content block type %q is not supported", block.Type))
	}
}

func encodeImage(image *ir.Image) (any, error) {
	if image == nil {
		return nil, invalidRequest("image block has no image payload")
	}
	base64Data := image.Base64Data()
	if strings.HasPrefix(image.URL, "data:") {
		mediaType, data, ok := ir.ParseDataURL(image.URL)
		if !ok || !strings.HasPrefix(mediaType, "image/") || data == "" {
			return nil, invalidRequest("inline image has invalid data URL format")
		}
		if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
			return nil, invalidRequest("image data must be valid base64")
		}
		return wireImageBlock{Type: "image", Source: wireImageSource{
			Type: "base64", MediaType: mediaType, Data: data,
		}}, nil
	}
	if image.URL != "" {
		if base64Data != "" {
			return nil, invalidRequest("image block cannot contain both URL and base64 data")
		}
		parsed, err := url.Parse(image.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, invalidRequest("image URL must be an absolute HTTP or HTTPS URL")
		}
		return wireImageBlock{Type: "image", Source: wireImageSource{Type: "url", URL: image.URL}}, nil
	}
	if base64Data == "" || !strings.HasPrefix(image.MediaType, "image/") {
		return nil, invalidRequest("base64 image requires data and an image media type")
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(base64Data))); err != nil {
		return nil, invalidRequest("image data must be valid base64")
	}
	return wireImageBlock{Type: "image", Source: wireImageSource{
		Type: "base64", MediaType: image.MediaType, Data: base64Data,
	}}, nil
}

func validateSampling(sampling ir.Sampling) error {
	if sampling.Temperature != nil && (*sampling.Temperature < 0 || *sampling.Temperature > 1) {
		return invalidRequest("temperature must be between 0 and 1")
	}
	if sampling.TopP != nil && (*sampling.TopP < 0 || *sampling.TopP > 1) {
		return invalidRequest("top_p must be between 0 and 1")
	}
	if sampling.TopK != nil && *sampling.TopK < 0 {
		return invalidRequest("top_k must not be negative")
	}
	return nil
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

func encodeThinking(reasoning *ir.ReasoningConfig) (any, error) {
	if reasoning == nil {
		return nil, nil
	}
	if reasoning.Effort != "" {
		return nil, unsupported("Anthropic thinking effort is not supported by this adapter")
	}
	if reasoning.Enabled != nil && !*reasoning.Enabled {
		if reasoning.MaxTokens != nil {
			return nil, invalidRequest("disabled thinking cannot have a token budget")
		}
		return wireThinking{Type: "disabled"}, nil
	}
	if reasoning.MaxTokens != nil {
		if *reasoning.MaxTokens < 1024 {
			return nil, invalidRequest("thinking token budget must be at least 1024")
		}
		return wireThinking{Type: "enabled", BudgetTokens: *reasoning.MaxTokens}, nil
	}
	if reasoning.Enabled != nil && *reasoning.Enabled {
		return wireThinking{Type: "adaptive"}, nil
	}
	return nil, nil
}

var normalizedExtensionFields = map[string]struct{}{
	"model": {}, "system": {}, "messages": {}, "input": {}, "instructions": {},
	"max_tokens": {}, "max_output_tokens": {}, "max_completion_tokens": {},
	"temperature": {}, "top_p": {}, "top_k": {}, "stop": {}, "stop_sequences": {}, "seed": {},
	"stream": {}, "stream_options": {}, "thinking": {}, "reasoning": {}, "reasoning_effort": {},
	"tools": {}, "tool_choice": {}, "response_format": {}, "text": {},
	"metadata": {}, "user": {}, "store": {}, "previous_response_id": {}, "conversation": {},
}

func isReservedField(name string) bool {
	_, ok := normalizedExtensionFields[name]
	return ok
}
