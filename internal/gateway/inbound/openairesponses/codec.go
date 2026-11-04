// Package openairesponses decodes stateless Responses requests and encodes
// normalized responses. Its embedded encoder supplies the Responses SSE path.
package openairesponses

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

type Adapter struct {
	StreamEncoder
}

var _ adapter.Inbound = (*Adapter)(nil)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIResponses }

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    json.RawMessage `json:"instructions"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          bool            `json:"stream"`
	User            *string         `json:"user"`
	Metadata        json.RawMessage `json:"metadata"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Text *struct {
		Format json.RawMessage `json:"format"`
	} `json:"text"`
	Tools              []json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	PreviousResponseID json.RawMessage   `json:"previous_response_id"`
	Conversation       json.RawMessage   `json:"conversation"`
	Store              *bool             `json:"store"`
}

func invalidResponses(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupportedResponses(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: message}
}

func absentResponsesJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func responsesJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && trimmed[0] == '{'
}

// Input, instructions and other composite wire fields remain in Extensions.
// They preserve interleaving, unknown parts and exact numbers; outbound
// adapters must not use them to override normalized routing or changed IR.
func (*Adapter) DecodeRequest(request *http.Request) (*ir.Request, error) {
	if request == nil || request.Body == nil {
		return nil, invalidResponses("a JSON request body is required")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(request.Body)
	var raw, trailing json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if cause := request.Context().Err(); cause != nil {
			return nil, cause
		}
		return nil, invalidResponses("request body must be a JSON object")
	}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if cause := request.Context().Err(); cause != nil {
			return nil, cause
		}
		return nil, invalidResponses("request body must contain exactly one JSON object")
	}
	var wire responsesRequest
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, invalidResponses("request fields have invalid JSON types")
	}
	if err := json.Unmarshal(raw, &extensions); err != nil || extensions == nil {
		return nil, invalidResponses("request body must be a JSON object")
	}
	if strings.TrimSpace(wire.Model) == "" {
		return nil, invalidResponses("model is required")
	}
	if !absentResponsesJSON(wire.PreviousResponseID) || !absentResponsesJSON(wire.Conversation) || wire.Store != nil && *wire.Store {
		return nil, unsupportedResponses("stateful Responses features require a same-protocol passthrough")
	}
	if wire.MaxOutputTokens != nil && *wire.MaxOutputTokens < 0 {
		return nil, invalidResponses("max_output_tokens must be a non-negative integer")
	}
	if !absentResponsesJSON(wire.Metadata) && !responsesJSONObject(wire.Metadata) {
		return nil, invalidResponses("metadata must be an object")
	}
	result := &ir.Request{Model: wire.Model, Messages: []ir.Message{}, Sampling: ir.Sampling{MaxOutputTokens: wire.MaxOutputTokens, Temperature: wire.Temperature, TopP: wire.TopP}, Stream: wire.Stream, Extensions: extensions}
	for _, field := range []string{"model", "max_output_tokens", "temperature", "top_p", "stream", "user"} {
		if !absentResponsesJSON(extensions[field]) {
			delete(extensions, field)
		}
	}
	if !absentResponsesJSON(wire.Instructions) {
		var text string
		if err := json.Unmarshal(wire.Instructions, &text); err != nil {
			return nil, invalidResponses("instructions must be a string")
		}
		result.Instructions = append(result.Instructions, ir.NewTextInstruction(ir.InstructionDeveloper, text))
	}
	if err := decodeResponsesInput(result, wire.Input); err != nil {
		return nil, err
	}
	if wire.User != nil {
		result.Metadata = &ir.Metadata{UserID: *wire.User}
	}
	if wire.Reasoning != nil {
		result.Reasoning = &ir.ReasoningConfig{Effort: ir.ReasoningEffort(wire.Reasoning.Effort)}
	}
	if wire.Text != nil && !absentResponsesJSON(wire.Text.Format) {
		format, err := decodeResponsesFormat(wire.Text.Format)
		if err != nil {
			return nil, err
		}
		result.ResponseFormat = format
	}
	for _, rawTool := range wire.Tools {
		var header struct {
			Type string `json:"type"`
		}
		if !responsesJSONObject(rawTool) || json.Unmarshal(rawTool, &header) != nil {
			return nil, invalidResponses("tools must contain objects")
		}
		if header.Type != "function" {
			continue
		}
		var tool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(rawTool, &tool); err != nil || tool.Name == "" || !absentResponsesJSON(tool.Parameters) && !responsesJSONObject(tool.Parameters) {
			return nil, invalidResponses("function tools require a name and object parameters")
		}
		result.Tools = append(result.Tools, ir.Tool{Name: tool.Name, Description: tool.Description, Schema: tool.Parameters})
	}
	if !absentResponsesJSON(wire.ToolChoice) {
		choice, err := decodeResponsesToolChoice(wire.ToolChoice)
		if err != nil {
			return nil, err
		}
		result.ToolChoice = choice
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeResponsesInput(result *ir.Request, raw json.RawMessage) error {
	if absentResponsesJSON(raw) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		result.Messages = append(result.Messages, ir.NewMessage(ir.RoleUser, ir.NewTextBlock(text)))
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return invalidResponses("input must be a string or an item array")
	}
	for _, item := range items {
		var header struct {
			Type string `json:"type"`
		}
		if !responsesJSONObject(item) || json.Unmarshal(item, &header) != nil {
			return invalidResponses("input items must be objects")
		}
		switch header.Type {
		case "", "message":
			var message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(item, &message); err != nil {
				return invalidResponses("message fields have invalid types")
			}
			blocks, err := decodeResponsesContent(message.Content)
			if err != nil {
				return err
			}
			switch message.Role {
			case "system", "developer":
				result.Instructions = append(result.Instructions, ir.Instruction{Role: ir.InstructionRole(message.Role), Content: blocks})
			case "user", "assistant":
				result.Messages = append(result.Messages, ir.Message{Role: ir.Role(message.Role), Content: blocks})
			default:
				return invalidResponses("message role must be user, assistant, system or developer")
			}
		case "function_call":
			var call struct {
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(item, &call); err != nil || call.CallID == "" || call.Name == "" {
				return invalidResponses("function_call requires call_id and name")
			}
			var arguments string
			if absentResponsesJSON(call.Arguments) || json.Unmarshal(call.Arguments, &arguments) != nil {
				return invalidResponses("function_call arguments must be a JSON string")
			}
			result.Messages = append(result.Messages, ir.NewMessage(ir.RoleAssistant, ir.NewToolUseBlock(call.CallID, call.Name, call.Arguments)))
		case "function_call_output":
			var output struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(item, &output); err != nil || output.CallID == "" {
				return invalidResponses("function_call_output requires call_id")
			}
			blocks, err := decodeResponsesContent(output.Output)
			if err != nil {
				return err
			}
			result.Messages = append(result.Messages, ir.NewMessage(ir.RoleTool, ir.NewToolResultBlock(output.CallID, blocks, false)))
		case "reasoning":
			var reasoning struct {
				Summary []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"summary"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if err := json.Unmarshal(item, &reasoning); err != nil {
				return invalidResponses("reasoning fields have invalid types")
			}
			var summary strings.Builder
			for _, part := range reasoning.Summary {
				if part.Type == "summary_text" {
					summary.WriteString(part.Text)
				}
			}
			result.Messages = append(result.Messages, ir.NewMessage(ir.RoleAssistant, ir.NewReasoningBlock(summary.String(), reasoning.EncryptedContent)))
		case "item_reference":
			return unsupportedResponses("item references require a stateful Responses upstream")
		}
	}
	return nil
}

func decodeResponsesContent(raw json.RawMessage) ([]ir.Block, error) {
	if absentResponsesJSON(raw) {
		return nil, invalidResponses("content must be a string or a content-part array")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []ir.Block{ir.NewTextBlock(text)}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, invalidResponses("content must be a string or a content-part array")
	}
	blocks := make([]ir.Block, 0, len(parts))
	for _, part := range parts {
		var header struct {
			Type string `json:"type"`
		}
		if !responsesJSONObject(part) || json.Unmarshal(part, &header) != nil || header.Type == "" {
			return nil, invalidResponses("content parts must be typed objects")
		}
		switch header.Type {
		case "input_text", "output_text":
			var value struct {
				Text *string `json:"text"`
			}
			if err := json.Unmarshal(part, &value); err != nil || value.Text == nil {
				return nil, invalidResponses("text content requires a text string")
			}
			blocks = append(blocks, ir.NewTextBlock(*value.Text))
		case "input_image":
			var value struct {
				ImageURL string `json:"image_url"`
			}
			if err := json.Unmarshal(part, &value); err != nil || value.ImageURL == "" {
				return nil, invalidResponses("image content requires image_url")
			}
			if strings.HasPrefix(value.ImageURL, "data:") {
				metadata, payload, ok := strings.Cut(strings.TrimPrefix(value.ImageURL, "data:"), ",")
				mediaType, encoded := strings.CutSuffix(metadata, ";base64")
				if !ok || !encoded || !strings.HasPrefix(mediaType, "image/") || payload == "" {
					return nil, invalidResponses("inline images require an image base64 data URL")
				}
				blocks = append(blocks, ir.NewImageBlockBase64(mediaType, payload))
			} else {
				blocks = append(blocks, ir.NewImageBlockURL(value.ImageURL))
			}
		}
	}
	return blocks, nil
}

func decodeResponsesFormat(raw json.RawMessage) (*ir.ResponseFormat, error) {
	var format struct {
		Type   ir.ResponseFormatType `json:"type"`
		Name   string                `json:"name"`
		Strict bool                  `json:"strict"`
		Schema json.RawMessage       `json:"schema"`
	}
	if !responsesJSONObject(raw) || json.Unmarshal(raw, &format) != nil {
		return nil, invalidResponses("text.format must be an object")
	}
	result := &ir.ResponseFormat{Type: format.Type}
	switch format.Type {
	case ir.ResponseFormatText, ir.ResponseFormatJSONObject:
	case ir.ResponseFormatJSONSchema:
		if format.Name == "" || !responsesJSONObject(format.Schema) {
			return nil, invalidResponses("JSON schema format requires name and object schema")
		}
		result.JSONSchema = &ir.JSONSchema{Name: format.Name, Strict: format.Strict, Schema: format.Schema}
	default:
		return nil, unsupportedResponses("text.format type is not supported for translation")
	}
	return result, nil
}

func decodeResponsesToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		choice := &ir.ToolChoice{}
		switch mode {
		case "auto":
			choice.Mode = ir.ToolChoiceAuto
		case "none":
			choice.Mode = ir.ToolChoiceNone
		case "required":
			choice.Mode = ir.ToolChoiceAny
		default:
			return nil, invalidResponses("tool_choice must be auto, none, required or a tool object")
		}
		return choice, nil
	}
	var header struct {
		Type string `json:"type"`
	}
	if !responsesJSONObject(raw) || json.Unmarshal(raw, &header) != nil || header.Type == "" {
		return nil, invalidResponses("tool_choice must be a string or typed tool object")
	}
	if header.Type != "function" {
		return nil, nil
	}
	var choice struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil || choice.Name == "" {
		return nil, invalidResponses("function tool_choice requires name")
	}
	return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: choice.Name}, nil
}

