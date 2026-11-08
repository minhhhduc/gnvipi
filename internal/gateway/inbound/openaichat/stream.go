package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// StreamEncoder renders normalized events as OpenAI Chat Completions chunks.
// IncludeUsage adds usage:null to ordinary chunks and a final choices-empty
// accounting chunk. Reasoning uses the reasoning_content extension unless
// DisableReasoning is set. Created is a fixed Unix timestamp when nonzero;
// zero selects the current time once per stream. Configuration is read-only
// during encoding, so one encoder can serve concurrent requests.
type StreamEncoder struct {
	IncludeUsage     bool
	DisableReasoning bool
	Created          int64
}

// EncodeStream borrows events and w: their caller retains ownership and must
// close the upstream stream. Every pull and write observes ctx. A successful
// message_stop emits finish, optional usage, then exactly one [DONE]; an IR
// error emits only an OpenAI error frame and returns. Neither terminal path
// pulls another event. Upstream read failures and conflicting tool identities
// emit an error frame before returning their cause; cancellation and writer
// failures return without recovery writes or a successful finish.
func (e StreamEncoder) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	if opts := adapter.StreamOptionsFrom(ctx); opts.IncludeUsage {
		e.IncludeUsage = true
	}
	created := e.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	state := streamState{encoder: e, ctx: ctx, writer: w, created: created, blocks: make(map[int]*streamBlock)}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		event, err := events.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return state.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if event.Type == ir.EventError {
			if event.Error == nil {
				return fmt.Errorf("openaichat: error event has no payload")
			}
			return state.writeError(event.Error)
		}
		if !state.started && event.Type != ir.EventMessageStart {
			return fmt.Errorf("openaichat: %s before message_start", event.Type)
		}
		switch event.Type {
		case ir.EventMessageStart:
			if state.started || event.Message == nil {
				return fmt.Errorf("openaichat: duplicate or missing message_start payload")
			}
			state.started = true
			state.id, state.model = event.Message.ID, event.Message.Model
			state.updateUsage(event.Usage)
			if err := state.writeChunk(streamDelta{Role: "assistant"}, "", false); err != nil {
				return err
			}
			// Initial message content is complete content, not an open block.
			for _, block := range event.Message.Content {
				started, err := state.startBlock(block)
				if err != nil {
					return err
				}
				if err := state.finishBlock(started); err != nil {
					return err
				}
			}
		case ir.EventBlockStart:
			if event.Block == nil || event.Index < 0 || state.blocks[event.Index] != nil {
				return fmt.Errorf("openaichat: invalid block_start at index %d", event.Index)
			}
			block, err := state.startBlock(*event.Block)
			if err != nil {
				return err
			}
			state.blocks[event.Index] = block
		case ir.EventBlockDelta:
			block := state.blocks[event.Index]
			if block == nil || !block.open || event.Delta == nil {
				return fmt.Errorf("openaichat: delta for unopened block %d or missing payload", event.Index)
			}
			if err := state.writeDelta(block, event.Delta); err != nil {
				return err
			}
		case ir.EventBlockStop:
			block := state.blocks[event.Index]
			if block == nil || !block.open {
				return fmt.Errorf("openaichat: stop for unopened block %d", event.Index)
			}
			if err := state.finishBlock(block); err != nil {
				return err
			}
		case ir.EventUsageUpdate:
			if state.usageUpdated || event.Usage == nil {
				return fmt.Errorf("openaichat: duplicate or missing usage_update payload")
			}
			state.usageUpdated = true
			state.updateUsage(event.Usage)
		case ir.EventMessageStop:
			for index, block := range state.blocks {
				if block.open {
					return fmt.Errorf("openaichat: message_stop with open block %d", index)
				}
			}
			if event.StopReason == ir.StopError {
				return state.writeError(&ir.StreamError{Kind: string(gwerr.KindUpstreamError), Message: "Upstream generation stopped with an error"})
			}
			finish, err := streamFinishReason(event.StopReason)
			if err != nil {
				return err
			}
			if err := state.writeChunk(streamDelta{}, finish, false); err != nil {
				return err
			}
			if e.IncludeUsage {
				if err := state.writeChunk(streamDelta{}, "", true); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return w.Done()
		default:
			return fmt.Errorf("openaichat: unknown event type %q", event.Type)
		}
	}
}

