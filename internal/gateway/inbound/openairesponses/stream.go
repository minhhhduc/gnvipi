package openairesponses

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// StreamEncoder encodes a normalized generation as Responses SSE. Created is
// the Unix creation timestamp; zero captures the current time once per stream.
// All generation state is local, so an encoder can serve concurrent requests.
type StreamEncoder struct {
	Created int64
}

// EncodeStream consumes events through the first terminal event. The caller
// retains ownership of the upstream stream. Provider errors are reported on the
// wire; transport, cancellation and client write errors are also returned.
func (e StreamEncoder) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	state := responseStream{
		ctx: ctx, writer: w, blocks: make(map[int]*responseStreamBlock),
		response: responseStreamResponse{Object: "response", CreatedAt: e.Created, Status: "in_progress", Output: []*responseStreamItem{}},
	}
	if state.response.CreatedAt == 0 {
		state.response.CreatedAt = time.Now().Unix()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		event, err := events.Next(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			if !state.started {
				if writeErr := state.start(nil, nil); writeErr != nil {
					return writeErr
				}
			}
			if writeErr := state.fail("server_error", err.Error()); writeErr != nil {
				return writeErr
			}
			return err
		}
		if !state.started {
			if event.Type != ir.EventMessageStart && event.Type != ir.EventError {
				return fmt.Errorf("openairesponses: stream must begin with message_start")
			}
			if event.Type == ir.EventMessageStart {
				if event.Message == nil {
					return fmt.Errorf("openairesponses: message_start has no message")
				}
				if err := state.start(event.Message, event.Usage); err != nil {
					return err
				}
				for _, block := range event.Message.Content {
					if err := state.addBlock(block.Index, block); err != nil {
						return state.invalid(err)
					}
					// Initial message content is complete, not an open streamed group.
					if err := state.finishBlock(state.blocks[block.Index], "completed"); err != nil {
						return err
					}
				}
				continue
			}
			if err := state.start(nil, nil); err != nil {
				return err
			}
		}
		switch event.Type {
		case ir.EventBlockStart:
			if event.Block == nil {
				return state.invalid(fmt.Errorf("openairesponses: block_start has no block"))
			}
			if err := state.addBlock(event.Index, *event.Block); err != nil {
				return state.invalid(err)
			}
		case ir.EventBlockDelta:
			block := state.blocks[event.Index]
			if block == nil || block.closed || event.Delta == nil {
				return state.invalid(fmt.Errorf("openairesponses: delta references unopened or completed block %d", event.Index))
			}
			if err := state.delta(block, event.Delta); err != nil {
				return state.invalid(err)
			}
		case ir.EventBlockStop:
			block := state.blocks[event.Index]
			if block == nil || block.closed {
				return state.invalid(fmt.Errorf("openairesponses: stop references unopened or completed block %d", event.Index))
			}
			if err := state.finishBlock(block, "completed"); err != nil {
				return err
			}
		case ir.EventUsageUpdate:
			if event.Usage == nil {
				return state.invalid(fmt.Errorf("openairesponses: usage_update has no usage"))
			}
			state.mergeUsage(*event.Usage)
		case ir.EventError:
			if event.Error == nil {
				return state.fail("server_error", "Upstream generation failed")
			}
			code := "server_error"
			switch event.Error.Kind {
			case "rate_limit":
				code = "rate_limit_exceeded"
			case "invalid_request":
				code = "invalid_prompt"
			}
			return state.fail(code, event.Error.Message)
		case ir.EventMessageStop:
			switch event.StopReason {
			case ir.StopEndTurn, ir.StopSequence, ir.StopToolUse:
				state.response.Status = "completed"
			case ir.StopMaxTokens:
				state.response.Status = "incomplete"
				state.response.IncompleteDetails = &responseStreamIncomplete{Reason: "max_output_tokens"}
			case ir.StopContentFilter:
				state.response.Status = "incomplete"
				state.response.IncompleteDetails = &responseStreamIncomplete{Reason: "content_filter"}
			case ir.StopError:
				return state.fail("server_error", "Upstream generation failed")
			default:
				return state.invalid(fmt.Errorf("openairesponses: unsupported stop reason %q", event.StopReason))
			}
			for _, block := range state.outputBlocks {
				if !block.closed {
					return state.invalid(fmt.Errorf("openairesponses: message_stop before block %d completed", block.index))
				}
			}
			state.setUsage()
			return state.emit("response."+state.response.Status, map[string]any{"response": &state.response})
		default:
			return state.invalid(fmt.Errorf("openairesponses: unexpected stream event %q", event.Type))
		}
	}
}