func (a *Adapter) EncodeResponse(response *ir.Response) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("openairesponses: response is required")
	}
	created := a.Created
	if created <= 0 {
		created = time.Now().Unix()
	}
	wire := responseStreamResponse{ID: response.ID, Object: "response", CreatedAt: created, Status: "completed", Model: response.Model, Output: make([]*responseStreamItem, 0, len(response.Content))}
	switch response.StopReason {
	case ir.StopEndTurn, ir.StopSequence, ir.StopToolUse:
	case ir.StopMaxTokens:
		wire.Status, wire.IncompleteDetails = "incomplete", &responseStreamIncomplete{Reason: "max_output_tokens"}
	case ir.StopContentFilter:
		wire.Status, wire.IncompleteDetails = "incomplete", &responseStreamIncomplete{Reason: "content_filter"}
	default:
		return nil, fmt.Errorf("openairesponses: stop reason %q cannot encode a response", response.StopReason)
	}
	for _, block := range response.Content {
		if block.Type == ir.BlockText && len(wire.Output) != 0 && wire.Output[len(wire.Output)-1].Type == "message" {
			last := wire.Output[len(wire.Output)-1]
			*last.Content = append(*last.Content, newResponseStreamTextPart(block.Text))
			continue
		}
		item := &responseStreamItem{Status: wire.Status}
		prefix := ""
		switch block.Type {
		case ir.BlockText:
			part := newResponseStreamTextPart(block.Text)
			prefix, item.Type, item.Role = "msg_", "message", "assistant"
			item.Content = &[]responseStreamPart{part}
		case ir.BlockReasoning:
			if block.Reasoning == nil {
				return nil, fmt.Errorf("openairesponses: reasoning block has no payload")
			}
			prefix, item.Type = "rs_", "reasoning"
			item.Summary = &[]responseStreamPart{{Type: "summary_text", Text: block.Reasoning.Content}}
			item.EncryptedContent = block.Reasoning.Signature
		case ir.BlockToolUse:
			if block.ToolUse == nil || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
				return nil, fmt.Errorf("openairesponses: tool use requires payload, ID and name")
			}
			prefix, item.Type, item.CallID = "fc_", "function_call", block.ToolUse.ID
			name, arguments := block.ToolUse.Name, "{}"
			if !absentResponsesJSON(block.ToolUse.Input) {
				if !json.Valid(block.ToolUse.Input) {
					return nil, fmt.Errorf("openairesponses: tool arguments contain invalid JSON")
				}
				if err := json.Unmarshal(block.ToolUse.Input, &arguments); err != nil {
					arguments = string(block.ToolUse.Input)
				}
			}
			item.Name, item.Arguments = &name, &arguments
		default:
			return nil, fmt.Errorf("openairesponses: response block %q is not representable", block.Type)
		}
		item.ID = fmt.Sprintf("%s%s_%d", prefix, wire.ID, len(wire.Output))
		wire.Output = append(wire.Output, item)
	}
	usage := response.Usage
	wire.Usage = &responseStreamUsage{InputTokens: usage.InputTokens, InputTokensDetails: responseStreamInputDetails{CachedTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens}, OutputTokens: usage.OutputTokens, OutputTokensDetails: responseStreamOutputDetails{ReasoningTokens: usage.ReasoningTokens}, TotalTokens: usage.InputTokens + usage.OutputTokens}
	return json.Marshal(wire)
}