type streamState struct {
	encoder               StreamEncoder
	ctx                   context.Context
	writer                *sse.Writer
	created               int64
	id, model             string
	started, usageUpdated bool
	usage                 ir.Usage
	blocks                map[int]*streamBlock
	nextTool              int
}

type streamBlock struct {
	kind              ir.BlockType
	open              bool
	toolIndex         int
	id, name          string
	identitySent      bool
	argumentsReceived bool
	pendingInput      json.RawMessage
}

func (s *streamState) startBlock(prototype ir.Block) (*streamBlock, error) {
	block := &streamBlock{kind: prototype.Type, open: true}
	switch prototype.Type {
	case ir.BlockText:
		if prototype.Text != "" {
			text := prototype.Text
			if err := s.writeChunk(streamDelta{Content: &text}, "", false); err != nil {
				return nil, err
			}
		}
	case ir.BlockReasoning:
		if prototype.Reasoning != nil && prototype.Reasoning.Content != "" && !s.encoder.DisableReasoning {
			text := prototype.Reasoning.Content
			if err := s.writeChunk(streamDelta{Reasoning: &text}, "", false); err != nil {
				return nil, err
			}
		}
	case ir.BlockToolUse:
		block.toolIndex = s.nextTool
		s.nextTool++
		if prototype.ToolUse != nil {
			block.id, block.name = prototype.ToolUse.ID, prototype.ToolUse.Name
			block.pendingInput = prototype.ToolUse.Input
			if block.id != "" || block.name != "" {
				if err := s.writeTool(block, &ir.Delta{}); err != nil {
					return nil, err
				}
			}
		}
	default:
		return nil, fmt.Errorf("openaichat: unsupported output block type %q", prototype.Type)
	}
	return block, nil
}

func (s *streamState) finishBlock(block *streamBlock) error {
	if block.kind == ir.BlockToolUse && !block.argumentsReceived {
		arguments := "{}"
		if len(block.pendingInput) != 0 {
			input := bytes.TrimSpace(block.pendingInput)
			if len(input) != 0 && input[0] == '"' {
				if err := json.Unmarshal(block.pendingInput, &arguments); err != nil {
					return fmt.Errorf("openaichat: invalid tool argument wrapper: %w", err)
				}
			} else {
				arguments = string(block.pendingInput)
			}
		}
		if err := s.writeTool(block, &ir.Delta{PartialJSON: arguments}); err != nil {
			return err
		}
	}
	block.open = false
	return nil
}

func (s *streamState) writeDelta(block *streamBlock, delta *ir.Delta) error {
	switch {
	case block.kind == ir.BlockText && delta.Type == ir.DeltaText:
		return s.writeChunk(streamDelta{Content: &delta.Text}, "", false)
	case block.kind == ir.BlockReasoning && delta.Type == ir.DeltaReasoning:
		if s.encoder.DisableReasoning {
			return nil
		}
		return s.writeChunk(streamDelta{Reasoning: &delta.Text}, "", false)
	case block.kind == ir.BlockReasoning && delta.Type == ir.DeltaSignature:
		return nil
	case block.kind == ir.BlockToolUse && delta.Type == ir.DeltaToolArguments:
		return s.writeTool(block, delta)
	default:
		return fmt.Errorf("openaichat: delta type %q does not match block type %q", delta.Type, block.kind)
	}
}

func (s *streamState) writeTool(block *streamBlock, delta *ir.Delta) error {
	if delta.ToolCallID != "" && block.id != "" && delta.ToolCallID != block.id {
		return s.fail(fmt.Errorf("openaichat: conflicting tool ID at index %d", block.toolIndex))
	}
	if delta.ToolCallName != "" && block.name != "" && delta.ToolCallName != block.name {
		return s.fail(fmt.Errorf("openaichat: conflicting tool name at index %d", block.toolIndex))
	}
	tool := streamToolDelta{Index: block.toolIndex, Function: streamFunctionDelta{Arguments: delta.PartialJSON}}
	if !block.identitySent {
		tool.ID, tool.Type, tool.Function.Name = block.id, "function", block.name
	}
	// Some upstreams identify a call on its first argument delta rather than
	// the block prototype. Emit that metadata once, never on later fragments.
	if block.id == "" && delta.ToolCallID != "" {
		block.id, tool.ID = delta.ToolCallID, delta.ToolCallID
	}
	if block.name == "" && delta.ToolCallName != "" {
		block.name, tool.Function.Name = delta.ToolCallName, delta.ToolCallName
	}
	block.identitySent = true
	if delta.Type == ir.DeltaToolArguments {
		block.argumentsReceived = true
	}
	return s.writeChunk(streamDelta{ToolCalls: []streamToolDelta{tool}}, "", false)
}

