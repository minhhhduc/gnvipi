package openairesponses

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
		return nil, invalid("response is required")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			gatewayError := outbound.UpstreamError(resp, nil)
			gatewayError.Kind = gwerr.KindFromErr(err)
			gatewayError.Message = fmt.Sprintf("read Responses upstream response: %v", err)
			return nil, gatewayError
		}
		return nil, outbound.UpstreamError(resp, body)
	}
	reader := sse.NewReader(resp.Body)
	return &responsesStream{
		reader:     reader,
		body:       resp.Body,
		blocks:     make(map[int]*streamBlock),
		openBlocks: make(map[int]bool),
	}, nil
}

type streamBlock struct {
	index    int
	kind     ir.BlockType
	toolID   string
	toolName string
}

type responsesStream struct {
	reader *sse.Reader
	body   io.Closer

	pending    []ir.StreamEvent
	started    bool
	terminated bool
	blocks     map[int]*streamBlock
	openBlocks map[int]bool

	savedUsage   *ir.Usage
	usageEmitted bool
	hasToolCalls bool
}

func (s *responsesStream) Close() error {
	if s.reader != nil {
		s.reader.Close()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func (s *responsesStream) Next(ctx context.Context) (ir.StreamEvent, error) {
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

func (s *responsesStream) handleEOF() {
	if s.terminated {
		return
	}
	if s.started {
		for idx := range s.openBlocks {
			s.pending = append(s.pending, ir.NewBlockStop(idx))
		}
		s.openBlocks = make(map[int]bool)
		if s.savedUsage != nil && !s.usageEmitted {
			s.pending = append(s.pending, ir.NewUsageUpdate(*s.savedUsage))
			s.usageEmitted = true
		}
		stopReason := ir.StopEndTurn
		if s.hasToolCalls {
			stopReason = ir.StopToolUse
		}
		s.pending = append(s.pending, ir.NewMessageStop(stopReason))
	}
	s.terminated = true
}

func (s *responsesStream) ensureStarted(id, model string) {
	if s.started {
		return
	}
	if id == "" {
		id = "resp_stream"
	}
	if model == "" {
		model = "responses-model"
	}
	s.pending = append(s.pending, ir.StreamEvent{
		Type:    ir.EventMessageStart,
		Message: &ir.Message{ID: id, Role: ir.RoleAssistant, Model: model},
	})
	s.started = true
}

func (s *responsesStream) handleSSEEvent(event sse.Event) error {
	data := strings.TrimSpace(event.Data)
	if data == "" {
		return nil
	}

	var frame struct {
		Type        string `json:"type"`
		OutputIndex *int   `json:"output_index"`
		Delta       string `json:"delta"`
		Item        *struct {
			ID      string  `json:"id"`
			Type    string  `json:"type"`
			Role    string  `json:"role"`
			CallID  string  `json:"call_id"`
			Name    *string `json:"name"`
			Content *[]struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
		Response *struct {
			ID                string `json:"id"`
			Model             string `json:"model"`
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Usage *struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return fmt.Errorf("openairesponses: malformed SSE: %w", err)
	}

	typ := frame.Type
	if typ == "" {
		typ = event.Type
	}

	switch typ {
	case "response.created", "response.in_progress":
		if frame.Response != nil {
			s.ensureStarted(frame.Response.ID, frame.Response.Model)
		}

	case "response.output_item.added":
		s.ensureStarted("", "")
		if frame.OutputIndex != nil && frame.Item != nil {
			idx := *frame.OutputIndex
			sb := &streamBlock{index: idx}
			var prototype ir.Block
			prototype.Index = idx
			switch frame.Item.Type {
			case "message":
				sb.kind = ir.BlockText
				prototype.Type = ir.BlockText
			case "reasoning":
				sb.kind = ir.BlockReasoning
				prototype.Type = ir.BlockReasoning
				prototype.Reasoning = &ir.ReasoningBlock{}
			case "function_call":
				sb.kind = ir.BlockToolUse
				s.hasToolCalls = true
				sb.toolID = frame.Item.CallID
				name := ""
				if frame.Item.Name != nil {
					name = *frame.Item.Name
				}
				sb.toolName = name
				prototype.Type = ir.BlockToolUse
				prototype.ToolUse = &ir.ToolUse{
					ID:   frame.Item.CallID,
					Name: name,
				}
			default:
				sb.kind = ir.BlockType(frame.Item.Type)
				prototype.Type = sb.kind
			}
			s.blocks[idx] = sb
			s.openBlocks[idx] = true
			s.pending = append(s.pending, ir.NewBlockStart(idx, prototype))
		}

	case "response.output_text.delta":
		s.ensureStarted("", "")
		idx := 0
		if frame.OutputIndex != nil {
			idx = *frame.OutputIndex
		}
		if !s.openBlocks[idx] {
			sb := &streamBlock{index: idx, kind: ir.BlockText}
			s.blocks[idx] = sb
			s.openBlocks[idx] = true
			s.pending = append(s.pending, ir.NewBlockStart(idx, ir.Block{Type: ir.BlockText, Index: idx}))
		}
		s.pending = append(s.pending, ir.NewTextDelta(idx, frame.Delta))

	case "response.reasoning_summary_text.delta":
		s.ensureStarted("", "")
		idx := 0
		if frame.OutputIndex != nil {
			idx = *frame.OutputIndex
		}
		if !s.openBlocks[idx] {
			sb := &streamBlock{index: idx, kind: ir.BlockReasoning}
			s.blocks[idx] = sb
			s.openBlocks[idx] = true
			s.pending = append(s.pending, ir.NewBlockStart(idx, ir.Block{Type: ir.BlockReasoning, Index: idx, Reasoning: &ir.ReasoningBlock{}}))
		}
		s.pending = append(s.pending, ir.NewReasoningDelta(idx, frame.Delta))

	case "response.function_call_arguments.delta":
		s.ensureStarted("", "")
		idx := 0
		if frame.OutputIndex != nil {
			idx = *frame.OutputIndex
		}
		sb := s.blocks[idx]
		callID := ""
		if sb != nil {
			callID = sb.toolID
		}
		s.pending = append(s.pending, ir.NewToolArgumentsDelta(idx, callID, frame.Delta))

	case "response.output_item.done":
		if frame.OutputIndex != nil {
			idx := *frame.OutputIndex
			if s.openBlocks[idx] {
				s.pending = append(s.pending, ir.NewBlockStop(idx))
				delete(s.openBlocks, idx)
			}
		}

	case "response.completed", "response.incomplete":
		if frame.Response != nil {
			s.ensureStarted(frame.Response.ID, frame.Response.Model)
		} else {
			s.ensureStarted("", "")
		}
		for idx := range s.openBlocks {
			s.pending = append(s.pending, ir.NewBlockStop(idx))
		}
		s.openBlocks = make(map[int]bool)

		if frame.Response != nil && frame.Response.Usage != nil {
			u := ir.Usage{
				InputTokens:  frame.Response.Usage.InputTokens,
				OutputTokens: frame.Response.Usage.OutputTokens,
			}
			if frame.Response.Usage.InputTokensDetails != nil {
				u.CacheReadTokens = frame.Response.Usage.InputTokensDetails.CachedTokens
			}
			if frame.Response.Usage.OutputTokensDetails != nil {
				u.ReasoningTokens = frame.Response.Usage.OutputTokensDetails.ReasoningTokens
			}
			s.savedUsage = &u
		}
		if s.savedUsage != nil && !s.usageEmitted {
			s.pending = append(s.pending, ir.NewUsageUpdate(*s.savedUsage))
			s.usageEmitted = true
		}

		stopReason := ir.StopEndTurn
		if s.hasToolCalls {
			stopReason = ir.StopToolUse
		}
		if frame.Response != nil && frame.Response.Status == "incomplete" {
			stopReason = ir.StopMaxTokens
			if frame.Response.IncompleteDetails != nil {
				switch frame.Response.IncompleteDetails.Reason {
				case "content_filter":
					stopReason = ir.StopContentFilter
				case "max_output_tokens":
					stopReason = ir.StopMaxTokens
				}
			}
		}
		s.pending = append(s.pending, ir.NewMessageStop(stopReason))
		s.terminated = true

	case "response.failed", "error":
		message := "Upstream generation failed"
		kind := string(gwerr.KindUpstreamError)
		if frame.Response != nil && frame.Response.Error != nil && frame.Response.Error.Message != "" {
			message = frame.Response.Error.Message
			if frame.Response.Error.Code == "rate_limit_exceeded" {
				kind = string(gwerr.KindRateLimit)
			}
		} else if frame.Error != nil && frame.Error.Message != "" {
			message = frame.Error.Message
			if frame.Error.Code == "rate_limit_exceeded" {
				kind = string(gwerr.KindRateLimit)
			}
		}
		s.ensureStarted("", "")
		s.pending = append(s.pending, ir.NewErrorEvent(kind, message))
		s.terminated = true
	}

	return nil
}
