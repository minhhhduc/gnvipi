// Package anthropic adapts normalized gateway requests and responses to the
// non-streaming Anthropic Messages JSON protocol.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayoutbound "github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

const defaultMaxTokens = 4096

// Adapter is a stateless non-streaming Anthropic Messages adapter.
// DefaultMaxTokens is used when the IR has no output-token budget. Zero uses
// the protocol default of 4096.
type Adapter struct {
	DefaultMaxTokens int
}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns an Anthropic Messages outbound adapter.
func New() *Adapter { return &Adapter{} }

// Protocol reports the wire protocol implemented by this adapter.
func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolAnthropicMessages }

// Capabilities reports only behavior implemented by this package. Tool request
// conformance and streaming are separate issues even though non-streaming
// responses tolerate the minimal tool_use block representable by the IR.
func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Images: true, Reasoning: true, TopK: true}
}

// BuildRequest converts normalized IR into one Anthropic Messages request.
func (a *Adapter) BuildRequest(ctx context.Context, request *ir.Request, target adapter.Target) (*http.Request, error) {
	if request == nil {
		return nil, invalidRequest("request is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Stream || request.StreamIncludeUsage {
		return nil, unsupported("Anthropic streaming requires the streaming adapter")
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

	fields := extensionFields(request.Extensions)
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
	if image.URL != "" {
		if image.Base64 != "" {
			return nil, invalidRequest("image block cannot contain both URL and base64 data")
		}
		parsed, err := url.Parse(image.URL)
		if err != nil || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, invalidRequest("image URL must be an absolute HTTP or HTTPS URL")
		}
		return wireImageBlock{Type: "image", Source: wireImageSource{Type: "url", URL: image.URL}}, nil
	}
	if image.Base64 == "" || !strings.HasPrefix(image.MediaType, "image/") {
		return nil, invalidRequest("base64 image requires data and an image media type")
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(image.Base64))); err != nil {
		return nil, invalidRequest("image data must be valid base64")
	}
	return wireImageBlock{Type: "image", Source: wireImageSource{
		Type: "base64", MediaType: image.MediaType, Data: image.Base64,
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

func extensionFields(extensions map[string]json.RawMessage) map[string]any {
	fields := make(map[string]any, len(extensions)+8)
	for name, value := range extensions {
		if _, normalized := normalizedExtensionFields[name]; !normalized {
			fields[name] = value
		}
	}
	return fields
}

// ParseResponse converts one complete Anthropic Messages response into IR. It
// reads but never closes the caller-owned response body.
func (*Adapter) ParseResponse(response *http.Response) (*ir.Response, error) {
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
	return parseMessage(response, body)
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

func parseMessage(response *http.Response, body []byte) (*ir.Response, error) {
	var message wireResponse
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, malformedResponse(response, "Anthropic response must be a JSON object")
	}
	if message.Type != "message" || message.Role != "assistant" || message.ID == "" || message.Model == "" {
		return nil, malformedResponse(response, "Anthropic response is missing required message fields")
	}
	content := make([]ir.Block, 0, len(message.Content))
	for _, raw := range message.Content {
		block, err := parseContentBlock(response, raw)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
	}
	stopReason, err := parseStopReason(response, message.StopReason)
	if err != nil {
		return nil, err
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

func parseContentBlock(response *http.Response, raw json.RawMessage) (ir.Block, error) {
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
		if json.Unmarshal(raw, &block) != nil || block.ID == "" || block.Name == "" || !jsonObject(block.Input) {
			return ir.Block{}, malformedResponse(response, "Anthropic tool_use block is invalid")
		}
		return ir.NewToolUseBlock(block.ID, block.Name, bytes.Clone(block.Input)), nil
	default:
		return ir.Block{}, malformedResponse(response, fmt.Sprintf("Anthropic content block type %q is not supported", kind.Type))
	}
}

func jsonObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &object) == nil && object != nil
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

// ParseStream truthfully rejects streaming until the dedicated SSE adapter is
// implemented. Rejection leaves response.Body owned by the caller.
func (*Adapter) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, unsupported("Anthropic response streaming is not supported by this adapter")
}

func invalidRequest(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupported(message string) *gwerr.Error {
	err := invalidRequest(message)
	err.Code = "unsupported_feature"
	return err
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