type responseStreamResponse struct {
	ID                string                    `json:"id"`
	Object            string                    `json:"object"`
	CreatedAt         int64                     `json:"created_at"`
	Status            string                    `json:"status"`
	Error             *responseStreamError      `json:"error"`
	IncompleteDetails *responseStreamIncomplete `json:"incomplete_details"`
	Model             string                    `json:"model"`
	Output            []*responseStreamItem     `json:"output"`
	Usage             *responseStreamUsage      `json:"usage"`
}

type responseStreamError struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

type responseStreamIncomplete struct {
	Reason string `json:"reason"`
}

type responseStreamUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  responseStreamInputDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails responseStreamOutputDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type responseStreamInputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type responseStreamOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responseStreamItem struct {
	ID               string                `json:"id"`
	Type             string                `json:"type"`
	Status           string                `json:"status"`
	Role             string                `json:"role,omitempty"`
	Content          *[]responseStreamPart `json:"content,omitempty"`
	Summary          *[]responseStreamPart `json:"summary,omitempty"`
	CallID           string                `json:"call_id,omitempty"`
	Name             *string               `json:"name,omitempty"`
	Arguments        *string               `json:"arguments,omitempty"`
	EncryptedContent string                `json:"encrypted_content,omitempty"`
}

type responseStreamPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations *[]any `json:"annotations,omitempty"`
	Logprobs    *[]any `json:"logprobs,omitempty"`
}

type responseStreamBlock struct {
	index            int
	outputIndex      int
	kind             ir.BlockType
	item             *responseStreamItem
	text             strings.Builder
	initialArguments string
	argumentsStarted bool
	closed           bool
}

type responseStream struct {
	ctx          context.Context
	writer       *sse.Writer
	sequence     int64
	started      bool
	writeErr     error
	response     responseStreamResponse
	usage        ir.Usage
	blocks       map[int]*responseStreamBlock
	outputBlocks []*responseStreamBlock
}

func (s *responseStream) emit(typ string, fields map[string]any) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	fields["type"] = typ
	fields["sequence_number"] = s.sequence
	body, err := json.Marshal(fields)
	if err != nil {
		s.writeErr = err
		return err
	}
	if err := s.writer.WriteEvent(sse.Event{Type: typ, Data: string(body)}); err != nil {
		s.writeErr = err
		return err
	}
	s.sequence++
	return nil
}

