package openaichat

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
		return nil, invalid("OpenAI Chat response is required")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			gatewayError := outbound.UpstreamError(resp, nil)
			gatewayError.Kind = gwerr.KindFromErr(err)
			gatewayError.Message = fmt.Sprintf("read OpenAI Chat upstream response: %v", err)
			return nil, gatewayError
		}
		return nil, outbound.UpstreamError(resp, body)
	}
	reader := sse.NewReader(resp.Body)
	return &chatStream{
		reader:     reader,
		body:       resp.Body,
		toolBlocks: make(map[int]int),
	}, nil
}

type chatStream struct {
	reader *sse.Reader
	body   io.Closer

	pending           []ir.StreamEvent
	started           bool
	terminated        bool
	openBlock         bool
	currentBlockIndex int
	currentBlockType  ir.BlockType
	toolBlocks        map[int]int
	nextBlockIndex    int

	savedStopReason ir.StopReason
	finalUsage      *ir.Usage
	usageEmitted    bool
}

func (s *chatStream) Close() error {
	if s.reader != nil {
		s.reader.Close()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func (s *chatStream) Next(ctx context.Context) (ir.StreamEvent, error) {
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

func (s *chatStream) closeOpenBlocks() {
	if s.openBlock {
		s.pending = append(s.pending, ir.NewBlockStop(s.currentBlockIndex))
		s.openBlock = false
	}
	for _, bIdx := range s.toolBlocks {
		s.pending = append(s.pending, ir.NewBlockStop(bIdx))
	}
	s.toolBlocks = make(map[int]int)
}

func (s *chatStream) handleEOF() {
	if s.terminated {
		return
	}
	if s.started {
		s.closeOpenBlocks()
		if s.finalUsage != nil && !s.usageEmitted {
			s.pending = append(s.pending, ir.NewUsageUpdate(*s.finalUsage))
			s.usageEmitted = true
		}
		reason := s.savedStopReason
		if reason == "" {
			reason = ir.StopEndTurn
		}
		s.pending = append(s.pending, ir.NewMessageStop(reason))
	}
	s.terminated = true
}

func (s *chatStream) handleSSEEvent(event sse.Event) error {
	data := strings.TrimSpace(event.Data)
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		s.handleEOF()
		return nil
	}

	var errChunk struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &errChunk) == nil && errChunk.Error != nil && errChunk.Error.Message != "" {
		if !s.started {
			s.pending = append(s.pending, ir.StreamEvent{
				Type:    ir.EventMessageStart,
				Message: &ir.Message{ID: "err_stream", Role: ir.RoleAssistant, Model: "unknown"},
			})
			s.started = true
		}
		kind := string(gwerr.KindUpstreamError)
		switch errChunk.Error.Type {
		case "insufficient_quota", "rate_limit_exceeded":
			kind = string(gwerr.KindRateLimit)
		case "invalid_request_error":
			kind = string(gwerr.KindInvalidRequest)
		}
		s.pending = append(s.pending, ir.NewErrorEvent(kind, errChunk.Error.Message))
		s.terminated = true
		return nil
	}

	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string  `json:"role"`
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens        int `json:"cached_tokens"`
				CacheCreationTokens int `json:"cache_creation_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}

	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return fmt.Errorf("openaichat: malformed SSE chunk: %w", err)
	}

	if !s.started {
		if chunk.ID == "" && len(chunk.Choices) == 0 && chunk.Usage == nil {
			return nil
		}
		id := chunk.ID
		if id == "" {
			id = "chatcmpl_stream"
		}
		model := chunk.Model
		if model == "" {
			model = "chat-model"
		}
		s.pending = append(s.pending, ir.StreamEvent{
			Type:    ir.EventMessageStart,
			Message: &ir.Message{ID: id, Role: ir.RoleAssistant, Model: model},
		})
		s.started = true
	}

	if chunk.Usage != nil {
		u := ir.Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
		}
		if chunk.Usage.PromptTokensDetails != nil {
			u.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			u.CacheWriteTokens = chunk.Usage.PromptTokensDetails.CacheCreationTokens
		}
		if chunk.Usage.CompletionTokensDetails != nil {
			u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		s.finalUsage = &u
	}

	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]

		var reasoningText string
		if choice.Delta.ReasoningContent != nil {
			reasoningText = *choice.Delta.ReasoningContent
		} else if choice.Delta.Reasoning != nil {
			reasoningText = *choice.Delta.Reasoning
		}
		if reasoningText != "" {
			if !s.openBlock || s.currentBlockType != ir.BlockReasoning {
				if s.openBlock {
					s.pending = append(s.pending, ir.NewBlockStop(s.currentBlockIndex))
					s.openBlock = false
				}
				idx := s.nextBlockIndex
				s.nextBlockIndex++
				s.currentBlockIndex = idx
				s.currentBlockType = ir.BlockReasoning
				s.openBlock = true
				s.pending = append(s.pending, ir.NewBlockStart(idx, ir.Block{Type: ir.BlockReasoning, Index: idx}))
			}
			s.pending = append(s.pending, ir.NewReasoningDelta(s.currentBlockIndex, reasoningText))
		}

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if !s.openBlock || s.currentBlockType != ir.BlockText {
				if s.openBlock {
					s.pending = append(s.pending, ir.NewBlockStop(s.currentBlockIndex))
					s.openBlock = false
				}
				idx := s.nextBlockIndex
				s.nextBlockIndex++
				s.currentBlockIndex = idx
				s.currentBlockType = ir.BlockText
				s.openBlock = true
				s.pending = append(s.pending, ir.NewBlockStart(idx, ir.Block{Type: ir.BlockText, Index: idx}))
			}
			s.pending = append(s.pending, ir.NewTextDelta(s.currentBlockIndex, *choice.Delta.Content))
		}

		if len(choice.Delta.ToolCalls) > 0 {
			if s.openBlock {
				s.pending = append(s.pending, ir.NewBlockStop(s.currentBlockIndex))
				s.openBlock = false
			}
			for _, tc := range choice.Delta.ToolCalls {
				bIdx, exists := s.toolBlocks[tc.Index]
				if !exists {
					bIdx = s.nextBlockIndex
					s.nextBlockIndex++
					s.toolBlocks[tc.Index] = bIdx
					s.pending = append(s.pending, ir.NewBlockStart(bIdx, ir.Block{
						Type:  ir.BlockToolUse,
						Index: bIdx,
						ToolUse: &ir.ToolUse{
							ID:   tc.ID,
							Name: tc.Function.Name,
						},
					}))
				}
				s.pending = append(s.pending, ir.NewToolArgumentsDelta(bIdx, tc.ID, tc.Function.Arguments))
			}
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.closeOpenBlocks()
			switch *choice.FinishReason {
			case "stop":
				s.savedStopReason = ir.StopEndTurn
			case "length":
				s.savedStopReason = ir.StopMaxTokens
			case "tool_calls":
				s.savedStopReason = ir.StopToolUse
			case "content_filter":
				s.savedStopReason = ir.StopContentFilter
			default:
				s.savedStopReason = ir.StopReason(*choice.FinishReason)
			}
		}
	}

	return nil
}
