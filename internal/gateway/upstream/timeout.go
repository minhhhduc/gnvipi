package upstream

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

type timeoutTransport struct {
	base     http.RoundTripper
	timeouts provider.Timeouts
	proxy    func(*http.Request) (*url.URL, error)
}

// contextFailure retains both cancellation and its cause without turning a
// caller-supplied EOF cancellation cause into a successful end of response.
type contextFailure struct{ interrupted, cause error }

func (e *contextFailure) Error() string   { return e.cause.Error() }
func (e *contextFailure) Unwrap() []error { return []error{e.interrupted, e.cause} }

func requestContextError(ctx context.Context) error {
	interrupted := ctx.Err()
	if interrupted == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, interrupted) {
		return cause
	}
	return &contextFailure{interrupted: interrupted, cause: cause}
}

type totalDeadlineKey struct{}

func (t *timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	streaming, _ := ctx.Value(streamKey{}).(bool)
	var total *time.Timer
	if !streaming {
		deadline, ok := req.Context().Value(totalDeadlineKey{}).(time.Time)
		if !ok && req.Response != nil && req.Response.Request != nil {
			// net/http builds redirect requests from the original context,
			// but carries the preceding response (and its outgoing request).
			deadline, ok = req.Response.Request.Context().Value(totalDeadlineKey{}).(time.Time)
		}
		if !ok {
			deadline = time.Now().Add(t.timeouts.Total)
		}
		ctx = context.WithValue(ctx, totalDeadlineKey{}, deadline)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			cancel(context.DeadlineExceeded)
		} else {
			total = time.AfterFunc(remaining, func() { cancel(context.DeadlineExceeded) })
		}
	}
	var headerMu sync.Mutex
	headersDone := false
	// Bound the entire pre-header phase, including a blocked upload, rather
	// than leaving streaming requests unbounded before the first response.
	header := time.AfterFunc(t.timeouts.ResponseHeader, func() {
		headerMu.Lock()
		defer headerMu.Unlock()
		if !headersDone {
			cancel(context.DeadlineExceeded)
		}
	})
	var resp *http.Response
	var err error
	if t.proxy != nil {
		var u *url.URL
		u, err = t.proxy(req)
		if err == nil {
			ctx = context.WithValue(ctx, proxyURLKey{}, u)
		}
	}
	if err == nil {
		resp, err = t.base.RoundTrip(req.Clone(ctx))
	} else if req.Body != nil {
		err = errors.Join(err, req.Body.Close())
	}
	headerMu.Lock()
	headersDone = true
	header.Stop()
	headerMu.Unlock()
	if cause := requestContextError(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		if total != nil {
			total.Stop()
		}
		cancel(nil)
		if resp != nil && resp.Body != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		return nil, err
	}
	idle := time.Duration(0)
	if streaming {
		idle = t.timeouts.Idle
	}
	resp.Body = newTimedBody(resp.Body, ctx, cancel, total, idle)
	return resp, nil
}

func (t *timeoutTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type timedBody struct {
	body        io.ReadCloser
	ctx         context.Context
	cancel      context.CancelCauseFunc
	total       *time.Timer
	idle        time.Duration
	mu          sync.Mutex
	timer       *time.Timer
	last        time.Time
	closed      bool
	terminal    error
	stopContext func() bool
	once        sync.Once
	closeErr    error
}

func newTimedBody(body io.ReadCloser, ctx context.Context, cancel context.CancelCauseFunc, total *time.Timer, idle time.Duration) *timedBody {
	b := &timedBody{body: body, ctx: ctx, cancel: cancel, total: total, idle: idle, last: time.Now()}
	b.mu.Lock()
	b.stopContext = context.AfterFunc(ctx, func() {
		if err := b.closeWithError(requestContextError(ctx)); err != nil {
			log.Printf("close canceled upstream body: %v", err)
		}
	})
	if idle > 0 {
		b.timer = time.AfterFunc(idle, b.expireIdle)
	}
	b.mu.Unlock()
	return b
}

func (b *timedBody) expireIdle() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	remaining := b.idle - time.Since(b.last)
	if remaining > 0 {
		b.timer.Reset(remaining)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	if err := b.closeWithError(context.DeadlineExceeded); err != nil {
		log.Printf("close idle upstream body: %v", err)
	}
}

func (b *timedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	closed, terminal := b.closed, b.terminal
	b.mu.Unlock()
	if closed {
		if terminal != nil {
			return 0, terminal
		}
		return 0, io.ErrClosedPipe
	}
	n, err := b.body.Read(p)
	if n > 0 {
		b.mu.Lock()
		if !b.closed {
			b.last = time.Now()
			if b.timer != nil {
				b.timer.Reset(b.idle)
			}
		}
		b.mu.Unlock()
	}
	if cause := requestContextError(b.ctx); cause != nil {
		err = cause
	}
	if err != nil {
		if closeErr := b.closeWithError(err); closeErr != nil {
			if errors.Is(err, io.EOF) {
				err = closeErr
			} else {
				err = errors.Join(err, closeErr)
			}
		}
	}
	return n, err
}

func (b *timedBody) Close() error { return b.closeWithError(nil) }

func (b *timedBody) closeWithError(terminal error) error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed, b.terminal = true, terminal
		if b.timer != nil {
			b.timer.Stop()
		}
		if b.total != nil {
			b.total.Stop()
		}
		stop := b.stopContext
		b.mu.Unlock()
		if stop != nil {
			stop()
		}
		cause := terminal
		if errors.Is(cause, io.EOF) {
			cause = nil
		}
		b.cancel(cause) // Abort network I/O before Close waits for a blocked Read.
		b.closeErr = b.body.Close()
	})
	return b.closeErr
}
