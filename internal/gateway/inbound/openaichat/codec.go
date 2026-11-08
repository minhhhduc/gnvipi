// Package openaichat decodes OpenAI Chat Completions requests and encodes
// normalized completions. Streaming is supplied by the embedded StreamEncoder.
package openaichat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Adapter is safe to share between requests. Configure its embedded encoder
// before serving; decoding never changes per-request stream options on it.
// Multiple choices (n > 1) are unsupported by the single-response IR.
type Adapter struct {
	StreamEncoder
}

var _ adapter.Inbound = (*Adapter)(nil)

// New builds a Chat adapter that timestamps each completion or stream separately.
func New() *Adapter {
	return &Adapter{}
}

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIChat }

type chatRequest struct {
	Model               string             `json:"model"`
	Messages            []chatMessage      `json:"messages"`
	Tools               []chatTool         `json:"tools"`
	ToolChoice          json.RawMessage    `json:"tool_choice"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls"`
	Functions           []chatFunction     `json:"functions"`
	FunctionCall        json.RawMessage    `json:"function_call"`
	MaxTokens           *int               `json:"max_tokens"`
	MaxCompletionTokens *int               `json:"max_completion_tokens"`
	Temperature         *float64           `json:"temperature"`
	TopP                *float64           `json:"top_p"`
	Stop                json.RawMessage    `json:"stop"`
	Seed                *int64             `json:"seed"`
	N                   *int               `json:"n"`
	Stream              bool               `json:"stream"`
	StreamOptions       *chatStreamOptions `json:"stream_options"`
	ResponseFormat      *chatFormat        `json:"response_format"`
	ReasoningEffort     *string            `json:"reasoning_effort"`
	User                *string            `json:"user"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        []chatToolCall  `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent *string         `json:"reasoning_content"`
	Refusal          *string         `json:"refusal"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatFormat struct {
	Type       string          `json:"type"`
	JSONSchema *chatJSONSchema `json:"json_schema"`
}

type chatJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

func invalidChat(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func absentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// DecodeRequest retains original wire composites in Extensions: messages keep
// interleaved instructions, content forms, image detail, cache annotations and
// unknown parts; tools/formats/options keep fields outside the current IR.
// These are preservation payloads, not permission to override normalized IR
// fields after routing or transformation. Unknown top-level fields keep their
// original names and raw values. Explicit nulls and stop's wire form survive.
func (*Adapter) DecodeRequest(request *http.Request) (*ir.Request, error) {
	if request == nil || request.Body == nil {
		return nil, invalidChat("a JSON request body is required")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(request.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if cause := request.Context().Err(); cause != nil {
			return nil, cause
		}
		return nil, invalidChat("request body must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if cause := request.Context().Err(); cause != nil {
			return nil, cause
		}
		return nil, invalidChat("request body must contain exactly one JSON object")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	var wire chatRequest
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, invalidChat("request fields have invalid JSON types")
	}
	if err := json.Unmarshal(raw, &extensions); err != nil || extensions == nil {
		return nil, invalidChat("request body must be a JSON object")
	}
	if strings.TrimSpace(wire.Model) == "" {
		return nil, invalidChat("model is required")
	}
	if len(wire.Messages) == 0 {
		return nil, invalidChat("messages must be a non-empty array")
	}
	if wire.N != nil && *wire.N != 1 {
		return nil, invalidChat("n must be 1; multiple choices (n > 1) are unsupported")
	}
	if (wire.MaxTokens != nil && *wire.MaxTokens < 0) || (wire.MaxCompletionTokens != nil && *wire.MaxCompletionTokens < 0) {
		return nil, invalidChat("max_tokens and max_completion_tokens must be non-negative integers")
	}

	result := &ir.Request{
		Model: wire.Model, Messages: make([]ir.Message, 0, len(wire.Messages)),
		Sampling: ir.Sampling{Temperature: wire.Temperature, TopP: wire.TopP, Seed: wire.Seed, MaxOutputTokens: wire.MaxTokens},
		Stream:   wire.Stream, Extensions: extensions,
	}
	if wire.MaxCompletionTokens != nil {
		result.Sampling.MaxOutputTokens = wire.MaxCompletionTokens
	}
	for _, field := range []string{"model", "max_completion_tokens", "temperature", "top_p", "seed", "n", "stream", "reasoning_effort", "user"} {
		if !absentJSON(extensions[field]) {
			delete(extensions, field)
		}
	}
	if wire.MaxCompletionTokens == nil && !absentJSON(extensions["max_tokens"]) {
		delete(extensions, "max_tokens")
	}
	if !absentJSON(wire.Stop) {
		var single string
		if err := json.Unmarshal(wire.Stop, &single); err == nil {
			result.Sampling.Stop = []string{single}
		} else if err := json.Unmarshal(wire.Stop, &result.Sampling.Stop); err != nil {
			return nil, invalidChat("stop must be a string or an array of strings")
		} else {
			var entries []json.RawMessage
			if err := json.Unmarshal(wire.Stop, &entries); err != nil {
				return nil, invalidChat("stop must be a string or an array of strings")
			}
			for _, entry := range entries {
				if absentJSON(entry) {
					return nil, invalidChat("stop entries must be strings")
				}
			}
		}
	}
	if wire.StreamOptions != nil {
		result.StreamIncludeUsage = wire.StreamOptions.IncludeUsage
	}
	if wire.User != nil {
		result.Metadata = &ir.Metadata{UserID: *wire.User}
		if *wire.User == "" {
			extensions["user"] = json.RawMessage(`""`)
		}
	}
	if wire.ReasoningEffort != nil {
		if *wire.ReasoningEffort == "" {
			return nil, invalidChat("reasoning_effort must be a non-empty string")
		}
		result.Reasoning = &ir.ReasoningConfig{Effort: ir.ReasoningEffort(*wire.ReasoningEffort)}
	}
	if wire.ResponseFormat != nil {
		format := wire.ResponseFormat
		result.ResponseFormat = &ir.ResponseFormat{Type: ir.ResponseFormatType(format.Type)}
		switch result.ResponseFormat.Type {
		case ir.ResponseFormatText, ir.ResponseFormatJSONObject:
		case ir.ResponseFormatJSONSchema:
			if format.JSONSchema == nil || format.JSONSchema.Name == "" || !jsonObject(format.JSONSchema.Schema) {
				return nil, invalidChat("response_format.json_schema requires a name and an object schema")
			}
			result.ResponseFormat.JSONSchema = &ir.JSONSchema{Name: format.JSONSchema.Name, Strict: format.JSONSchema.Strict, Schema: format.JSONSchema.Schema}
		default:
			return nil, invalidChat("response_format.type must be text, json_object, or json_schema")
		}
	}
	for i, message := range wire.Messages {
		blocks, err := decodeChatContent(message.Content)
		if err != nil {
			return nil, invalidChat(fmt.Sprintf("messages[%d]: %s", i, err))
		}
		if absentJSON(message.Content) && (message.Role != "assistant" || (len(message.ToolCalls) == 0 && message.ReasoningContent == nil && message.Refusal == nil)) {
			return nil, invalidChat(fmt.Sprintf("messages[%d].content must be a string or an array of parts", i))
		}
		if len(message.ToolCalls) > 0 && message.Role != "assistant" {
			return nil, invalidChat(fmt.Sprintf("messages[%d].tool_calls requires the assistant role", i))
		}
		if message.ReasoningContent != nil {
			if message.Role != "assistant" {
				return nil, invalidChat(fmt.Sprintf("messages[%d].reasoning_content requires the assistant role", i))
			}
			blocks = append([]ir.Block{ir.NewReasoningBlock(*message.ReasoningContent, "")}, blocks...)
		}
		for _, call := range message.ToolCalls {
			if call.Type != "function" {
				// Other tool kinds remain intact in Extensions["messages"].
				continue
			}
			if call.ID == "" || call.Function.Name == "" {
				return nil, invalidChat(fmt.Sprintf("messages[%d].tool_calls requires function names and call IDs", i))
			}
			input := call.Function.Arguments
			if !absentJSON(input) {
				var arguments string
				if err := json.Unmarshal(input, &arguments); err != nil {
					return nil, invalidChat(fmt.Sprintf("messages[%d].tool_calls.function.arguments must be a string", i))
				}
				// Valid object arguments normalize to an IR object. Partial or
				// non-object strings remain JSON strings rather than corrupt IR.
				if jsonObject(json.RawMessage(arguments)) {
					input = json.RawMessage(arguments)
				}
			} else {
				input = nil
			}
			blocks = append(blocks, ir.NewToolUseBlock(call.ID, call.Function.Name, input))
		}
		switch message.Role {
		case "system", "developer":
			result.Instructions = append(result.Instructions, ir.Instruction{Role: ir.InstructionRole(message.Role), Content: blocks})
		case "user", "assistant":
			result.Messages = append(result.Messages, ir.NewMessage(ir.Role(message.Role), blocks...))
		case "tool":
			if message.ToolCallID == "" {
				return nil, invalidChat(fmt.Sprintf("messages[%d].tool_call_id is required", i))
			}
			result.Messages = append(result.Messages, ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock(message.ToolCallID, blocks, false)))
		default:
			return nil, invalidChat(fmt.Sprintf("messages[%d].role is unsupported", i))
		}
	}
	for _, tool := range wire.Tools {
		if tool.Type != "function" {
			continue // Preserve unrepresented tool kinds in Extensions["tools"].
		}
		if tool.Function.Name == "" || (!absentJSON(tool.Function.Parameters) && !jsonObject(tool.Function.Parameters)) {
			return nil, invalidChat("tools.function requires a name and object parameters")
		}
		result.Tools = append(result.Tools, ir.Tool{Name: tool.Function.Name, Description: tool.Function.Description, Schema: tool.Function.Parameters})
	}
	if len(result.Tools) == 0 && len(wire.Functions) > 0 {
		delete(extensions, "functions")
		for _, fn := range wire.Functions {
			if fn.Name == "" || (!absentJSON(fn.Parameters) && !jsonObject(fn.Parameters)) {
				return nil, invalidChat("functions requires a name and object parameters")
			}
			result.Tools = append(result.Tools, ir.Tool{Name: fn.Name, Description: fn.Description, Schema: fn.Parameters})
		}
	}
	if !absentJSON(wire.ToolChoice) {
		var mode string
		if err := json.Unmarshal(wire.ToolChoice, &mode); err == nil {
			result.ToolChoice = &ir.ToolChoice{}
			switch mode {
			case "auto":
				result.ToolChoice.Mode = ir.ToolChoiceAuto
			case "none":
				result.ToolChoice.Mode = ir.ToolChoiceNone
			case "required":
				result.ToolChoice.Mode = ir.ToolChoiceAny
			default:
				return nil, invalidChat("tool_choice must be auto, none, required, or a named function")
			}
		} else {
			var choice chatTool
			if err := json.Unmarshal(wire.ToolChoice, &choice); err != nil || choice.Type == "" {
				return nil, invalidChat("tool_choice must be a string or a tool object")
			}
			if choice.Type == "function" {
				if choice.Function.Name == "" {
					return nil, invalidChat("tool_choice.function.name is required")
				}
				result.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: choice.Function.Name}
			}
		}
	} else if !absentJSON(wire.FunctionCall) {
		delete(extensions, "function_call")
		var mode string
		if err := json.Unmarshal(wire.FunctionCall, &mode); err == nil {
			result.ToolChoice = &ir.ToolChoice{}
			switch mode {
			case "auto":
				result.ToolChoice.Mode = ir.ToolChoiceAuto
			case "none":
				result.ToolChoice.Mode = ir.ToolChoiceNone
			default:
				return nil, invalidChat("function_call must be auto, none, or a named function")
			}
		} else {
			var choice struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(wire.FunctionCall, &choice); err != nil || choice.Name == "" {
				return nil, invalidChat("function_call must be a string or a function object")
			}
			result.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: choice.Name}
		}
	}
	if wire.ParallelToolCalls != nil {
		result.ParallelToolCalls = wire.ParallelToolCalls
		if result.ToolChoice == nil && absentJSON(wire.ToolChoice) && absentJSON(wire.FunctionCall) {
			result.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
		}
		if result.ToolChoice != nil {
			disabled := !*wire.ParallelToolCalls
			result.ToolChoice.DisableParallelToolUse = &disabled
		}
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func jsonObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

func decodeChatContent(raw json.RawMessage) ([]ir.Block, error) {
	if absentJSON(raw) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []ir.Block{ir.NewTextBlock(text)}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	blocks := make([]ir.Block, 0, len(parts))
	for _, rawPart := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(rawPart, &part); err != nil || part == nil {
			return nil, fmt.Errorf("content parts must be objects")
		}
		var kind string
		if err := json.Unmarshal(part["type"], &kind); err != nil || kind == "" {
			return nil, fmt.Errorf("content parts require a string type")
		}
		switch kind {
		case "text":
			var text string
			if err := json.Unmarshal(part["text"], &text); err != nil || absentJSON(part["text"]) {
				return nil, fmt.Errorf("text parts require a string text field")
			}
			blocks = append(blocks, ir.NewTextBlock(text))
		case "image_url":
			var imageURL struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(part["image_url"], &imageURL); err != nil || imageURL.URL == "" {
				return nil, fmt.Errorf("image_url parts require a non-empty URL")
			}
			url := imageURL.URL
			if strings.HasPrefix(url, "data:") {
				mediaType, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,")
				if !ok || !strings.HasPrefix(mediaType, "image/") || data == "" {
					return nil, fmt.Errorf("inline images require an image media type and base64 data URL")
				}
				blocks = append(blocks, ir.NewImageBlockBase64(mediaType, data))
			} else {
				blocks = append(blocks, ir.NewImageBlockURL(url))
			}
		default:
			// Unknown content parts are retained in Extensions["messages"].
		}
	}
	return blocks, nil
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatChoice struct {
	Index        int                 `json:"index"`
	Message      chatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type chatResponseMessage struct {
	Role             string                 `json:"role"`
	Content          *string                `json:"content"`
	ReasoningContent *string                `json:"reasoning_content,omitempty"`
	ToolCalls        []chatResponseToolCall `json:"tool_calls,omitempty"`
}

type chatResponseToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens            int                    `json:"prompt_tokens"`
	CompletionTokens        int                    `json:"completion_tokens"`
	TotalTokens             int                    `json:"total_tokens"`
	PromptTokensDetails     *chatPromptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *chatCompletionDetails `json:"completion_tokens_details,omitempty"`
}

type chatPromptDetails struct {
	CachedTokens        int `json:"cached_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type chatCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// EncodeResponse emits resp.Model verbatim: the dispatcher must restore the
// client alias before encoding; a shared adapter must not remember request models.
// StopError has no successful completion representation and must use gwerr.
func (a *Adapter) EncodeResponse(response *ir.Response) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("openaichat: response is required")
	}
	var finish string
	switch response.StopReason {
	case ir.StopEndTurn, ir.StopSequence:
		finish = "stop"
	case ir.StopMaxTokens:
		finish = "length"
	case ir.StopToolUse:
		finish = "tool_calls"
	case ir.StopContentFilter:
		finish = "content_filter"
	default:
		return nil, fmt.Errorf("openaichat: stop reason %q cannot encode a successful completion", response.StopReason)
	}
	message := chatResponseMessage{Role: "assistant"}
	var text, reasoning strings.Builder
	hasText, hasReasoning := false, false
	for _, block := range response.Content {
		switch block.Type {
		case ir.BlockText:
			hasText = true
			text.WriteString(block.Text)
		case ir.BlockReasoning:
			if block.Reasoning == nil {
				return nil, fmt.Errorf("openaichat: reasoning block is missing its payload")
			}
			if !a.DisableReasoning {
				hasReasoning = true
				reasoning.WriteString(block.Reasoning.Content)
			}
		case ir.BlockToolUse:
			if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
				return nil, fmt.Errorf("openaichat: tool use requires its payload, ID and name")
			}
			call := chatResponseToolCall{ID: block.ToolUse.ID, Type: "function"}
			call.Function.Name = block.ToolUse.Name
			call.Function.Arguments = "{}"
			if !absentJSON(block.ToolUse.Input) {
				if !json.Valid(block.ToolUse.Input) {
					return nil, fmt.Errorf("openaichat: tool arguments contain invalid JSON")
				}
				if err := json.Unmarshal(block.ToolUse.Input, &call.Function.Arguments); err != nil {
					call.Function.Arguments = string(block.ToolUse.Input)
				}
			}
			message.ToolCalls = append(message.ToolCalls, call)
		default:
			return nil, fmt.Errorf("openaichat: response block type %q is not representable in a chat completion", block.Type)
		}
	}
	if hasText {
		value := text.String()
		message.Content = &value
	}
	if hasReasoning {
		value := reasoning.String()
		message.ReasoningContent = &value
	}
	usage := chatUsage{PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens,
		TotalTokens: response.Usage.InputTokens + response.Usage.OutputTokens}
	if response.Usage.CacheReadTokens != 0 || response.Usage.CacheWriteTokens != 0 {
		usage.PromptTokensDetails = &chatPromptDetails{CachedTokens: response.Usage.CacheReadTokens, CacheCreationTokens: response.Usage.CacheWriteTokens}
	}
	if response.Usage.ReasoningTokens != 0 {
		usage.CompletionTokensDetails = &chatCompletionDetails{ReasoningTokens: response.Usage.ReasoningTokens}
	}
	created := a.Created
	if created <= 0 {
		created = time.Now().Unix()
	}
	return json.Marshal(chatResponse{ID: response.ID, Object: "chat.completion", Created: created, Model: response.Model,
		Choices: []chatChoice{{Index: 0, Message: message, FinishReason: finish}}, Usage: usage})
}
