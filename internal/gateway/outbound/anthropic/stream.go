package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

func parseStream(_ context.Context, resp *http.Response) (adapter.Stream, error) {
	if resp == nil || resp.Body == nil {
		return nil, invalidRequest("Anthropic response is required")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			gatewayError := outbound.UpstreamError(resp, nil)
			gatewayError.Kind = gwerr.KindFromErr(err)
			gatewayError.Message = fmt.Sprintf("read Anthropic upstream response: %v", err)
			return nil, gatewayError
		}
		return nil, outbound.UpstreamError(resp, body)
	}
	reader := sse.NewReader(resp.Body)
	return &anthropicStream{
		reader: reader,
		body:   resp.Body,
		blocks: make(map[int]*streamBlock),
	}, nil
}

type streamBlock struct {
	index     int
	kind      ir.BlockType
	toolID    string
	toolName  string
	signature string
}

type anthropicStream struct {
	reader *sse.Reader
	body   io.Closer

	pending      []ir.StreamEvent
	started      bool
	terminated   bool
	blocks       map[int]*streamBlock
	openBlocks   map[int]bool
	mergedUsage  ir.Usage
	usageEmitted bool

	savedStopReason ir.StopReason
}

func (s *anthropicStream) Close() error {
	if s.reader != nil {
		s.reader.Close()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func (s *anthropicStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	for len(s.pending) == 0 {
		if s.terminated {
			return ir.StreamEvent{}, io.EOF
		}
		event, err := s.reader.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.handleEOF()
				continue
			}
			return ir.StreamEvent{}, err
		}
		if err := s.handleSSEEvent(event); err != nil {
			return ir.StreamEvent{}, err
		}
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

func (s *anthropicStream) handleEOF() {
	if s.terminated {
		return
	}
	if s.started {
		if !s.usageEmitted {
			if s.mergedUsage.InputTokens != 0 || s.mergedUsage.OutputTokens != 0 || s.mergedUsage.CacheReadTokens != 0 || s.mergedUsage.CacheWriteTokens != 0 {
				s.pending = append(s.pending, ir.NewUsageUpdate(s.mergedUsage))
				s.usageEmitted = true
			}
		}
		reason := s.savedStopReason
		if reason == "" {
			reason = ir.StopEndTurn
		}
		s.pending = append(s.pending, ir.NewMessageStop(reason))
	}
	s.terminated = true
}

func (s *anthropicStream) handleSSEEvent(event sse.Event) error {
	data := strings.TrimSpace(event.Data)
	if data == "" || event.Type == "ping" {
		return nil
	}

	var frame struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Role  string `json:"role"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Index        *int `json:"index"`
		ContentBlock *struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			ID       string `json:"id"`
			Name     string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			Thinking     string `json:"thinking"`
			Signature    string `json:"signature"`
			PartialJSON  string `json:"partial_json"`
			StopReason   string `json:"stop_reason"`
			StopSequence string `json:"stop_sequence"`
		} `json:"delta"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return fmt.Errorf("anthropic: malformed SSE event: %w", err)
	}

	typ := frame.Type
	if typ == "" {
		typ = event.Type
	}

	switch typ {
	case "ping":
		return nil

	case "message_start":
		if frame.Message == nil {
			return errors.New("anthropic: message_start missing message payload")
		}
		s.mergedUsage.InputTokens = frame.Message.Usage.InputTokens
		s.mergedUsage.CacheReadTokens = frame.Message.Usage.CacheReadInputTokens
		s.mergedUsage.CacheWriteTokens = frame.Message.Usage.CacheCreationInputTokens
		startEv := ir.StreamEvent{
			Type: ir.EventMessageStart,
			Message: &ir.Message{
				ID:    frame.Message.ID,
				Role:  ir.RoleAssistant,
				Model: frame.Message.Model,
			},
		}
		if s.mergedUsage.InputTokens != 0 || s.mergedUsage.CacheReadTokens != 0 || s.mergedUsage.CacheWriteTokens != 0 {
			u := s.mergedUsage
			startEv.Usage = &u
		}
		s.pending = append(s.pending, startEv)
		s.started = true

	case "content_block_start":
		if frame.Index == nil || frame.ContentBlock == nil {
			return errors.New("anthropic: content_block_start missing index or content_block")
		}
		idx := *frame.Index
		sb := &streamBlock{index: idx}
		var prototype ir.Block
		prototype.Index = idx
		switch frame.ContentBlock.Type {
		case "text":
			sb.kind = ir.BlockText
			prototype.Type = ir.BlockText
		case "thinking":
			sb.kind = ir.BlockReasoning
			prototype.Type = ir.BlockReasoning
			prototype.Reasoning = &ir.ReasoningBlock{Content: frame.ContentBlock.Thinking}
		case "tool_use":
			sb.kind = ir.BlockToolUse
			sb.toolID = frame.ContentBlock.ID
			sb.toolName = frame.ContentBlock.Name
			prototype.Type = ir.BlockToolUse
			prototype.ToolUse = &ir.ToolUse{
				ID:   frame.ContentBlock.ID,
				Name: frame.ContentBlock.Name,
			}
		default:
			sb.kind = ir.BlockType(frame.ContentBlock.Type)
			prototype.Type = sb.kind
		}
		s.blocks[idx] = sb
		if s.openBlocks == nil {
			s.openBlocks = make(map[int]bool)
		}
		s.openBlocks[idx] = true
		s.pending = append(s.pending, ir.NewBlockStart(idx, prototype))

	case "content_block_delta":
		if frame.Index == nil || frame.Delta == nil {
			return errors.New("anthropic: content_block_delta missing index or delta")
		}
		idx := *frame.Index
		sb := s.blocks[idx]
		switch frame.Delta.Type {
		case "text_delta":
			s.pending = append(s.pending, ir.NewTextDelta(idx, frame.Delta.Text))
		case "thinking_delta":
			s.pending = append(s.pending, ir.NewReasoningDelta(idx, frame.Delta.Thinking))
		case "signature_delta":
			if sb != nil {
				sb.signature = frame.Delta.Signature
			}
			s.pending = append(s.pending, ir.NewSignatureDelta(idx, frame.Delta.Signature))
		case "input_json_delta":
			toolID := ""
			if sb != nil {
				toolID = sb.toolID
			}
			s.pending = append(s.pending, ir.NewToolArgumentsDelta(idx, toolID, frame.Delta.PartialJSON))
		}

	case "content_block_stop":
		if frame.Index == nil {
			return errors.New("anthropic: content_block_stop missing index")
		}
		idx := *frame.Index
		if s.openBlocks != nil {
			delete(s.openBlocks, idx)
		}
		s.pending = append(s.pending, ir.NewBlockStop(idx))

	case "message_delta":
		if frame.Delta != nil && frame.Delta.StopReason != "" {
			switch frame.Delta.StopReason {
			case "end_turn":
				s.savedStopReason = ir.StopEndTurn
			case "max_tokens":
				s.savedStopReason = ir.StopMaxTokens
			case "stop_sequence":
				s.savedStopReason = ir.StopSequence
			case "tool_use":
				s.savedStopReason = ir.StopToolUse
			default:
				s.savedStopReason = ir.StopReason(frame.Delta.StopReason)
			}
		}
		if frame.Usage != nil {
			if frame.Usage.InputTokens != 0 {
				s.mergedUsage.InputTokens = frame.Usage.InputTokens
			}
			if frame.Usage.OutputTokens != 0 {
				s.mergedUsage.OutputTokens = frame.Usage.OutputTokens
			}
			if frame.Usage.CacheReadInputTokens != 0 {
				s.mergedUsage.CacheReadTokens = frame.Usage.CacheReadInputTokens
			}
			if frame.Usage.CacheCreationInputTokens != 0 {
				s.mergedUsage.CacheWriteTokens = frame.Usage.CacheCreationInputTokens
			}
		}

	case "message_stop":
		for idx := range s.openBlocks {
			s.pending = append(s.pending, ir.NewBlockStop(idx))
		}
		s.openBlocks = make(map[int]bool)
		if !s.usageEmitted {
			if s.mergedUsage.InputTokens != 0 || s.mergedUsage.OutputTokens != 0 || s.mergedUsage.CacheReadTokens != 0 || s.mergedUsage.CacheWriteTokens != 0 {
				s.pending = append(s.pending, ir.NewUsageUpdate(s.mergedUsage))
				s.usageEmitted = true
			}
		}
		reason := s.savedStopReason
		if reason == "" {
			reason = ir.StopEndTurn
		}
		s.pending = append(s.pending, ir.NewMessageStop(reason))
		s.terminated = true

	case "error":
		if frame.Error == nil {
			return errors.New("anthropic: error event missing error payload")
		}
		kind := string(gwerr.KindUpstreamError)
		switch frame.Error.Type {
		case "overloaded_error":
			kind = string(gwerr.KindOverloaded)
		case "rate_limit_error":
			kind = string(gwerr.KindRateLimit)
		case "invalid_request_error":
			kind = string(gwerr.KindInvalidRequest)
		}
		if !s.started {
			s.pending = append(s.pending, ir.StreamEvent{
				Type:    ir.EventMessageStart,
				Message: &ir.Message{ID: "err_msg", Role: ir.RoleAssistant, Model: "unknown"},
			})
			s.started = true
		}
		s.pending = append(s.pending, ir.NewErrorEvent(kind, frame.Error.Message))
		s.terminated = true
	}

	return nil
}
