package ir

import "encoding/json"

// Role is the author role of a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries the result of a tool call. OpenAI Chat models
	// represent it as a dedicated "tool" message; Anthropic Messages
	// embeds it as a tool_result content block on a user message. Both
	// decode to RoleTool holding one tool_result block.
	RoleTool Role = "tool"
)

// InstructionRole is the role of an out-of-band instruction (system prompt).
// Anthropic and OpenAI Chat use "system"; OpenAI's reasoning models and the
// Responses API use "developer".
type InstructionRole string

const (
	InstructionSystem    InstructionRole = "system"
	InstructionDeveloper InstructionRole = "developer"
)

// BlockType enumerates the content block kinds that can appear in a message
// or a response.
type BlockType string

const (
	// BlockText is plain text content.
	BlockText BlockType = "text"
	// BlockImage is an image, referenced by URL or inline base64.
	BlockImage BlockType = "image"
	// BlockToolUse is a model request to call a tool.
	BlockToolUse BlockType = "tool_use"
	// BlockToolResult is the outcome of a tool call.
	BlockToolResult BlockType = "tool_result"
	// BlockReasoning is model reasoning/thinking, typically not shown to
	// the end user. Anthropic calls it "thinking", OpenAI Chat exposes it
	// as reasoning_content, Responses as reasoning summary items.
	BlockReasoning BlockType = "reasoning"
)

// Image is one image content part in exactly one of its two wire forms:
// a URL, or inline base64 data with an explicit media type. Adapters must
// convert between the two (data URL <-> base64 source) as their protocol
// requires; the IR preserves whichever form the sender used.
type Image struct {
	// URL is the http(s) reference form. Empty when Base64 is used.
	URL string `json:"url,omitempty"`
	// Base64 is the raw base64 payload (no data: prefix). Empty when URL
	// is used.
	Base64 string `json:"base64,omitempty"`
	// MediaType is the image MIME type, for example "image/png". Required
	// with Base64; some providers require it with URL as well.
	MediaType string `json:"media_type,omitempty"`
}

