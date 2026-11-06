package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// StreamEncoder renders normalized events as Anthropic Messages SSE. Its zero
// value disables pings; a positive KeepaliveInterval sends ping events while
// waiting for the next upstream event. Encoders share no per-request state.
// It is independent of the JSON adapter and can be embedded by that adapter.
type StreamEncoder struct {
	KeepaliveInterval time.Duration
}

// EncodeStream leaves stream ownership with the caller: it never calls Close.
// It never pulls past a terminal event or writes after one, and returns the
// first write failure unchanged. Successfully delivered IR error events return
// nil; transport and malformed-stream failures emit a terminal error and return
// the cause. Cancellation emits nothing further. With pings enabled, the owned
// pending Next is canceled and joined before returning; Next must honor ctx.
func (e StreamEncoder) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	var cancel context.CancelFunc
	if e.KeepaliveInterval > 0 {
		ctx, cancel = context.WithCancel(ctx)
	}
	state := streamState{ctx: ctx, writer: w, blocks: make(map[int]*streamBlock)}
	next := func() streamReadResult {
		event, err := events.Next(ctx)
		return streamReadResult{event: event, err: err, readFailure: true}
	}
	var done chan struct{}
	defer func() {
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	}()

	if e.KeepaliveInterval > 0 {
		// Demand-driven reads avoid prefetching beyond terminal IR events and
		// avoid changing Next's cancellation context for each idle interval.
		requests := make(chan struct{})
		results := make(chan streamReadResult)
		done = make(chan struct{})
		timer := time.NewTimer(e.KeepaliveInterval)
		defer timer.Stop()
		go func() {
			defer close(done)
			for {
				select {
				case <-ctx.Done():
					return
				case <-requests:
				}
				event, err := events.Next(ctx)
				select {
				case <-ctx.Done():
					return
				case results <- streamReadResult{event: event, err: err, readFailure: true}:
				}
			}
		}()
		next = func() streamReadResult {
			select {
			case <-ctx.Done():
				return streamReadResult{err: ctx.Err()}
			case requests <- struct{}{}:
			}
			timer.Reset(e.KeepaliveInterval)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return streamReadResult{err: ctx.Err()}
				case result := <-results:
					return result
				case <-timer.C:
					// Prefer a ready event over a simultaneously due ping.
					select {
					case result := <-results:
						return result
					default:
					}
					if err := state.emit(streamFrame{Type: "ping"}); err != nil {
						return streamReadResult{err: err}
					}
					timer.Reset(e.KeepaliveInterval)
				}
			}
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result := next()
		if result.err != nil && !result.readFailure {
			return result.err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.err != nil {
			if errors.Is(result.err, io.EOF) {
				result.err = io.ErrUnexpectedEOF
			}
			return state.fail(result.err)
		}
		terminal, err := state.handle(&result.event)
		if terminal || err != nil {
			return err
		}
	}
}

type streamReadResult struct {
	event       ir.StreamEvent
	err         error
	readFailure bool
}

type streamState struct {
	ctx     context.Context
	writer  *sse.Writer
	started bool
	blocks  map[int]*streamBlock
	order   []int
	usage   ir.Usage
}

type streamBlock struct {
	wireIndex                   int
	kind                        ir.BlockType
	toolID, toolName, signature string
	initialArguments            string
	argumentsStarted            bool
	open                        bool
}

type streamFrame struct {
	Type         string         `json:"type"`
	Message      *streamMessage `json:"message,omitempty"`
	Index        *int           `json:"index,omitempty"`
	ContentBlock *streamContent `json:"content_block,omitempty"`
	Delta        any            `json:"delta,omitempty"`
	Usage        *streamUsage   `json:"usage,omitempty"`
}

type streamMessage struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         ir.Role         `json:"role"`
	Model        string          `json:"model"`
	Content      []streamContent `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        streamUsage     `json:"usage"`
}

type streamContent struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text,omitempty"`
	Thinking *string         `json:"thinking,omitempty"`
	ID       *string         `json:"id,omitempty"`
	Name     *string         `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type streamDelta struct {
	Type        string  `json:"type"`
	Text        *string `json:"text,omitempty"`
	Thinking    *string `json:"thinking,omitempty"`
	PartialJSON *string `json:"partial_json,omitempty"`
	Signature   *string `json:"signature,omitempty"`
}

type streamStopDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type streamUsage struct {
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     int  `json:"output_tokens"`
	CacheReadTokens  int  `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int  `json:"cache_creation_input_tokens,omitempty"`
}

func (s *streamState) emit(frame streamFrame) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return s.writer.WriteEvent(sse.Event{Type: frame.Type, Data: string(data)})
}

func (s *streamState) fail(cause error) error {
	if err := s.emitError(cause); err != nil {
		return err
	}
	return cause
}

