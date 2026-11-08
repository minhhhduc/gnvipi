package adapter

import (
	"context"
	"encoding/json"
	"fmt"
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

// StreamOptions conveys per-request streaming configuration (such as OpenAI's
// stream_options.include_usage) across the streaming pipeline via context.
type StreamOptions struct {
	IncludeUsage bool
}

type streamOptionsKey struct{}

// WithStreamOptions returns a context carrying opts.
func WithStreamOptions(ctx context.Context, opts StreamOptions) context.Context {
	return context.WithValue(ctx, streamOptionsKey{}, opts)
}

// StreamOptionsFrom extracts StreamOptions from ctx, returning zero value if unset.
func StreamOptionsFrom(ctx context.Context) StreamOptions {
	if v, ok := ctx.Value(streamOptionsKey{}).(StreamOptions); ok {
		return v
	}
	return StreamOptions{}
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

// ImageCapability enumerates supported image input forms for an upstream protocol.
type ImageCapability string

const (
	ImageCapabilityNone   ImageCapability = "none"
	ImageCapabilityBase64 ImageCapability = "base64"
	ImageCapabilityURL    ImageCapability = "url"
	ImageCapabilityBoth   ImageCapability = "both"
)

// Enabled reports whether any image format is supported.
func (c ImageCapability) Enabled() bool {
	return c == ImageCapabilityBase64 || c == ImageCapabilityURL || c == ImageCapabilityBoth
}

// SupportsBase64 reports whether inline base64 images are supported.
func (c ImageCapability) SupportsBase64() bool {
	return c == ImageCapabilityBase64 || c == ImageCapabilityBoth
}

// SupportsURL reports whether remote HTTP/HTTPS image URLs are supported.
func (c ImageCapability) SupportsURL() bool {
	return c == ImageCapabilityURL || c == ImageCapabilityBoth
}

func (c *ImageCapability) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*c = ImageCapabilityBoth
		} else {
			*c = ImageCapabilityNone
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch ImageCapability(s) {
	case ImageCapabilityNone, ImageCapabilityBase64, ImageCapabilityURL, ImageCapabilityBoth:
		*c = ImageCapability(s)
		return nil
	case "":
		*c = ImageCapabilityNone
		return nil
	default:
		return fmt.Errorf("invalid image capability %q: must be none, base64, url, or both", s)
	}
}

// Capabilities describes what an upstream protocol can express. Fields that
// are false are not necessarily errors: the dispatcher decides per request
// whether to drop the feature, reject it, or fall back to another provider.
type Capabilities struct {
	Streaming         bool            `json:"streaming"`           // server-sent events
	Tools             bool            `json:"tools"`               // tool / function definitions and calls
	ParallelToolCalls bool            `json:"parallel_tool_calls"` // more than one tool call per turn
	Images            ImageCapability `json:"images"`              // image inputs: none|base64|url|both
	Reasoning         bool            `json:"reasoning"`           // extended thinking / reasoning tokens
	StructuredOutput  bool            `json:"structured_output"`   // JSON mode and JSON schema
	TopK              bool            `json:"top_k"`               // top-k sampling
	StatefulResponses bool            `json:"stateful_responses"`  // server-side conversation state
	CountTokens       bool            `json:"count_tokens"`        // an exact token-count endpoint
	MaxImageBytes     int64           `json:"max_image_bytes,omitempty"`
	AllowedMediaTypes []string        `json:"allowed_media_types,omitempty"`
}

// IsZero reports whether all capability flags are unset.
func (c Capabilities) IsZero() bool {
	return !c.Streaming &&
		!c.Tools &&
		!c.ParallelToolCalls &&
		(c.Images == "" || c.Images == ImageCapabilityNone) &&
		!c.Reasoning &&
		!c.StructuredOutput &&
		!c.TopK &&
		!c.StatefulResponses &&
		!c.CountTokens &&
		c.MaxImageBytes == 0 &&
		len(c.AllowedMediaTypes) == 0
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
