package dispatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

type fallbackAttemptWriter struct {
	gin.ResponseWriter
	header    http.Header
	status    int
	buffer    bytes.Buffer
	committed bool
	firstByte bool
	isTrigger func(int) bool
	onCommit  func(http.Header)
}

func newFallbackAttemptWriter(dest gin.ResponseWriter, isTrigger func(int) bool, onCommit func(http.Header)) *fallbackAttemptWriter {
	if isTrigger == nil {
		isTrigger = func(s int) bool { return isFallbackTriggerStatus(s) }
	}
	return &fallbackAttemptWriter{
		ResponseWriter: dest,
		header:         make(http.Header),
		isTrigger:      isTrigger,
		onCommit:       onCommit,
	}
}

func (w *fallbackAttemptWriter) Header() http.Header {
	return w.header
}

func (w *fallbackAttemptWriter) WriteHeader(statusCode int) {
	if w.status != 0 {
		return
	}
	w.status = statusCode
	if !w.isTrigger(statusCode) {
		w.commit()
	}
}

func (w *fallbackAttemptWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *fallbackAttemptWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		w.firstByte = true
		return w.ResponseWriter.Write(data)
	}
	if w.buffer.Len() < 1<<20 {
		w.buffer.Write(data[:min(len(data), (1<<20)-w.buffer.Len())])
	}
	return len(data), nil
}

func (w *fallbackAttemptWriter) WriteString(s string) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		w.firstByte = true
		return w.ResponseWriter.WriteString(s)
	}
	if w.buffer.Len() < 1<<20 {
		left := (1 << 20) - w.buffer.Len()
		if len(s) > left {
			s = s[:left]
		}
		return w.buffer.WriteString(s)
	}
	return len(s), nil
}

func (w *fallbackAttemptWriter) Status() int {
	if w.status != 0 {
		return w.status
	}
	return w.ResponseWriter.Status()
}

func (w *fallbackAttemptWriter) Written() bool {
	return w.committed || w.ResponseWriter.Written()
}

func (w *fallbackAttemptWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		w.ResponseWriter.Flush()
	}
}

