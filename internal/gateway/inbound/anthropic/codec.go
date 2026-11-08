package anthropic

import (
	"bytes"
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
)

// Adapter is a stateless Anthropic Messages codec. Streaming is supplied by
// StreamEncoder; no request-specific model or options are stored here.
type Adapter struct {
	StreamEncoder
}

var _ adapter.Inbound = (*Adapter)(nil)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolAnthropicMessages }

type codecRequest struct {
	Model         string            `json:"model"`
	System        json.RawMessage   `json:"system"`
	Messages      json.RawMessage   `json:"messages"`
	MaxTokens     *int              `json:"max_tokens"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	TopK          *int              `json:"top_k"`
	StopSequences []json.RawMessage `json:"stop_sequences"`
	Metadata      json.RawMessage   `json:"metadata"`
	Stream        bool              `json:"stream"`
	Thinking      json.RawMessage   `json:"thinking"`
	Tools         json.RawMessage   `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
}

func (*Adapter) DecodeRequest(request *http.Request) (*ir.Request, error) {
	if request == nil || request.Body == nil {
		return nil, codecInvalid("request body is required")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(request.Body)
	var body json.RawMessage
	if err := decoder.Decode(&body); err != nil {
		if contextErr := request.Context().Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, codecInvalid("request body must contain one JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if contextErr := request.Context().Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, codecInvalid("request body must contain one JSON object")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	fields, err := codecObject(body, "request body")
	if err != nil {
		return nil, err
	}
	var input codecRequest
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, codecInvalid("request fields have invalid types")
	}
	if strings.TrimSpace(input.Model) == "" {
		return nil, codecInvalid("model must be a non-empty string")
	}
	isCountTokens := request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/count_tokens")
	if (input.MaxTokens == nil && !isCountTokens) || (input.MaxTokens != nil && *input.MaxTokens < 0) {
		return nil, codecInvalid("max_tokens must be a non-negative integer")
	}
	for _, name := range []string{"temperature", "top_p", "top_k", "stop_sequences", "stream"} {
		if raw, exists := fields[name]; exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, codecInvalid(name + " must not be null")
		}
	}
	if input.Temperature != nil && (*input.Temperature < 0 || *input.Temperature > 1) {
		return nil, codecInvalid("temperature must be between 0 and 1")
	}
	if input.TopP != nil && (*input.TopP < 0 || *input.TopP > 1) {
		return nil, codecInvalid("top_p must be between 0 and 1")
	}
	if input.TopK != nil && *input.TopK < 0 {
		return nil, codecInvalid("top_k must be a non-negative integer")
	}
	var stopSequences []string
	if input.StopSequences != nil {
		stopSequences = make([]string, len(input.StopSequences))
		for index, raw := range input.StopSequences {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &stopSequences[index]) != nil {
				return nil, codecInvalid(fmt.Sprintf("stop_sequences[%d] must be a string", index))
			}
		}
	}
	out := &ir.Request{
		Model:  input.Model,
		Stream: input.Stream,
		Sampling: ir.Sampling{
			MaxOutputTokens: input.MaxTokens,
			Temperature:     input.Temperature,
			TopP:            input.TopP,
			TopK:            input.TopK,
			Stop:            stopSequences,
		},
	}
	if len(input.System) > 0 {
		var preserve bool
		blocks, err := codecContent(input.System, "system", "system", &preserve)
		if err != nil {
			return nil, err
		}
		out.Instructions = []ir.Instruction{{Role: ir.InstructionSystem, Content: blocks}}
		if !preserve {
			delete(fields, "system")
		}
	}
	var messages []json.RawMessage
	if len(input.Messages) == 0 || json.Unmarshal(input.Messages, &messages) != nil || (len(messages) == 0 && !isCountTokens) {
		return nil, codecInvalid("messages must be a non-empty array")
	}
	out.Messages = make([]ir.Message, 0, len(messages))
	var preserveMessages bool
	for index, raw := range messages {
		path := fmt.Sprintf("messages[%d]", index)
		message, err := codecObject(raw, path)
		if err != nil {
			return nil, err
		}
		role, err := codecString(message, "role", path, true)
		if err != nil {
			return nil, err
		}
		if role != string(ir.RoleUser) && role != string(ir.RoleAssistant) {
			return nil, codecInvalid(path + ".role must be user or assistant")
		}
		blocks, err := codecContent(message["content"], path+".content", role, &preserveMessages)
		if err != nil {
			return nil, err
		}
		preserveMessages = preserveMessages || codecHasUnknown(message, "role", "content")
		// IR represents Anthropic tool results as RoleTool. Split mixed user
		// content without reordering it.
		current := ir.Message{Role: ir.Role(role)}
		for _, block := range blocks {
			blockRole := ir.Role(role)
			if block.Type == ir.BlockToolResult {
				blockRole = ir.RoleTool
			}
			if current.Role != blockRole {
				if len(current.Content) > 0 {
					out.Messages = append(out.Messages, current)
				}
				current = ir.Message{Role: blockRole}
			}
			current.Content = append(current.Content, block)
		}
		if len(current.Content) > 0 || len(blocks) == 0 {
			out.Messages = append(out.Messages, current)
		}
	}
	if !preserveMessages {
		delete(fields, "messages")
	}
	if len(input.Metadata) > 0 {
		metadata, err := codecObject(input.Metadata, "metadata")
		if err != nil {
			return nil, err
		}
		nullUserID := bytes.Equal(bytes.TrimSpace(metadata["user_id"]), []byte("null"))
		var userID string
		if !nullUserID {
			userID, err = codecString(metadata, "user_id", "metadata", false)
			if err != nil {
				return nil, err
			}
		}
		out.Metadata = &ir.Metadata{UserID: userID}
		if !nullUserID && !codecHasUnknown(metadata, "user_id") {
			delete(fields, "metadata")
		}
	}
	if len(input.Thinking) > 0 {
		thinking, err := codecObject(input.Thinking, "thinking")
		if err != nil {
			return nil, err
		}
		typ, err := codecString(thinking, "type", "thinking", true)
		if err != nil {
			return nil, err
		}
		enabled := typ != "disabled"
		out.Reasoning = &ir.ReasoningConfig{Enabled: &enabled}
		switch typ {
		case "enabled":
			var budget *int
			if json.Unmarshal(thinking["budget_tokens"], &budget) != nil || budget == nil || *budget < 1024 {
				return nil, codecInvalid("thinking.budget_tokens must be an integer of at least 1024")
			}
			out.Reasoning.MaxTokens = budget
		case "disabled", "adaptive":
		default:
			return nil, codecInvalid("thinking.type must be enabled, disabled or adaptive")
		}
		if typ == "enabled" && !codecHasUnknown(thinking, "type", "budget_tokens") || typ == "disabled" && !codecHasUnknown(thinking, "type") {
			delete(fields, "thinking")
		}
	}
	if len(input.Tools) > 0 {
		var tools []json.RawMessage
		if json.Unmarshal(input.Tools, &tools) != nil || tools == nil {
			return nil, codecInvalid("tools must be an array")
		}
		out.Tools = make([]ir.Tool, 0, len(tools))
		preserve := len(tools) == 0
		for index, raw := range tools {
			path := fmt.Sprintf("tools[%d]", index)
			tool, err := codecObject(raw, path)
			if err != nil {
				return nil, err
			}
			name, err := codecString(tool, "name", path, true)
			if err != nil {
				return nil, err
			}
			description, err := codecString(tool, "description", path, false)
			if err != nil {
				return nil, err
			}
			schema := tool["input_schema"]
			if len(schema) > 0 {
				if _, err := codecObject(schema, path+".input_schema"); err != nil {
					return nil, err
				}
			}
			out.Tools = append(out.Tools, ir.Tool{Name: name, Description: description, Schema: schema})
			preserve = preserve || codecHasUnknown(tool, "name", "description", "input_schema")
		}
		if !preserve {
			delete(fields, "tools")
		}
	}
	if len(input.ToolChoice) > 0 {
		trimmed := bytes.TrimSpace(input.ToolChoice)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			var choiceStr string
			if err := json.Unmarshal(trimmed, &choiceStr); err != nil {
				return nil, codecInvalid("tool_choice must be a string or object")
			}
			switch ir.ToolChoiceMode(choiceStr) {
			case ir.ToolChoiceAuto, ir.ToolChoiceAny, ir.ToolChoiceNone:
				out.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceMode(choiceStr)}
				delete(fields, "tool_choice")
			default:
				return nil, codecInvalid(fmt.Sprintf("tool_choice string %q is not supported", choiceStr))
			}
		} else {
			choice, err := codecObject(input.ToolChoice, "tool_choice")
			if err != nil {
				return nil, err
			}
			typ, err := codecString(choice, "type", "tool_choice", true)
			if err != nil {
				return nil, err
			}
			switch ir.ToolChoiceMode(typ) {
			case ir.ToolChoiceAuto, ir.ToolChoiceAny, ir.ToolChoiceNone, ir.ToolChoiceTool:
				out.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceMode(typ)}
				out.ToolChoice.Name, err = codecString(choice, "name", "tool_choice", typ == string(ir.ToolChoiceTool))
				if err != nil {
					return nil, err
				}
				if raw, exists := choice["disable_parallel_tool_use"]; exists {
					if json.Unmarshal(raw, &out.ToolChoice.DisableParallelToolUse) != nil || out.ToolChoice.DisableParallelToolUse == nil {
						return nil, codecInvalid("tool_choice.disable_parallel_tool_use must be a boolean")
					}
				}
				if !codecHasUnknown(choice, "type", "name", "disable_parallel_tool_use") {
					delete(fields, "tool_choice")
				}
			default:
				return nil, codecInvalid(fmt.Sprintf("tool_choice.type %q is not supported", typ))
			}
		}
	}
	if raw, exists := fields["disable_parallel_tool_use"]; exists {
		var val *bool
		if json.Unmarshal(raw, &val) != nil || val == nil {
			return nil, codecInvalid("disable_parallel_tool_use must be a boolean")
		}
		if out.ToolChoice == nil {
			out.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
		}
		out.ToolChoice.DisableParallelToolUse = val
		delete(fields, "disable_parallel_tool_use")
	}
	// Block/turn IR has no extension slot. Only unrepresented nested data
	// retains its enclosing raw field at the original wire key. Outbound
	// builders must merge only unrepresented extras with normalized IR, never
	// replace IR messages/instructions/tools wholesale with the wire value.
	for _, name := range []string{"model", "max_tokens", "temperature", "top_p", "top_k", "stream"} {
		delete(fields, name)
	}
	if len(input.StopSequences) > 0 {
		delete(fields, "stop_sequences")
	}
	if len(fields) > 0 {
		out.Extensions = fields
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func codecInvalid(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func codecObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, codecInvalid(path + " must be an object")
	}
	return object, nil
}