// ToolUse is a model request to invoke a tool. ID and Input are preserved
// verbatim because protocols and providers disagree on ID formats and on
// whether arguments are a JSON string or a JSON object.
type ToolUse struct {
	// ID is the tool call identifier that a matching tool_result must echo
	// back unchanged.
	ID string `json:"id"`
	// Name is the tool function name.
	Name string `json:"name"`
	// Input is the raw arguments payload (a JSON object). May be nil or
	// empty for argument-less calls.
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResult is the outcome of a ToolUse, carried in a message with
// RoleTool (OpenAI) or as a user content block (Anthropic).
type ToolResult struct {
	// ToolUseID is the ID of the ToolUse this result answers.
	ToolUseID string `json:"tool_use_id"`
	// Content holds the result payload, usually a single text block. May
	// be empty when the tool returned nothing.
	Content []Block `json:"content,omitempty"`
	// IsError marks a failed tool execution.
	IsError bool `json:"is_error,omitempty"`
}

// ReasoningBlock carries model reasoning ("thinking") content. Signature
// holds Anthropic's encrypted thinking blob, which must be forwarded
// untouched on multi-turn conversations.
type ReasoningBlock struct {
	Content   string `json:"content"`
	Signature string `json:"signature,omitempty"`
}

// Block is one content part of a message, response, or tool result. The
// Type field selects which of the payload fields is meaningful; the
// constructors below are the supported way to build a block so that the
// type and payload can never disagree.
//
// Index is a stable, adapter-assigned position (counting from 0) so that
// streaming events can reference the same block across block_start,
// block_delta and block_stop. It is not related to the block's position in
// any wire array.
type Block struct {
	Type       BlockType       `json:"type"`
	Index      int             `json:"index,omitempty"`
	Text       string          `json:"text,omitempty"`
	Image      *Image          `json:"image,omitempty"`
	ToolUse    *ToolUse        `json:"tool_use,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
	Reasoning  *ReasoningBlock `json:"reasoning,omitempty"`
}

// NewTextBlock builds a text content block.
func NewTextBlock(text string) Block {
	return Block{Type: BlockText, Text: text}
}

// NewImageBlockURL builds an image block referenced by URL.
func NewImageBlockURL(url string) Block {
	return Block{Type: BlockImage, Image: &Image{URL: url}}
}

// NewImageBlockBase64 builds an inline image block.
func NewImageBlockBase64(mediaType, base64 string) Block {
	return Block{Type: BlockImage, Image: &Image{Base64: base64, MediaType: mediaType}}
}

// NewToolUseBlock builds a tool_use block.
func NewToolUseBlock(id, name string, input json.RawMessage) Block {
	return Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: id, Name: name, Input: input}}
}

// NewToolResultBlock builds a tool_result block answering the tool call
// with the given ID. Pass nil content for a result with no payload.
func NewToolResultBlock(toolUseID string, content []Block, isError bool) Block {
	return Block{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: toolUseID, Content: content, IsError: isError}}
}

// NewReasoningBlock builds a reasoning/thinking block.
func NewReasoningBlock(content, signature string) Block {
	return Block{Type: BlockReasoning, Reasoning: &ReasoningBlock{Content: content, Signature: signature}}
}

// Message is one conversational turn. Content may hold blocks of any type;
// an assistant turn answering a tool call holds tool_result blocks, and an
// assistant turn issuing tool calls holds tool_use blocks.
//
// ID and Model only name a response message: they are set by NewMessageStart
// on the message a stream opens, and inbound encoders emit them on
// message_start / response.created. Request messages leave them empty.
type Message struct {
	ID      string  `json:"id,omitempty"`
	Role    Role    `json:"role"`
	Model   string  `json:"model,omitempty"`
	Content []Block `json:"content,omitempty"`
}

// NewMessage builds a message from blocks. It is a convenience for the
// common one-block-per-message case; multi-block messages can be built
// directly.
func NewMessage(role Role, blocks ...Block) Message {
	return Message{Role: role, Content: blocks}
}

// Instruction is an out-of-band system/developer prompt. Instructions keep
// their order and their originating role because the Responses API requires
// developer instructions after (not merged into) system ones.
type Instruction struct {
	Role    InstructionRole `json:"role"`
	Content []Block         `json:"content,omitempty"`
}

// NewTextInstruction builds a single system or developer text instruction.
func NewTextInstruction(role InstructionRole, text string) Instruction {
	return Instruction{Role: role, Content: []Block{NewTextBlock(text)}}
}

// Tool describes one callable tool. Schema is the JSON Schema for its
// arguments (the OpenAI "parameters" / Anthropic "input_schema" object),
// kept raw so provider-specific schema dialects survive unchanged.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// ToolChoiceMode constrains how the model may call tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto ToolChoiceMode = "auto" // the model decides
	ToolChoiceNone ToolChoiceMode = "none" // tools must not be called
	// ToolChoiceAny requires at least one tool call; it maps to OpenAI's
	// "required".
	ToolChoiceAny ToolChoiceMode = "any"
	// ToolChoiceTool requires a call to one specific tool.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice is the requested tool-calling policy. Name must be set when
// Mode is ToolChoiceTool.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
	// DisableParallelToolUse maps to Anthropic's
	// disable_parallel_tool_use.
	DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
}

// Sampling groups the optional sampling parameters protocols share. Absent
// fields mean "not sent" and must not be turned into zero values on the
// wire.
type Sampling struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	Stop            []string `json:"stop,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`
}

// ResponseFormatType selects the requested output structure.
type ResponseFormatType string

const (
	ResponseFormatText       ResponseFormatType = "text"
	ResponseFormatJSONObject ResponseFormatType = "json_object"
	// ResponseFormatJSONSchema requests output conforming to a JSON Schema.
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"
)

// JSONSchema describes a structured-output schema: OpenAI Chat's
// json_schema format and Anthropic's tool-enforced JSON mode both reduce to
// this.
type JSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// ResponseFormat is the requested response structure. JSONSchema is set
// when Type is ResponseFormatJSONSchema.
type ResponseFormat struct {
	Type       ResponseFormatType `json:"type"`
	JSONSchema *JSONSchema        `json:"json_schema,omitempty"`
}

// ReasoningEffort is the thinking budget level a protocol may request.
type ReasoningEffort string

const (
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
)

// ReasoningConfig controls model reasoning. Enabled distinguishes an
// explicit "do not reason" (false) from an unset policy. MaxTokens bounds
// the reasoning budget (Anthropic thinking budget_tokens, OpenAI
// reasoning.max_tokens).
type ReasoningConfig struct {
	Enabled   *bool           `json:"enabled,omitempty"`
	Effort    ReasoningEffort `json:"effort,omitempty"`
	MaxTokens *int            `json:"max_tokens,omitempty"`
}

// Metadata carries end-user attribution protocols pass through.
type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

// Request is the protocol-neutral form of one completion request. The
// fields cover the intersection of the three supported protocols' documented
// request subsets; anything outside it travels in Extensions.
//
// Extensions maps the provider-specific parameter name (for example
// "cache_control" or "service_tier") to its raw JSON value. Adapters copy
// unknown inbound parameters they cannot represent into Extensions and
// outbound adapters forward matching keys if the upstream supports them, so
// round-tripping a request through a same-protocol pair loses nothing.
type Request struct {
	Model              string                     `json:"model"`
	Instructions       []Instruction              `json:"instructions,omitempty"`
	Messages           []Message                  `json:"messages"`
	Tools              []Tool                     `json:"tools,omitempty"`
	ToolChoice         *ToolChoice                `json:"tool_choice,omitempty"`
	Sampling           Sampling                   `json:"sampling,omitempty"`
	ResponseFormat     *ResponseFormat            `json:"response_format,omitempty"`
	Reasoning          *ReasoningConfig           `json:"reasoning,omitempty"`
	Stream             bool                       `json:"stream,omitempty"`
	StreamIncludeUsage bool                       `json:"stream_include_usage,omitempty"`
	Metadata           *Metadata                  `json:"metadata,omitempty"`
	Extensions         map[string]json.RawMessage `json:"extensions,omitempty"`
}
