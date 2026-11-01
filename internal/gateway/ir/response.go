package ir

// StopReason is the normalized reason a generation stopped. The enum is the
// union of the three protocols' finish reasons; adapters map their wire
// values onto it and onto each other through it.
type StopReason string

const (
	// StopEndTurn means the model finished naturally.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTokens means a token budget was hit (OpenAI "length",
	// Anthropic "max_tokens", Responses "incomplete").
	StopMaxTokens StopReason = "max_tokens"
	// StopSequence means a configured stop sequence matched.
	StopSequence StopReason = "stop_sequence"
	// StopToolUse means the model issued a tool call.
	StopToolUse StopReason = "tool_use"
	// StopContentFilter means content filtering stopped the generation.
	StopContentFilter StopReason = "content_filter"
	// StopError means the stream or call ended because of an error.
	StopError StopReason = "error"
)

// Usage is the token accounting for one request. Zero values mean
// "unreported", not "no tokens".
type Usage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

// Response is the protocol-neutral form of one completed (non-streaming)
// request. Content holds assistant blocks in order; a tool-calling response
// carries tool_use blocks and StopReason is StopToolUse.
type Response struct {
	ID           string     `json:"id"`
	Model        string     `json:"model"`
	Content      []Block    `json:"content,omitempty"`
	StopReason   StopReason `json:"stop_reason"`
	StopSequence *string    `json:"stop_sequence,omitempty"`
	Usage        Usage      `json:"usage"`
}

// NewTextResponse is a convenience constructor for the common case of a
// successful plain-text response.
func NewTextResponse(id, model, text string, usage Usage) Response {
	return Response{
		ID:         id,
		Model:      model,
		Content:    []Block{NewTextBlock(text)},
		StopReason: StopEndTurn,
		Usage:      usage,
	}
}