func (s *streamState) updateUsage(usage *ir.Usage) {
	if usage == nil {
		return
	}
	// IR zeros mean unreported, so an output-only update must not erase the
	// input/cache accounting already supplied by message_start.
	if usage.InputTokens != 0 {
		s.usage.InputTokens = usage.InputTokens
	}
	if usage.OutputTokens != 0 {
		s.usage.OutputTokens = usage.OutputTokens
	}
	if usage.CacheReadTokens != 0 {
		s.usage.CacheReadTokens = usage.CacheReadTokens
	}
	if usage.CacheWriteTokens != 0 {
		s.usage.CacheWriteTokens = usage.CacheWriteTokens
	}
	if usage.ReasoningTokens != 0 {
		s.usage.ReasoningTokens = usage.ReasoningTokens
	}
}

func (s *streamState) writeChunk(delta streamDelta, finish string, usageOnly bool) error {
	chunk := streamChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model}
	if usageOnly {
		chunk.Choices = []streamChoice{}
		usage := streamUsage{PromptTokens: s.usage.InputTokens, CompletionTokens: s.usage.OutputTokens, TotalTokens: s.usage.InputTokens + s.usage.OutputTokens}
		if s.usage.CacheReadTokens != 0 || s.usage.CacheWriteTokens != 0 {
			usage.PromptDetails = &streamPromptDetails{CachedTokens: s.usage.CacheReadTokens, CacheCreationTokens: s.usage.CacheWriteTokens}
		}
		if s.usage.ReasoningTokens != 0 {
			usage.CompletionDetails = &streamCompletionDetails{ReasoningTokens: s.usage.ReasoningTokens}
		}
		chunk.Usage = &usage
	} else {
		choice := streamChoice{Index: 0, Delta: delta}
		if finish != "" {
			choice.FinishReason = &finish
		}
		chunk.Choices = []streamChoice{choice}
		if s.encoder.IncludeUsage {
			// A non-nil interface containing a nil pointer encodes JSON null,
			// while a nil interface omits usage when it was not requested.
			chunk.Usage = (*streamUsage)(nil)
		}
	}
	body, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return s.writeData(string(body))
}

func (s *streamState) fail(cause error) error {
	if s.ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
		// Preserve the upstream cause even if the recovery write also fails.
		return errors.Join(cause, s.writeError(&ir.StreamError{Kind: string(gwerr.KindFromErr(cause)), Message: cause.Error()}))
	}
	return cause
}

func (s *streamState) writeError(streamErr *ir.StreamError) error {
	body, _, err := gwerr.Encode(gwerr.ProtocolOpenAIChat, &gwerr.Error{Kind: gwerr.Kind(streamErr.Kind), Message: streamErr.Message})
	if err != nil {
		return err
	}
	return s.writeData(string(body))
}

func (s *streamState) writeData(data string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return s.writer.WriteData(data)
}

func streamFinishReason(reason ir.StopReason) (string, error) {
	switch reason {
	case ir.StopEndTurn, ir.StopSequence:
		return "stop", nil
	case ir.StopMaxTokens:
		return "length", nil
	case ir.StopToolUse:
		return "tool_calls", nil
	case ir.StopContentFilter:
		return "content_filter", nil
	default:
		return "", fmt.Errorf("openaichat: unsupported stop reason %q", reason)
	}
}

type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   any            `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role      string            `json:"role,omitempty"`
	Content   *string           `json:"content,omitempty"`
	Reasoning *string           `json:"reasoning_content,omitempty"`
	ToolCalls []streamToolDelta `json:"tool_calls,omitempty"`
}

type streamToolDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function streamFunctionDelta `json:"function"`
}

type streamFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type streamUsage struct {
	PromptTokens      int                      `json:"prompt_tokens"`
	CompletionTokens  int                      `json:"completion_tokens"`
	TotalTokens       int                      `json:"total_tokens"`
	PromptDetails     *streamPromptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *streamCompletionDetails `json:"completion_tokens_details,omitempty"`
}

type streamPromptDetails struct {
	CachedTokens        int `json:"cached_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type streamCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}