func (s *responseStream) start(message *ir.Message, usage *ir.Usage) error {
	if message != nil {
		s.response.ID, s.response.Model = message.ID, message.Model
	}
	if s.response.ID == "" {
		s.response.ID = "resp_" + rand.Text()
	}
	if usage != nil {
		s.mergeUsage(*usage)
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{"response": &s.response}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": &s.response})
}

func (s *responseStream) addBlock(index int, prototype ir.Block) error {
	if index < 0 || s.blocks[index] != nil {
		return fmt.Errorf("openairesponses: invalid or duplicate block index %d", index)
	}
	outputIndex := len(s.response.Output)
	item := &responseStreamItem{Status: "in_progress"}
	block := &responseStreamBlock{index: index, outputIndex: outputIndex, kind: prototype.Type, item: item}
	prefix := ""
	switch prototype.Type {
	case ir.BlockText:
		prefix, item.Type, item.Role = "msg_", "message", "assistant"
		item.Content = &[]responseStreamPart{}
	case ir.BlockReasoning:
		if prototype.Reasoning == nil {
			return fmt.Errorf("openairesponses: reasoning block %d has no payload", index)
		}
		prefix, item.Type = "rs_", "reasoning"
		item.Summary = &[]responseStreamPart{}
		item.EncryptedContent = prototype.Reasoning.Signature
	case ir.BlockToolUse:
		if prototype.ToolUse == nil || prototype.ToolUse.ID == "" {
			return fmt.Errorf("openairesponses: tool block %d has no call identity", index)
		}
		prefix, item.Type = "fc_", "function_call"
		item.CallID = prototype.ToolUse.ID
		name, arguments := prototype.ToolUse.Name, ""
		item.Name, item.Arguments = &name, &arguments
		block.initialArguments = string(prototype.ToolUse.Input)
		// Some upstreams carry already-encoded argument strings in RawMessage.
		// Decode only that wrapper, never parse or reserialize the JSON arguments.
		input := bytes.TrimSpace(prototype.ToolUse.Input)
		if len(prototype.ToolUse.Input) == 0 {
			block.initialArguments = "{}"
		} else if len(input) != 0 && input[0] == '"' {
			if err := json.Unmarshal(prototype.ToolUse.Input, &block.initialArguments); err != nil {
				return fmt.Errorf("openairesponses: invalid tool argument string: %w", err)
			}
		}
	default:
		return fmt.Errorf("openairesponses: unsupported output block type %q", prototype.Type)
	}
	item.ID = fmt.Sprintf("%s%s_%d", prefix, s.response.ID, outputIndex)
	s.blocks[index] = block
	s.outputBlocks = append(s.outputBlocks, block)
	s.response.Output = append(s.response.Output, item)
	if err := s.emit("response.output_item.added", map[string]any{"output_index": outputIndex, "item": item}); err != nil {
		return err
	}
	switch prototype.Type {
	case ir.BlockText:
		part := newResponseStreamTextPart("")
		*item.Content = append(*item.Content, part)
		fields := block.fields()
		fields["content_index"], fields["part"] = 0, part
		if err := s.emit("response.content_part.added", fields); err != nil {
			return err
		}
		if prototype.Text != "" {
			return s.delta(block, &ir.Delta{Type: ir.DeltaText, Text: prototype.Text})
		}
	case ir.BlockReasoning:
		part := responseStreamPart{Type: "summary_text", Text: ""}
		*item.Summary = append(*item.Summary, part)
		fields := block.fields()
		fields["summary_index"], fields["part"] = 0, part
		if err := s.emit("response.reasoning_summary_part.added", fields); err != nil {
			return err
		}
		if prototype.Reasoning.Content != "" {
			return s.delta(block, &ir.Delta{Type: ir.DeltaReasoning, Text: prototype.Reasoning.Content})
		}
	}
	return nil
}

func (b *responseStreamBlock) fields() map[string]any {
	return map[string]any{"item_id": b.item.ID, "output_index": b.outputIndex}
}

func (s *responseStream) delta(block *responseStreamBlock, delta *ir.Delta) error {
	fields := block.fields()
	var typ, fragment string
	switch {
	case block.kind == ir.BlockText && delta.Type == ir.DeltaText:
		typ, fragment = "response.output_text.delta", delta.Text
		fields["content_index"], fields["logprobs"] = 0, []any{}
	case block.kind == ir.BlockReasoning && delta.Type == ir.DeltaReasoning:
		typ, fragment = "response.reasoning_summary_text.delta", delta.Text
		fields["summary_index"] = 0
	case block.kind == ir.BlockReasoning && delta.Type == ir.DeltaSignature:
		return nil
	case block.kind == ir.BlockToolUse && delta.Type == ir.DeltaToolArguments:
		if delta.ToolCallID != "" && delta.ToolCallID != block.item.CallID {
			return fmt.Errorf("openairesponses: tool call identity changed in block %d", block.index)
		}
		if delta.ToolCallName != "" {
			if *block.item.Name != "" && *block.item.Name != delta.ToolCallName {
				return fmt.Errorf("openairesponses: tool call name changed in block %d", block.index)
			}
			*block.item.Name = delta.ToolCallName
		}
		typ, fragment = "response.function_call_arguments.delta", delta.PartialJSON
		block.argumentsStarted = true
	default:
		return fmt.Errorf("openairesponses: delta %q does not match block %d (%s)", delta.Type, block.index, block.kind)
	}
	block.text.WriteString(fragment)
	fields["delta"] = fragment
	return s.emit(typ, fields)
}

func newResponseStreamTextPart(text string) responseStreamPart {
	annotations, logprobs := []any{}, []any{}
	return responseStreamPart{Type: "output_text", Text: text, Annotations: &annotations, Logprobs: &logprobs}
}

func (s *responseStream) finishBlock(block *responseStreamBlock, status string) error {
	fields := block.fields()
	switch block.kind {
	case ir.BlockText:
		text := block.text.String()
		part := newResponseStreamTextPart(text)
		(*block.item.Content)[0] = part
		fields["content_index"], fields["text"], fields["logprobs"] = 0, text, []any{}
		if err := s.emit("response.output_text.done", fields); err != nil {
			return err
		}
		fields = block.fields()
		fields["content_index"], fields["part"] = 0, part
		if err := s.emit("response.content_part.done", fields); err != nil {
			return err
		}
	case ir.BlockReasoning:
		text := block.text.String()
		part := responseStreamPart{Type: "summary_text", Text: text}
		(*block.item.Summary)[0] = part
		fields["summary_index"], fields["text"] = 0, text
		if err := s.emit("response.reasoning_summary_text.done", fields); err != nil {
			return err
		}
		fields = block.fields()
		fields["summary_index"], fields["part"] = 0, part
		if err := s.emit("response.reasoning_summary_part.done", fields); err != nil {
			return err
		}
	case ir.BlockToolUse:
		// A block prototype is not an argument delta. Preserve its payload
		// only when the upstream never sent streamed arguments (often a
		// prototype contains the placeholder object {} before real deltas).
		if !block.argumentsStarted && block.initialArguments != "" {
			if err := s.delta(block, &ir.Delta{Type: ir.DeltaToolArguments, PartialJSON: block.initialArguments}); err != nil {
				return err
			}
		}
		arguments := block.text.String()
		block.item.Arguments = &arguments
		fields["arguments"] = arguments
		if err := s.emit("response.function_call_arguments.done", fields); err != nil {
			return err
		}
	}
	block.item.Status = status
	if err := s.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": block.item}); err != nil {
		return err
	}
	block.closed = true
	return nil
}

