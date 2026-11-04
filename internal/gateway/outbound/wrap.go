// Package outbound decorates registered upstream adapters. Registration and
// selection are explicit; importing this package changes no global state.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Hooks runs Request after BuildRequest and Response before either response
// parser. Hooks are called concurrently and must not share mutable state.
// Request receives the caller's context; Response receives ParseStream's caller
// context or resp.Request.Context for non-streaming responses (Background only
// for synthetic responses with no Request). Hooks must not detach a request
// from its caller's context. A hook replacing a body must transfer ownership
// of the old body to its replacement or close it itself, just as a transport
// would. On hook failure the decorator closes the current body.
//
// Compose Wrap(Configure(base, config), hooks) to run Go request hooks after
// declarative changes. Nested wrappers run request hooks inside-out and
// response hooks outside-in. Hooks are trusted Go code, not provider config.
type Hooks struct {
	Request  func(context.Context, *http.Request) error
	Response func(context.Context, *http.Response) error
}

type wrapped struct {
	adapter.Outbound
	hooks Hooks
}

// Wrap decorates base without changing its protocol or capabilities. It panics
// for a nil base, consistent with adapter.Registry's wiring-error convention.
// Successful ParseStream returns the base stream untouched: only that stream,
// not the decorator, owns and closes the response body. Failed hooks or failed
// stream creation close the body instead of leaving a discarded connection.
func Wrap(base adapter.Outbound, hooks Hooks) adapter.Outbound {
	if base == nil {
		panic("outbound: Wrap with a nil adapter")
	}
	return &wrapped{Outbound: base, hooks: hooks}
}

func (w *wrapped) BuildRequest(ctx context.Context, req *ir.Request, target adapter.Target) (*http.Request, error) {
	r, err := w.Outbound.BuildRequest(ctx, req, target)
	if err != nil {
		return nil, errors.Join(err, closeRequest(r))
	}
	if r == nil {
		return nil, errors.New("outbound: base returned a nil request")
	}
	if r.Context() != ctx {
		r = r.WithContext(ctx)
	}
	if w.hooks.Request != nil {
		if err := w.hooks.Request(ctx, r); err != nil {
			return nil, errors.Join(fmt.Errorf("outbound: request hook: %w", err), closeRequest(r))
		}
	}
	if r.Context() != ctx {
		r = r.WithContext(ctx)
	}
	return r, nil
}

func (w *wrapped) ParseResponse(resp *http.Response) (*ir.Response, error) {
	ctx := context.Background()
	if resp != nil && resp.Request != nil {
		ctx = resp.Request.Context()
	}
	if err := w.responseHook(ctx, resp); err != nil {
		return nil, err
	}
	return w.Outbound.ParseResponse(resp)
}

func (w *wrapped) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	if err := w.responseHook(ctx, resp); err != nil {
		return nil, err
	}
	stream, err := w.Outbound.ParseStream(ctx, resp)
	if err != nil {
		if stream != nil {
			err = errors.Join(err, stream.Close())
		} else {
			err = errors.Join(err, closeResponse(resp))
		}
		return nil, err
	}
	if stream == nil {
		return nil, errors.Join(errors.New("outbound: base returned a nil stream"), closeResponse(resp))
	}
	return stream, nil
}

func (w *wrapped) responseHook(ctx context.Context, resp *http.Response) error {
	if resp == nil {
		return errors.New("outbound: nil response")
	}
	guardResponseBody(resp)
	if w.hooks.Response != nil {
		err := w.hooks.Response(ctx, resp)
		guardResponseBody(resp)
		if err != nil {
			return errors.Join(fmt.Errorf("outbound: response hook: %w", err), closeResponse(resp))
		}
	}
	return nil
}

func guardResponseBody(resp *http.Response) {
	if resp.Body != nil {
		if _, guarded := resp.Body.(*onceBody); !guarded {
			resp.Body = &onceBody{ReadCloser: resp.Body}
		}
	}
}

func closeRequest(req *http.Request) error {
	if req != nil && req.Body != nil {
		return req.Body.Close()
	}
	return nil
}
func closeResponse(resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		return resp.Body.Close()
	}
	return nil
}

// Nested decorators may each observe a parse failure; the shared body guard
// preserves a single close while leaving successful stream ownership alone.
type onceBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *onceBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}
