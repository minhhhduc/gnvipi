// Package anthropic adapts normalized gateway requests and responses to the
// non-streaming Anthropic Messages JSON protocol.
package anthropic

import (
	"context"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

const defaultMaxTokens = 4096

// Adapter is a stateless non-streaming Anthropic Messages adapter.
// DefaultMaxTokens is used when the IR has no output-token budget. Zero uses
// the protocol default of 4096.
type Adapter struct {
	DefaultMaxTokens int
}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns an Anthropic Messages outbound adapter.
func New() *Adapter { return &Adapter{} }

// Protocol reports the wire protocol implemented by this adapter.
func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolAnthropicMessages }

// Capabilities reports only behavior implemented by this package. Tool request
// conformance and streaming are separate issues even though non-streaming
// responses tolerate the minimal tool_use block representable by the IR.
func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Images: true, Reasoning: true, TopK: true}
}

// ParseStream truthfully rejects streaming until the dedicated SSE adapter is
// implemented. Rejection leaves response.Body owned by the caller.
func (*Adapter) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, unsupported("Anthropic response streaming is not supported by this adapter")
}

func invalidRequest(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupported(message string) *gwerr.Error {
	err := invalidRequest(message)
	err.Code = "unsupported_feature"
	return err
}
