package dispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/reliability"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

func (h *translation) ServeStream(c *gin.Context, body []byte, inbound adapter.Inbound, request *core.RequestContext) {
	c.Abort()
	request.ResolvedProvider = h.provider.ID
	request.UpstreamModel = h.model
	request.UpstreamProtocol = h.provider.Protocol
	request.Stream = true

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	request.BeginRecording(ctx, h.recorder)

	var usage ir.Usage
	var terminal error
	var completeOnce sync.Once
	markComplete := func(err error) {
		completeOnce.Do(func() {
			request.Complete(usage, completionError(err))
		})
	}
	defer func() {
		markComplete(terminal)
	}()

	decodedRequest := c.Request.Clone(ctx)
	decodedRequest.Body = io.NopCloser(bytes.NewReader(body))
	decodedRequest.ContentLength = int64(len(body))
	normalized, err := inbound.DecodeRequest(decodedRequest)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if normalized == nil {
		terminal = errors.New("dispatch: inbound adapter returned a nil request")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	normalized.Stream = true
	if request.InboundProtocol == ir.ProtocolAnthropicMessages || request.InboundProtocol == ir.ProtocolOpenAIResponses || normalized.StreamIncludeUsage {
		normalized.StreamIncludeUsage = true
	}
	ctx = adapter.WithStreamOptions(ctx, adapter.StreamOptions{IncludeUsage: normalized.StreamIncludeUsage})
	ctx = upstream.WithStream(ctx)

	effCaps, mode := h.resolvePolicy()
	policyRes, err := policy.Validate(normalized, effCaps, mode)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if dropped := policyRes.DroppedParamsHeaderValue(); dropped != "" {
		c.Writer.Header().Set(policy.DroppedParamsHeader, dropped)
	}
	if !effCaps.Streaming || !normalized.Stream {
		terminal = &gwerr.Error{
			Kind:    gwerr.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: "streaming is not supported by target upstream",
		}
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}

	headerRequest := c.Request.Clone(ctx)
	headerRequest.Body = http.NoBody
	headerRequest.Header = make(http.Header)
	if err := core.ApplyHeaders(headerRequest, c.Request, &h.provider); err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	headerRequest.Header.Set("Accept-Encoding", "identity")
	credentialHeader := "Authorization"
	if h.provider.Auth.Scheme == provider.AuthAPIKey {
		credentialHeader = "X-Api-Key"
	}
	redact.Default.Update(strings.TrimPrefix(headerRequest.Header.Get(credentialHeader), "Bearer "))
	target := adapter.Target{BaseURL: h.provider.BaseURL, Model: h.model, Headers: headerRequest.Header}
	outgoing, err := h.outbound.BuildRequest(ctx, normalized, target)
	if err != nil {
		if outgoing != nil && outgoing.Body != nil {
			err = errors.Join(err, outgoing.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if outgoing == nil {
		terminal = errors.New("dispatch: outbound adapter returned a nil request")
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}
	if outgoing.Context() != ctx {
		outgoing = outgoing.WithContext(ctx)
	}
	if err := h.policy.ValidateRequest(outgoing); err != nil {
		if outgoing.Body != nil {
			err = errors.Join(err, outgoing.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	if err := reliability.MakeReplayable(outgoing); err != nil {
		if outgoing.Body != nil {
			err = errors.Join(err, outgoing.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}

	response, err := h.client.Do(outgoing)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		respBody, readErr := io.ReadAll(response.Body)
		if closeErr := response.Body.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			terminal = outbound.UpstreamError(response, nil)
		} else {
			terminal = outbound.UpstreamError(response, respBody)
		}
		writeTranslationError(c, request.InboundProtocol, terminal)
		return
	}

	upstreamStream, err := h.outbound.ParseStream(ctx, response)
	if err != nil {
		if response.Body != nil {
			err = errors.Join(err, response.Body.Close())
		}
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}
	defer func() {
		if closeErr := upstreamStream.Close(); closeErr != nil && terminal == nil {
			terminal = closeErr
		}
	}()

	fbWriter := &firstByteTrackingWriter{
		ResponseWriter: c.Writer,
		rc:             request,
	}
	sseWriter, err := sse.NewWriter(fbWriter)
	if err != nil {
		terminal = err
		writeTranslationError(c, request.InboundProtocol, err)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	var streamErr error
	observedStream := &usageObservingStream{
		inner: upstreamStream,
		onUsage: func(u ir.Usage) {
			usage = u
		},
		onStart: func(msg *ir.Message, u *ir.Usage) {
			if u != nil {
				usage = *u
			}
			if msg != nil && request.ClientModel != "" {
				msg.Model = request.ClientModel
			}
		},
		onError: func(e *ir.StreamError) {
			if e != nil {
				streamErr = errors.New(e.Message)
			}
		},
	}

	terminal = inbound.EncodeStream(ctx, observedStream, sseWriter)
	if terminal == nil && streamErr != nil {
		terminal = streamErr
	}
}

type firstByteTrackingWriter struct {
	gin.ResponseWriter
	rc   *core.RequestContext
	once sync.Once
}

func (w *firstByteTrackingWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.once.Do(func() {
			w.rc.MarkFirstByte()
		})
	}
	return w.ResponseWriter.Write(p)
}

type usageObservingStream struct {
	inner   adapter.Stream
	onUsage func(ir.Usage)
	onStart func(*ir.Message, *ir.Usage)
	onError func(*ir.StreamError)
}

func (s *usageObservingStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	ev, err := s.inner.Next(ctx)
	if err != nil {
		return ev, err
	}
	switch ev.Type {
	case ir.EventMessageStart:
		if s.onStart != nil {
			s.onStart(ev.Message, ev.Usage)
		}
	case ir.EventUsageUpdate:
		if ev.Usage != nil && s.onUsage != nil {
			s.onUsage(*ev.Usage)
		}
	case ir.EventError:
		if ev.Error != nil {
			redacted := *ev.Error
			redacted.Message = redact.Default.Text(redacted.Message)
			ev.Error = &redacted
			if s.onError != nil {
				s.onError(ev.Error)
			}
		}
	}
	return ev, nil
}

func (s *usageObservingStream) Close() error {
	return s.inner.Close()
}
