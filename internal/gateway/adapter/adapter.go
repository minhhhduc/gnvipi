package adapter

import (
	"context"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// Stream is a pull-based sequence of normalized stream events.
//
// Next returns io.EOF after a clean end of stream. Any other error is a
// transport or parse failure; a failure the provider reported arrives as an
// ir.EventError event instead, because the client still has to be told about
// it in its own protocol.
type Stream interface {
	// Next returns the next event, blocking until one is available, the
	// stream ends (io.EOF), ctx is done, or the upstream fails.
	Next(ctx context.Context) (ir.StreamEvent, error)

	// Close releases the upstream connection. It is safe to call after Next
	// already returned an error.
	Close() error
}

// Inbound adapts one client-facing wire protocol to the IR.
//
// Implementations live in internal/gateway/inbound/<proto>. Methods are
// called concurrently from different requests and must not share mutable
// state.
type Inbound interface {
	// Protocol reports the client protocol this adapter decodes and encodes,
	// and is what the registry keys it by.
	Protocol() ir.Protocol

	// DecodeRequest reads req's body into a normalized request. The adapter
	// owns the body while decoding; the dispatcher does not read it again
	// afterwards.
	DecodeRequest(req *http.Request) (*ir.Request, error)

	// EncodeResponse renders a complete, non-streaming response body. The
	// returned bytes are written as-is, so the adapter is responsible for the
	// protocol's exact JSON shape.
	EncodeResponse(resp *ir.Response) ([]byte, error)

	// EncodeStream renders events as the protocol's SSE events through w. It
	// returns after the terminal event (message_stop or error) or on the
	// first write failure, and never writes anything after a terminal event.
	EncodeStream(ctx context.Context, events Stream, w *sse.Writer) error
}

// Outbound adapts the IR to one upstream provider protocol.
//
// Implementations live in internal/gateway/outbound/<proto>. Methods are
// called concurrently from different requests and must not share mutable
// state.
type Outbound interface {
	// Protocol reports the upstream wire protocol this adapter speaks, and is
	// what the registry keys it by.
	Protocol() ir.Protocol

	// Capabilities reports what this protocol can carry. The dispatcher and
	// the model catalog use it to decide what may be forwarded, translated or
	// rejected.
	Capabilities() Capabilities

	// BuildRequest turns a normalized request into the upstream HTTP request
	// for t. The body must be a reader the caller can send; any credential
	// already lives in t.Headers.
	BuildRequest(ctx context.Context, req *ir.Request, t Target) (*http.Request, error)

	// ParseResponse reads a complete upstream response into the IR. It
	// returns a *gwerr.Error for upstream failures so they reach the client
	// in a shape it understands.
	ParseResponse(resp *http.Response) (*ir.Response, error)

	// ParseStream reads an upstream SSE response into normalized events. The
	// returned Stream owns resp.Body and closes it.
	ParseStream(ctx context.Context, resp *http.Response) (Stream, error)
}

// Capabilities describes what an upstream protocol can express. Fields that
// are false are not necessarily errors: the dispatcher decides per request
// whether to drop the feature, reject it, or fall back to another provider.
type Capabilities struct {
	Streaming         bool // server-sent events
	Tools             bool // tool / function definitions and calls
	ParallelToolCalls bool // more than one tool call per turn
	Images            bool // image inputs
	Reasoning         bool // extended thinking / reasoning tokens
	StructuredOutput  bool // JSON mode and JSON schema
	TopK              bool // top-k sampling
	StatefulResponses bool // server-side conversation state
	CountTokens       bool // an exact token-count endpoint
}

// Target is the resolved upstream endpoint for one request.
//
// Headers is already fully resolved by the relay before BuildRequest is
// called: provider auth is applied (bearer, x-api-key or none), the
// allow-list forwarding policy has copied the client headers that are safe to
// send, and the client's own credentials have been stripped. An outbound
// adapter copies Headers verbatim, and must never derive, add or overwrite
// credentials itself.
//
// BaseURL has no trailing slash and may include a path prefix; the adapter
// appends its protocol's endpoint path to it.
type Target struct {
	BaseURL string
	Model   string
	Headers http.Header
}
