package core

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

const RequestIDHeader = "X-Request-Id"

// RequestContext is the identity, routing and timing of one inbound request.
// Populate routing fields before BeginRecording, then leave them immutable.
// FirstByte must be updated through MarkFirstByte once recording has started.
// A RequestContext must not be copied; use its pointer throughout dispatch.
// ResolvedProvider is the configured provider ID, independent of its schema.
// Merely attaching this context does not opt legacy requests into accounting.
type RequestContext struct {
	ID               string
	InboundProtocol  ir.Protocol
	ClientModel      string
	ResolvedProvider string
	UpstreamModel    string
	UpstreamProtocol ir.Protocol
	Start            time.Time
	FirstByte        time.Time
	Stream           bool

	mu        sync.Mutex
	ctx       context.Context
	recorder  Recorder
	stop      func() bool
	completed bool
}

// NewRequestContext accepts a bounded, visible ASCII client ID; invalid IDs
// are replaced using crypto/rand, never timestamps or a process counter.
func NewRequestContext(id string) *RequestContext {
	valid := id != "" && len(id) <= 128
	for i := 0; valid && i < len(id); i++ {
		valid = id[i] >= 0x21 && id[i] <= 0x7e
	}
	if !valid {
		id = rand.Text()
	}
	return &RequestContext{ID: id, Start: time.Now()}
}

type requestContextKey struct{}

func WithRequestContext(ctx context.Context, request *RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey{}, request)
}

func RequestContextFrom(ctx context.Context) *RequestContext {
	request, _ := ctx.Value(requestContextKey{}).(*RequestContext)
	return request
}

// PropagateRequestID uses the context's canonical ID, not untrusted upstream
// headers. The outbound request must retain the inbound context.
func PropagateRequestID(request *http.Request) {
	if rc := RequestContextFrom(request.Context()); rc != nil {
		request.Header.Set(RequestIDHeader, rc.ID)
	}
}

// Completion contains the immutable terminal snapshot given to a recorder.
// Request is detached from the live context, including first-byte timing.
type Completion struct {
	Request  *RequestContext
	Usage    ir.Usage
	Duration time.Duration
	TTFB     time.Duration
	Err      error
}

// Recorder consumes normalized usage, without inspecting response bytes.
// Callers must go through BeginRecording/Complete rather than invoking Record
// directly: the request context arbitrates competing terminal paths.
// Record receives a non-nil completion; treat it and Request as immutable.
type Recorder interface {
	Record(*Completion)
}

type recorderContextKey struct{}

// WithRecorder selects a request-local completion consumer. Re-entrant attempt
// handlers can retain usage without publishing each attempt as a client request.
// A scoped consumer takes precedence over a handler's default recorder.
func WithRecorder(ctx context.Context, recorder Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

// BeginRecording opts only normalized pipeline requests into the recorder.
// Call it after resolving routing metadata and before upstream work starts.
// Nil selects DefaultRecorder. Cancellation records even if dispatch never
// reaches an explicit completion; successful completion disarms that hook.
// Returns false if a recorder was already installed or completion already won.
func (r *RequestContext) BeginRecording(ctx context.Context, recorder Recorder) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorder != nil || r.completed {
		return false
	}
	if scoped, ok := ctx.Value(recorderContextKey{}).(Recorder); ok {
		recorder = scoped
	}
	if recorder == nil {
		recorder = DefaultRecorder{}
	}
	r.ctx, r.recorder = ctx, recorder
	r.stop = context.AfterFunc(ctx, func() { r.Complete(ir.Usage{}, ctx.Err()) })
	return true
}

// RecordingStarted reports whether normalized accounting was claimed, even
// when a competing terminal callback is still publishing its completion.
func (r *RequestContext) RecordingStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorder != nil
}

// MarkFirstByte captures the first observed upstream byte/token (or the
// middleware's first downstream write as a fallback). Later calls do nothing.
func (r *RequestContext) MarkFirstByte() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.completed && r.FirstByte.IsZero() {
		r.FirstByte = time.Now()
	}
}

// Complete records success, upstream error or cancellation exactly once.
// A canceled context cannot be reported as success, even if its AfterFunc is
// still waiting to run. Unclaimed legacy contexts are deliberately ignored.
// Returns true only for the winning terminal path. Recorder callbacks run
// outside the lifecycle lock and receive a detached snapshot.
func (r *RequestContext) Complete(usage ir.Usage, err error) bool {
	r.mu.Lock()
	if r.recorder == nil || r.completed {
		r.mu.Unlock()
		return false
	}
	if err == nil {
		err = r.ctx.Err()
	}
	r.completed = true
	request := &RequestContext{
		ID: r.ID, InboundProtocol: r.InboundProtocol, ClientModel: r.ClientModel,
		ResolvedProvider: r.ResolvedProvider, UpstreamModel: r.UpstreamModel,
		UpstreamProtocol: r.UpstreamProtocol, Start: r.Start, FirstByte: r.FirstByte,
		Stream: r.Stream,
	}
	completion := &Completion{Request: request, Usage: usage, Duration: max(time.Since(r.Start), 0), Err: err}
	if !r.FirstByte.IsZero() {
		completion.TTFB = max(r.FirstByte.Sub(r.Start), 0)
	}
	recorder, stop := r.recorder, r.stop
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	recorder.Record(completion)
	return true
}

// DefaultRecorder keeps the existing dashboard accounting and adds one
// escaped JSON access line. Raw errors, payloads and credentials are not logged.
// The existing collector's success/error accounting semantics are preserved.
type DefaultRecorder struct{}

func (DefaultRecorder) Record(c *Completion) {
	r := c.Request
	model := r.ClientModel
	if model == "" {
		model = r.UpstreamModel
	}
	nvidia.GlobalStats.Observe(model, uint64(max(c.Usage.InputTokens, 0)), uint64(max(c.Usage.OutputTokens, 0)), c.Duration, c.TTFB, r.Stream, c.Err != nil)
	line, err := json.Marshal(struct {
		RequestID        string      `json:"request_id"`
		InboundProtocol  ir.Protocol `json:"inbound_protocol"`
		ClientModel      string      `json:"client_model"`
		Provider         string      `json:"provider"`
		UpstreamModel    string      `json:"upstream_model"`
		UpstreamProtocol ir.Protocol `json:"upstream_protocol"`
		Stream           bool        `json:"stream"`
		DurationMS       int64       `json:"duration_ms"`
		TTFBMS           int64       `json:"ttfb_ms"`
		Usage            ir.Usage    `json:"usage"`
		Error            bool        `json:"error"`
	}{r.ID, r.InboundProtocol, r.ClientModel, r.ResolvedProvider, r.UpstreamModel,
		r.UpstreamProtocol, r.Stream, c.Duration.Milliseconds(), c.TTFB.Milliseconds(), c.Usage, c.Err != nil})
	if err != nil {
		log.Print("gateway access log: JSON encoding failed")
		return
	}
	log.Print(string(line))
}