func (s *streamState) emitError(cause error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var reported *gwerr.Error
	if !errors.As(cause, &reported) {
		reported = &gwerr.Error{Kind: gwerr.KindFromErr(cause), Message: cause.Error()}
	}
	data, _, err := gwerr.Encode(gwerr.ProtocolAnthropicMessages, reported)
	if err != nil {
		return err
	}
	return s.writer.WriteEvent(sse.Event{Type: "error", Data: string(data)})
}

func (s *streamState) invalid(format string, args ...any) error {
	return s.fail(fmt.Errorf("anthropic stream: "+format, args...))
}

func (s *streamState) handle(event *ir.StreamEvent) (bool, error) {
	if event.Type == ir.EventError {
		if event.Error == nil {
			return true, s.invalid("error event has no payload")
		}
		return true, s.emitError(&gwerr.Error{Kind: gwerr.Kind(event.Error.Kind), Message: event.Error.Message})
	}
	if event.Type == ir.EventMessageStart {
		if s.started {
			return true, s.invalid("duplicate message_start")
		}
		if event.Message == nil {
			return true, s.invalid("message_start has no message")
		}
		s.started = true
		s.mergeUsage(event.Usage)
		model := event.Message.Model
		if request := core.RequestContextFrom(s.ctx); request != nil && request.ClientModel != "" {
			model = request.ClientModel
		}
		role := event.Message.Role
		if role == "" {
			role = ir.RoleAssistant
		}
		usage := s.wireUsage(true)
		message := &streamMessage{ID: event.Message.ID, Type: "message", Role: role, Model: model, Content: []streamContent{}, Usage: usage}
		if err := s.emit(streamFrame{Type: "message_start", Message: message}); err != nil {
			return false, err
		}
		for _, block := range event.Message.Content {
			if err := s.startBlock(block.Index, &block); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if !s.started {
		return true, s.invalid("%s before message_start", event.Type)
	}

	switch event.Type {
	case ir.EventBlockStart:
		return false, s.startBlock(event.Index, event.Block)
	case ir.EventBlockDelta:
		block := s.blocks[event.Index]
		if block == nil || !block.open {
			return true, s.invalid("delta for unopened block %d", event.Index)
		}
		if event.Delta == nil {
			return true, s.invalid("block %d delta has no payload", event.Index)
		}
		delta := streamDelta{}
		switch event.Delta.Type {
		case ir.DeltaText:
			if block.kind != ir.BlockText {
				return true, s.invalid("text delta for %s block", block.kind)
			}
			delta.Type, delta.Text = "text_delta", &event.Delta.Text
		case ir.DeltaReasoning:
			if block.kind != ir.BlockReasoning {
				return true, s.invalid("thinking delta for %s block", block.kind)
			}
			delta.Type, delta.Thinking = "thinking_delta", &event.Delta.Text
		case ir.DeltaSignature:
			if block.kind != ir.BlockReasoning {
				return true, s.invalid("signature delta for %s block", block.kind)
			}
			block.signature = ""
			delta.Type, delta.Signature = "signature_delta", &event.Delta.Signature
		case ir.DeltaToolArguments:
			if block.kind != ir.BlockToolUse {
				return true, s.invalid("tool delta for %s block", block.kind)
			}
			if event.Delta.ToolCallID != "" && event.Delta.ToolCallID != block.toolID {
				return true, s.invalid("tool delta identity does not match block %d", event.Index)
			}
			if event.Delta.ToolCallName != "" && event.Delta.ToolCallName != block.toolName {
				return true, s.invalid("tool delta name does not match block %d", event.Index)
			}
			block.argumentsStarted = true
			delta.Type, delta.PartialJSON = "input_json_delta", &event.Delta.PartialJSON
		default:
			return true, s.invalid("unsupported delta %q", event.Delta.Type)
		}
		return false, s.emit(streamFrame{Type: "content_block_delta", Index: &block.wireIndex, Delta: delta})
	case ir.EventBlockStop:
		block := s.blocks[event.Index]
		if block == nil || !block.open {
			return true, s.invalid("stop for unopened block %d", event.Index)
		}
		return false, s.stopBlock(block)
	case ir.EventUsageUpdate:
		if event.Usage == nil {
			return true, s.invalid("usage_update has no usage")
		}
		s.mergeUsage(event.Usage)
		return false, nil
	case ir.EventMessageStop:
		if event.StopReason == ir.StopError {
			return true, s.emitError(&gwerr.Error{Kind: gwerr.KindUpstreamError, Message: "upstream generation stopped with an error"})
		}
		reason := string(event.StopReason)
		switch event.StopReason {
		case ir.StopEndTurn, ir.StopMaxTokens, ir.StopSequence, ir.StopToolUse:
		case ir.StopContentFilter:
			reason = "refusal"
		default:
			return true, s.invalid("unsupported stop reason %q", event.StopReason)
		}
		s.mergeUsage(event.Usage)
		for _, index := range s.order {
			if block := s.blocks[index]; block.open {
				if err := s.stopBlock(block); err != nil {
					return true, err
				}
			}
		}
		usage := s.wireUsage(false)
		if err := s.emit(streamFrame{Type: "message_delta", Delta: streamStopDelta{StopReason: reason}, Usage: &usage}); err != nil {
			return true, err
		}
		return true, s.emit(streamFrame{Type: "message_stop"})
	default:
		return true, s.invalid("unsupported event %q", event.Type)
	}
}

func (s *streamState) startBlock(index int, prototype *ir.Block) error {
	if index < 0 {
		return s.invalid("negative block index %d", index)
	}
	if s.blocks[index] != nil {
		return s.invalid("duplicate or reopened block %d", index)
	}
	if prototype == nil {
		return s.invalid("block %d has no prototype", index)
	}
	block := &streamBlock{wireIndex: len(s.order), kind: prototype.Type, open: true}
	content := &streamContent{}
	empty := ""
	var initial *streamDelta
	switch prototype.Type {
	case ir.BlockText:
		content.Type, content.Text = "text", &empty
		if prototype.Text != "" {
			initial = &streamDelta{Type: "text_delta", Text: &prototype.Text}
		}
	case ir.BlockReasoning:
		content.Type, content.Thinking = "thinking", &empty
		if prototype.Reasoning != nil {
			block.signature = prototype.Reasoning.Signature
			if prototype.Reasoning.Content != "" {
				initial = &streamDelta{Type: "thinking_delta", Thinking: &prototype.Reasoning.Content}
			}
		}
	case ir.BlockToolUse:
		if prototype.ToolUse == nil {
			return s.invalid("tool block %d has no tool", index)
		}
		tool := prototype.ToolUse
		if tool.ID == "" || tool.Name == "" {
			return s.invalid("tool block %d has no ID or name", index)
		}
		if len(tool.Input) != 0 && !json.Valid(tool.Input) {
			return s.invalid("tool block %d has invalid initial JSON", index)
		}
		block.toolID, block.toolName = tool.ID, tool.Name
		content.Type, content.ID, content.Name, content.Input = "tool_use", &tool.ID, &tool.Name, json.RawMessage("{}")
		block.initialArguments = string(tool.Input)
		// Decode only a JSON-string wrapper, leaving object bytes and precision
		// untouched. Leading JSON whitespace does not change the wrapper type.
		if input := bytes.TrimSpace(tool.Input); len(input) != 0 && input[0] == '"' {
			if err := json.Unmarshal(tool.Input, &block.initialArguments); err != nil {
				return s.invalid("tool block %d has invalid argument string: %v", index, err)
			}
		}
	default:
		return s.invalid("unsupported block type %q", prototype.Type)
	}
	s.blocks[index] = block
	s.order = append(s.order, index)
	if err := s.emit(streamFrame{Type: "content_block_start", Index: &block.wireIndex, ContentBlock: content}); err != nil {
		return err
	}
	if initial != nil {
		return s.emit(streamFrame{Type: "content_block_delta", Index: &block.wireIndex, Delta: initial})
	}
	return nil
}

func (s *streamState) stopBlock(block *streamBlock) error {
	// A prototype may just be {} before real fragments arrive. Emit it only
	// when completion proves there were no fragments; argument-less calls keep
	// the valid empty input object already supplied by content_block_start.
	if block.kind == ir.BlockToolUse && !block.argumentsStarted && block.initialArguments != "" {
		if err := s.emit(streamFrame{Type: "content_block_delta", Index: &block.wireIndex, Delta: streamDelta{Type: "input_json_delta", PartialJSON: &block.initialArguments}}); err != nil {
			return err
		}
	}
	if block.signature != "" {
		if err := s.emit(streamFrame{Type: "content_block_delta", Index: &block.wireIndex, Delta: streamDelta{Type: "signature_delta", Signature: &block.signature}}); err != nil {
			return err
		}
	}
	if err := s.emit(streamFrame{Type: "content_block_stop", Index: &block.wireIndex}); err != nil {
		return err
	}
	block.open = false
	return nil
}

func (s *streamState) mergeUsage(usage *ir.Usage) {
	if usage == nil {
		return
	}
	// IR zero counters are unreported, not resets. Later output-only usage
	// must not erase the input/cache accounting learned at message_start.
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
}

func (s *streamState) wireUsage(start bool) streamUsage {
	usage := streamUsage{OutputTokens: s.usage.OutputTokens, CacheReadTokens: s.usage.CacheReadTokens, CacheWriteTokens: s.usage.CacheWriteTokens}
	if start || s.usage.InputTokens != 0 {
		usage.InputTokens = &s.usage.InputTokens
	}
	return usage
}