func codecString(object map[string]json.RawMessage, key, path string, required bool) (string, error) {
	raw, exists := object[key]
	if !exists && !required {
		return "", nil
	}
	var value *string
	if json.Unmarshal(raw, &value) != nil || value == nil || (required && *value == "") {
		return "", codecInvalid(path + "." + key + " must be a string")
	}
	return *value, nil
}

func codecHasUnknown(object map[string]json.RawMessage, known ...string) bool {
	for key := range object {
		found := false
		for _, name := range known {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func parseTextOrThinking(text string) []ir.Block {
	const openTag = "<thinking>"
	const closeTag = "</thinking>"
	if !strings.Contains(text, openTag) || !strings.Contains(text, closeTag) {
		return []ir.Block{ir.NewTextBlock(text)}
	}
	var blocks []ir.Block
	remaining := text
	for {
		start := strings.Index(remaining, openTag)
		if start == -1 {
			break
		}
		end := strings.Index(remaining[start+len(openTag):], closeTag)
		if end == -1 {
			break
		}
		end += start + len(openTag)
		before := remaining[:start]
		thought := remaining[start+len(openTag) : end]
		remaining = remaining[end+len(closeTag):]

		if before != "" {
			blocks = append(blocks, ir.NewTextBlock(before))
		}
		blocks = append(blocks, ir.NewReasoningBlock(thought, ""))
	}
	if remaining != "" {
		blocks = append(blocks, ir.NewTextBlock(remaining))
	}
	if len(blocks) == 0 {
		return []ir.Block{ir.NewTextBlock(text)}
	}
	return blocks
}

func codecContent(raw json.RawMessage, path, role string, preserve *bool) ([]ir.Block, error) {
	var text string
	if len(raw) > 0 && bytes.TrimSpace(raw)[0] == '"' && json.Unmarshal(raw, &text) == nil {
		if role == string(ir.RoleAssistant) {
			return parseTextOrThinking(text), nil
		}
		return []ir.Block{ir.NewTextBlock(text)}, nil
	}
	var content []json.RawMessage
	if json.Unmarshal(raw, &content) != nil || content == nil {
		return nil, codecInvalid(path + " must be a string or array")
	}
	blocks := make([]ir.Block, 0, len(content))
	for index, raw := range content {
		blockPath := fmt.Sprintf("%s[%d]", path, index)
		object, err := codecObject(raw, blockPath)
		if err != nil {
			return nil, err
		}
		typ, err := codecString(object, "type", blockPath, true)
		if err != nil {
			return nil, err
		}
		if role == "system" && typ != "text" {
			return nil, codecInvalid("system blocks must have type text")
		}
		var block ir.Block
		switch typ {
		case "text":
			text, err := codecString(object, "text", blockPath, false)
			if err != nil {
				return nil, err
			}
			if _, exists := object["text"]; !exists {
				return nil, codecInvalid(blockPath + ".text is required")
			}
			if role == string(ir.RoleAssistant) {
				parsed := parseTextOrThinking(text)
				if len(parsed) > 1 || (len(parsed) == 1 && parsed[0].Type == ir.BlockReasoning) {
					for _, b := range parsed {
						b.Index = len(blocks)
						blocks = append(blocks, b)
					}
					continue
				}
				block = parsed[0]
			} else {
				block = ir.NewTextBlock(text)
			}
		case "image":
			source, err := codecObject(object["source"], blockPath+".source")
			if err != nil {
				return nil, err
			}
			sourceType, err := codecString(source, "type", blockPath+".source", true)
			if err != nil {
				return nil, err
			}
			*preserve = *preserve || codecHasUnknown(object, "type", "source")
			switch sourceType {
			case "base64":
				mediaType, err := codecString(source, "media_type", blockPath+".source", true)
				if err != nil {
					return nil, err
				}
				data, err := codecString(source, "data", blockPath+".source", true)
				if err != nil {
					return nil, err
				}
				if !strings.HasPrefix(mediaType, "image/") {
					return nil, codecInvalid(blockPath + ".source.media_type must be an image media type")
				}
				if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
					return nil, codecInvalid(blockPath + ".source.data must be base64")
				}
				block = ir.NewImageBlockBase64(mediaType, data)
				*preserve = *preserve || codecHasUnknown(source, "type", "media_type", "data")
			case "url":
				imageURL, err := codecString(source, "url", blockPath+".source", true)
				if err != nil {
					return nil, err
				}
				if strings.HasPrefix(imageURL, "data:") {
					mediaType, data, ok := ir.ParseDataURL(imageURL)
					if !ok || !strings.HasPrefix(mediaType, "image/") || data == "" {
						return nil, codecInvalid(blockPath + ".source.url is an invalid data URL")
					}
					if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
						return nil, codecInvalid(blockPath + ".source.url must be base64")
					}
					block = ir.NewImageBlockBase64(mediaType, data)
				} else {
					parsed, err := url.Parse(imageURL)
					if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
						return nil, codecInvalid(blockPath + ".source.url must be an absolute HTTP or HTTPS URL")
					}
					block = ir.NewImageBlockURL(imageURL)
				}
				*preserve = *preserve || codecHasUnknown(source, "type", "url")
			default:
				// File-backed and future sources are retained in raw messages.
				*preserve = true
				continue
			}
		case "tool_use":
			id, err := codecString(object, "id", blockPath, true)
			if err != nil {
				return nil, err
			}
			name, err := codecString(object, "name", blockPath, true)
			if err != nil {
				return nil, err
			}
			input := object["input"]
			if len(input) > 0 {
				if _, err := codecObject(input, blockPath+".input"); err != nil {
					return nil, err
				}
			}
			block = ir.NewToolUseBlock(id, name, input)
		case "tool_result":
			id, err := codecString(object, "tool_use_id", blockPath, true)
			if err != nil {
				return nil, err
			}
			var result []ir.Block
			if raw, exists := object["content"]; exists {
				result, err = codecContent(raw, blockPath+".content", "tool_result", preserve)
				if err != nil {
					return nil, err
				}
			}
			var isError bool
			if raw, exists := object["is_error"]; exists {
				var value *bool
				if json.Unmarshal(raw, &value) != nil || value == nil {
					return nil, codecInvalid(blockPath + ".is_error must be a boolean")
				}
				isError = *value
				if !isError {
					*preserve = true
				}
			}
			block = ir.NewToolResultBlock(id, result, isError)
		case "thinking":
			thinking, err := codecString(object, "thinking", blockPath, false)
			if err != nil {
				return nil, err
			}
			if _, exists := object["thinking"]; !exists {
				return nil, codecInvalid(blockPath + ".thinking is required")
			}
			signature, err := codecString(object, "signature", blockPath, false)
			if err != nil {
				return nil, err
			}
			block = ir.NewReasoningBlock(thinking, signature)
		default:
			// The original enclosing field retains unrepresented content and
			// its position, rather than manufacturing an invalid IR block.
			*preserve = true
			continue
		}
		switch typ {
		case "text":
			*preserve = *preserve || codecHasUnknown(object, "type", "text")
		case "tool_use":
			*preserve = *preserve || codecHasUnknown(object, "type", "id", "name", "input")
		case "tool_result":
			*preserve = *preserve || codecHasUnknown(object, "type", "tool_use_id", "content", "is_error")
		case "thinking":
			*preserve = *preserve || codecHasUnknown(object, "type", "thinking", "signature")
		}
		block.Index = index
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func (a *Adapter) EncodeResponse(response *ir.Response) ([]byte, error) {
	if response == nil {
		return nil, codecResponseError("response is required")
	}
	var stopReason string
	switch response.StopReason {
	case ir.StopEndTurn, ir.StopMaxTokens, ir.StopSequence, ir.StopToolUse:
		stopReason = string(response.StopReason)
	case ir.StopContentFilter:
		stopReason = "refusal"
	case ir.StopError:
		return nil, &gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "upstream generation failed"}
	default:
		return nil, codecResponseError("response has an unknown stop reason")
	}
	content := make([]map[string]any, 0, len(response.Content))
	for _, block := range response.Content {
		switch block.Type {
		case ir.BlockText:
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case ir.BlockReasoning:
			if a.DisableReasoning {
				continue
			}
			if block.Reasoning == nil {
				return nil, codecResponseError("reasoning block has no payload")
			}
			if a.FormatThinkingTags {
				content = append(content, map[string]any{"type": "text", "text": "<thinking>" + block.Reasoning.Content + "</thinking>"})
			} else {
				content = append(content, map[string]any{"type": "thinking", "thinking": block.Reasoning.Content, "signature": block.Reasoning.Signature})
			}
		case ir.BlockToolUse:
			if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
				return nil, codecResponseError("tool use block has no valid payload")
			}
			input := block.ToolUse.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			if _, err := codecObject(input, "tool input"); err != nil {
				return nil, codecResponseError("tool use input must be a JSON object")
			}
			content = append(content, map[string]any{"type": "tool_use", "id": block.ToolUse.ID, "name": block.ToolUse.Name, "input": input})
		default:
			return nil, codecResponseError("response contains an unsupported content block")
		}
	}
	var stopSequence *string
	if response.StopReason == ir.StopSequence {
		stopSequence = response.StopSequence
	}
	// Response.Model is the client alias supplied by the dispatcher, not an
	// upstream model name. Encoding must never infer it from another request.
	body := struct {
		ID           string           `json:"id"`
		Type         string           `json:"type"`
		Role         string           `json:"role"`
		Model        string           `json:"model"`
		Content      []map[string]any `json:"content"`
		StopReason   string           `json:"stop_reason"`
		StopSequence *string          `json:"stop_sequence"`
		Usage        struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheReadTokens  int `json:"cache_read_input_tokens"`
			CacheWriteTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}{ID: response.ID, Type: "message", Role: "assistant", Model: response.Model,
		Content: content, StopReason: stopReason, StopSequence: stopSequence}
	body.Usage.InputTokens = response.Usage.InputTokens
	body.Usage.OutputTokens = response.Usage.OutputTokens
	body.Usage.CacheReadTokens = response.Usage.CacheReadTokens
	body.Usage.CacheWriteTokens = response.Usage.CacheWriteTokens
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, codecResponseError("response cannot be encoded as JSON")
	}
	return encoded, nil
}

func codecResponseError(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInternal, Message: message}
}