func (s *responseStream) mergeUsage(usage ir.Usage) {
	// IR zero means unreported. Updates carry cumulative counts, not deltas,
	// and may report output only, leaving initial input/cache usage intact.
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

func (s *responseStream) setUsage() {
	s.response.Usage = &responseStreamUsage{
		InputTokens:         s.usage.InputTokens,
		InputTokensDetails:  responseStreamInputDetails{CachedTokens: s.usage.CacheReadTokens, CacheWriteTokens: s.usage.CacheWriteTokens},
		OutputTokens:        s.usage.OutputTokens,
		OutputTokensDetails: responseStreamOutputDetails{ReasoningTokens: s.usage.ReasoningTokens},
		TotalTokens:         s.usage.InputTokens + s.usage.OutputTokens,
	}
}

func (s *responseStream) fail(code, message string) error {
	for _, block := range s.outputBlocks {
		if !block.closed {
			if err := s.finishBlock(block, "incomplete"); err != nil {
				return err
			}
		}
	}
	s.response.Status = "failed"
	s.response.IncompleteDetails = nil
	s.response.Error = &responseStreamError{Code: code, Message: gwerr.Scrub(message)}
	s.setUsage()
	if err := s.emit("error", map[string]any{"code": code, "message": s.response.Error.Message, "param": nil}); err != nil {
		return err
	}
	return s.emit("response.failed", map[string]any{"response": &s.response})
}

func (s *responseStream) invalid(err error) error {
	if s.writeErr != nil || s.ctx.Err() != nil {
		return err
	}
	if writeErr := s.fail("server_error", err.Error()); writeErr != nil {
		return writeErr
	}
	return err
}
