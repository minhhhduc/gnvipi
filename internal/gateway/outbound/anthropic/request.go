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

	var wireTools []wireTool
	if len(request.Tools) > 0 {
		wireTools = make([]wireTool, 0, len(request.Tools))
		for _, tool := range request.Tools {
			if tool.Name == "" {
				return nil, invalidRequest("tool name is required")
			}
			schema := tool.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			wireTools = append(wireTools, wireTool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			})
		}
	}
	var wireChoice any
	// request.ResponseFormat != nil is included alongside wireTools because the
	// emulated-tool structured output path synthesizes a tool_choice.
	if request.ToolChoice != nil || (request.ParallelToolCalls != nil && (len(wireTools) > 0 || request.ResponseFormat != nil)) {
		tc, err := encodeToolChoice(request.ToolChoice, request.ParallelToolCalls)
		if err != nil {
			return nil, err
		}
		wireChoice = tc
	}
	var emulatedTools []any
	var emulatedToolChoice any
	if request.ResponseFormat != nil {
		switch request.ResponseFormat.Type {
		case ir.ResponseFormatText:
			// Plain text requires no emulation.
		case ir.ResponseFormatJSONSchema:
			if request.ResponseFormat.JSONSchema == nil {
				return nil, invalidRequest("JSON schema format requires a schema")
			}
			schema := request.ResponseFormat.JSONSchema.Schema
			if len(bytes.TrimSpace(schema)) == 0 || !gatewayoutbound.JSONObject(schema) {
				return nil, invalidRequest("JSON schema format requires an object schema")
			}
			if a.ToolEmulation {
				toolName := request.ResponseFormat.JSONSchema.Name
				if toolName == "" {
					toolName = "structured_output"
				}
				tool := map[string]any{
					"name":         toolName,
					"input_schema": schema,
				}
				emulatedTools = []any{tool}
				emulatedToolChoice = map[string]any{
					"type": "tool",
					"name": toolName,
				}
			} else if !a.JSONModePrompt {
				return nil, unsupported("Anthropic structured output is not supported by this adapter")
			}
		case ir.ResponseFormatJSONObject:
			switch {
			case a.ToolEmulation:
				toolName := "json_output"
				tool := map[string]any{
					"name":        toolName,
					"description": "Output valid JSON object",
					"input_schema": map[string]any{
						"type": "object",
					},
				}
				emulatedTools = []any{tool}
				emulatedToolChoice = map[string]any{
					"type": "tool",
					"name": toolName,
				}
			case a.JSONModePrompt:
				// Inject system prompt for JSON mode
			default:
				return nil, unsupported("Anthropic structured output is not supported by this adapter")
			}
		default:
			return nil, invalidRequest(fmt.Sprintf("unsupported response format %q", request.ResponseFormat.Type))
		}
	}
	if request.Sampling.Seed != nil {
		return nil, unsupported("Anthropic Messages does not support the seed sampling option")
	}

	system, err := encodeSystem(request.Instructions)
	if err != nil {
		return nil, err
	}
	if request.ResponseFormat != nil && a.JSONModePrompt && !a.ToolEmulation {
		switch request.ResponseFormat.Type {
		case ir.ResponseFormatJSONObject:
			system = append(system, wireTextBlock{Type: "text", Text: "Respond with valid JSON only."})
		case ir.ResponseFormatJSONSchema:
			prompt := fmt.Sprintf("Respond with valid JSON matching the following schema:\n%s", string(request.ResponseFormat.JSONSchema.Schema))
			system = append(system, wireTextBlock{Type: "text", Text: prompt})
		}
	}
	messages, err := a.encodeMessages(request.Messages)
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
	thinking, err := encodeThinking(request)
	if err != nil {
		return nil, err
	}
	var thinkingBudget int
	switch t := thinking.(type) {
	case wireThinking:
		thinkingBudget = t.BudgetTokens
	case *wireThinking:
		if t != nil {
			thinkingBudget = t.BudgetTokens
		}
	}
	if thinkingBudget > 0 && request.Sampling.MaxOutputTokens == nil && maxTokens <= thinkingBudget {
		maxTokens = thinkingBudget + maxTokens
	}

	fields, err := gatewayoutbound.ExtensionFields(request.Extensions, normalizedExtensionFields)
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
	switch {
	case len(wireTools) > 0:
		fields["tools"] = wireTools
		if wireChoice != nil {
			fields["tool_choice"] = wireChoice
		}
	case len(emulatedTools) > 0:
		fields["tools"] = emulatedTools
		fields["tool_choice"] = emulatedToolChoice
	case wireChoice != nil:
		fields["tool_choice"] = wireChoice
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

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type wireToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type wireToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   any    `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

func encodeToolChoice(tc *ir.ToolChoice, parallelToolCalls *bool) (any, error) {
	if tc == nil {
		if parallelToolCalls != nil && !*parallelToolCalls {
			disabled := true
			return wireToolChoice{Type: "auto", DisableParallelToolUse: &disabled}, nil
		}
		return nil, nil
	}
	disableParallel := tc.DisableParallelToolUse
	if disableParallel == nil && parallelToolCalls != nil {
		d := !*parallelToolCalls
		disableParallel = &d
	}
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		return wireToolChoice{Type: "auto", DisableParallelToolUse: disableParallel}, nil
	case ir.ToolChoiceAny:
		return wireToolChoice{Type: "any", DisableParallelToolUse: disableParallel}, nil
	case ir.ToolChoiceTool:
		if tc.Name == "" {
			return nil, invalidRequest("tool_choice tool mode requires tool name")
		}
		return wireToolChoice{Type: "tool", Name: tc.Name, DisableParallelToolUse: disableParallel}, nil
	case ir.ToolChoiceNone:
		return wireToolChoice{Type: "none", DisableParallelToolUse: disableParallel}, nil
	default:
		return nil, invalidRequest(fmt.Sprintf("unsupported tool choice mode %q", tc.Mode))
	}
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

func (a *Adapter) encodeMessages(input []ir.Message) ([]wireMessage, error) {
	if len(input) == 0 {
		return nil, invalidRequest("at least one message is required")
	}
	if input[0].Role != ir.RoleUser && input[0].Role != ir.RoleTool {
		return nil, unsupported("the first message must have the user role for Anthropic Messages")
	}
	messages := make([]wireMessage, 0, len(input))
	for _, message := range input {
		if message.Role != ir.RoleUser && message.Role != ir.RoleAssistant && message.Role != ir.RoleTool {
			return nil, invalidRequest(fmt.Sprintf("unsupported message role %q", message.Role))
		}
		content := make([]any, 0, len(message.Content))
		for _, block := range message.Content {
			encoded, err := a.encodeContentBlock(message.Role, block)
			if err != nil {
				return nil, err
			}
			content = append(content, encoded)
		}
		role := string(message.Role)
		if message.Role == ir.RoleTool {
			role = string(ir.RoleUser)
		}
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

func (a *Adapter) encodeContentBlock(role ir.Role, block ir.Block) (any, error) {
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
	case ir.BlockToolUse:
		if role != ir.RoleAssistant {
			return nil, invalidRequest("tool_use blocks are only supported in assistant messages")
		}
		if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
			return nil, invalidRequest("tool_use block requires id and name")
		}
		input := block.ToolUse.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		} else if trimmed := bytes.TrimSpace(input); len(trimmed) > 0 && trimmed[0] == '"' {
			var unquoted string
			if err := json.Unmarshal(trimmed, &unquoted); err == nil && json.Valid([]byte(unquoted)) {
				input = json.RawMessage(unquoted)
			}
		}
		if !gatewayoutbound.JSONObject(input) {
			return nil, invalidRequest("tool_use block input must be a JSON object")
		}
		return wireToolUseBlock{
			Type:  "tool_use",
			ID:    block.ToolUse.ID,
			Name:  block.ToolUse.Name,
			Input: input,
		}, nil
	case ir.BlockToolResult:
		if role == ir.RoleAssistant {
			return nil, invalidRequest("tool_result blocks are not supported in assistant messages")
		}
		if block.ToolResult == nil || block.ToolResult.ToolUseID == "" {
			return nil, invalidRequest("tool_result block requires tool_use_id")
		}
		content, err := a.encodeToolResultContent(block.ToolResult.Content)
		if err != nil {
			return nil, err
		}
		return wireToolResultBlock{
			Type:      "tool_result",
			ToolUseID: block.ToolResult.ToolUseID,
			Content:   content,
			IsError:   block.ToolResult.IsError,
		}, nil
	default:
		return nil, unsupported(fmt.Sprintf("Anthropic content block type %q is not supported", block.Type))
	}
}

func (a *Adapter) encodeToolResultContent(blocks []ir.Block) (any, error) {
	if len(blocks) == 0 {
		// Per Anthropic spec allowance, empty content is returned as an empty string.
		return "", nil
	}
	if len(blocks) == 1 && blocks[0].Type == ir.BlockText {
		return blocks[0].Text, nil
	}
	supportsImages := a.Capabilities().Images != adapter.ImageCapabilityNone
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ir.BlockText:
			parts = append(parts, wireTextBlock{Type: "text", Text: block.Text})
		case ir.BlockImage:
			if block.Image == nil {
				return nil, invalidRequest("image block has no image payload")
			}
			if supportsImages {
				img, err := encodeImage(block.Image)
				if err != nil {
					return nil, err
				}
				parts = append(parts, img)
			} else {
				fallback := "[image]"
				if block.Image.URL != "" {
					fallback = fmt.Sprintf("[image: %s]", block.Image.URL)
				}
				parts = append(parts, wireTextBlock{Type: "text", Text: fallback})
			}
		default:
			return nil, invalidRequest(fmt.Sprintf("unsupported block type %q in tool result", block.Type))
		}
	}
	return parts, nil
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

func encodeThinking(request *ir.Request) (any, error) {
	if request == nil {
		return nil, nil
	}
	reasoning := request.Reasoning
	if reasoning == nil && request.ReasoningEffort == "" && request.BudgetTokens == nil {
		return nil, nil
	}
	if reasoning != nil && reasoning.Enabled != nil && !*reasoning.Enabled {
		if reasoning.MaxTokens != nil || request.BudgetTokens != nil {
			return nil, invalidRequest("disabled thinking cannot have a token budget")
		}
		return wireThinking{Type: "disabled"}, nil
	}
	budget := request.GetBudgetTokens()
	if budget != nil {
		if *budget < 1024 {
			return nil, invalidRequest("thinking token budget must be at least 1024")
		}
		return wireThinking{Type: "enabled", BudgetTokens: *budget}, nil
	}
	effort := request.GetReasoningEffort()
	if effort != "" {
		b := ir.EffortToBudget(effort)
		if b < 1024 {
			b = 1024
		}
		return wireThinking{Type: "enabled", BudgetTokens: b}, nil
	}
	if reasoning != nil && reasoning.Enabled != nil && *reasoning.Enabled {
		return wireThinking{Type: "adaptive"}, nil
	}
	return nil, nil
}

func normalizedExtensionFields(name string) bool {
	switch name {
	case "model", "system", "messages", "input", "instructions",
		"max_tokens", "max_output_tokens", "max_completion_tokens",
		"temperature", "top_p", "top_k", "stop", "stop_sequences", "seed",
		"stream", "stream_options", "thinking", "reasoning", "reasoning_effort",
		"tools", "tool_choice", "response_format", "text",
		"metadata", "user", "store", "previous_response_id", "conversation",
		"parallel_tool_calls", "disable_parallel_tool_use", "functions", "function_call":
		return true
	default:
		return false
	}
}