func (w *fallbackAttemptWriter) commit() {
	if w.committed {
		return
	}
	if w.onCommit != nil {
		w.onCommit(w.header)
	}
	destHeader := w.ResponseWriter.Header()
	for key, values := range w.header {
		destHeader[key] = append([]string(nil), values...)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(status)
	w.committed = true
	if w.buffer.Len() > 0 {
		w.firstByte = true
		if _, err := w.ResponseWriter.Write(w.buffer.Bytes()); err != nil {
			log.Printf("write fallback response: %v", err)
		}
		w.buffer.Reset()
	}
}

func (w *fallbackAttemptWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func resolveFallbackStatuses(cand *routing.Route) []int {
	if cand == nil {
		return nil
	}
	if len(cand.FallbackStatuses) > 0 {
		return cand.FallbackStatuses
	}
	if cand.Provider != nil && len(cand.Provider.Extensions) > 0 {
		if raw, ok := cand.Provider.Extensions["fallback_statuses"]; ok && len(raw) > 0 {
			var statuses []int
			if err := json.Unmarshal(raw, &statuses); err == nil && len(statuses) > 0 {
				return statuses
			}
		}
	}
	return nil
}

func isFallbackTriggerStatus(status int, configured ...[]int) bool {
	var list []int
	for _, c := range configured {
		if len(c) > 0 {
			list = c
			break
		}
	}
	if len(list) > 0 {
		for _, s := range list {
			if s == status {
				return true
			}
		}
		return false
	}
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		(status >= 500 && status <= 599)
}

type attemptRecorder struct {
	completion *core.Completion
	done       chan struct{}
}

func newAttemptRecorder() *attemptRecorder {
	return &attemptRecorder{done: make(chan struct{})}
}

func (r *attemptRecorder) Record(c *core.Completion) {
	r.completion = c
	close(r.done)
}

func (d *Dispatcher) finishAttempt(recorder *attemptRecorder, rc *core.RequestContext, fallbackAlias string, isFallback bool) {
	if recorder == nil {
		return
	}
	select {
	case <-recorder.done:
		if recorder.completion != nil && recorder.completion.Request != nil {
			finished := recorder.completion.Request
			if isFallback && fallbackAlias != "" {
				finished.ClientModel = fallbackAlias
				rc.ClientModel = fallbackAlias
			}
			rc.ResolvedProvider = finished.ResolvedProvider
			rc.UpstreamModel = finished.UpstreamModel
			rc.UpstreamProtocol = finished.UpstreamProtocol
			rc.Stream = finished.Stream
			if rc.FirstByte.IsZero() {
				rc.FirstByte = finished.FirstByte
			}
			if d.options.Recorder != nil {
				d.options.Recorder.Record(recorder.completion)
			}
		}
	default:
	}
}

func (d *Dispatcher) executeCandidate(c *gin.Context, body []byte, payloadModel string, isStream bool, route *routing.Route, rc *core.RequestContext, protocol ir.Protocol) bool {
	if route.UpstreamProtocol == protocol {
		modelKey := route.Alias
		if modelKey == "" {
			modelKey = payloadModel
		}
		handler, err := d.handler(modelKey, route)
		if err != nil {
			writeDispatchError(c, protocol, err)
			return true
		}
		c.Abort()
		handler.ServeBuffered(c.Writer, c.Request, body)
		return true
	}
	if d.Registry == nil {
		return false
	}
	inbound, inboundOK := d.Registry.Inbound(protocol)
	base, outboundOK := d.Registry.Outbound(route.Provider.Protocol)
	if !inboundOK || !outboundOK {
		return false
	}
	if isStream && !base.Capabilities().Streaming {
		return false
	}
	modelKey := route.Alias
	if modelKey == "" {
		modelKey = payloadModel
	}
	handler, err := d.translator(modelKey, route, base)
	if err != nil {
		d.failTranslation(c, protocol, route, rc, isStream, err)
		return true
	}
	if isStream {
		handler.ServeStream(c, body, inbound, rc)
		return true
	}
	handler.Serve(c, body, inbound, rc)
	return true
}

func (d *Dispatcher) dispatchWithFallbacks(c *gin.Context, body []byte, payloadModel string, payloadStream json.RawMessage, candidates []routing.Route, rc *core.RequestContext, protocol ir.Protocol) {
	isStream := requestedStream(payloadStream)
	origWriter := c.Writer
	origRequest := c.Request
	var activeRelease func()
	defer func() {
		c.Writer = origWriter
		c.Request = origRequest
		if activeRelease != nil {
			activeRelease()
		}
	}()

	for i := range candidates {
		cand := &candidates[i]
		isFallback := i > 0
		fallbackAlias := cand.Alias
		if fallbackAlias == "" {
			fallbackAlias = cand.UpstreamModel
		}
		isLast := (i == len(candidates)-1)

		candTriggerStatuses := resolveFallbackStatuses(cand)
		isTrigger := func(s int) bool {
			return isFallbackTriggerStatus(s, candTriggerStatuses)
		}

		if d.Limiter != nil {
			rel, acquireErr := d.Limiter.Acquire(c.Request.Context(), cand.Provider)
			if acquireErr != nil {
				var gwErr *gwerr.Error
				status := http.StatusServiceUnavailable
				if errors.As(acquireErr, &gwErr) && gwErr.Status != 0 {
					status = gwErr.Status
				}
				if isTrigger(status) && !isLast {
					continue
				}
				writeDispatchError(c, protocol, acquireErr)
				return
			}
			activeRelease = rel
		}

		attemptRecorder := newAttemptRecorder()
		attemptRC := core.NewRequestContext(rc.ID)
		if isFallback {
			attemptRC.ClientModel = fallbackAlias
		} else {
			attemptRC.ClientModel = rc.ClientModel
		}
		attemptRC.InboundProtocol = protocol
		attemptCtx := core.WithRequestContext(core.WithRecorder(c.Request.Context(), attemptRecorder), attemptRC)

		attemptReq := c.Request.Clone(attemptCtx)
		c.Request = attemptReq

		attemptWriter := newFallbackAttemptWriter(origWriter, isTrigger, func(h http.Header) {
			if isFallback {
				h.Set("X-Gateway-Fallback", fallbackAlias)
			}
		})
		c.Writer = attemptWriter

		executed := d.executeCandidate(c, body, payloadModel, isStream, cand, attemptRC, protocol)
		if !executed {
			if activeRelease != nil {
				activeRelease()
				activeRelease = nil
			}
			if !isLast {
				continue
			}
			restoreAndContinue(c, body)
			return
		}

		if c.Request.Context().Err() != nil {
			attemptWriter.commit()
			d.finishAttempt(attemptRecorder, rc, fallbackAlias, isFallback)
			return
		}

		if attemptWriter.committed || attemptWriter.firstByte {
			d.finishAttempt(attemptRecorder, rc, fallbackAlias, isFallback)
			return
		}

		if isTrigger(attemptWriter.status) && !isLast {
			if activeRelease != nil {
				activeRelease()
				activeRelease = nil
			}
			continue
		}

		attemptWriter.commit()
		d.finishAttempt(attemptRecorder, rc, fallbackAlias, isFallback)
		return
	}
}
