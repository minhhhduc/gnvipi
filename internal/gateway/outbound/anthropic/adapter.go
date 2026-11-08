// Package anthropic adapts normalized gateway requests and responses to the
// Anthropic Messages protocol.
package anthropic

import (
	"context"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

const defaultMaxTokens = 4096

// Adapter is a stateless Anthropic Messages adapter.
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

// Capabilities reports behavior implemented by this package.
func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming: true,
		Images:    adapter.ImageCapabilityBoth,
		Reasoning: true,
		TopK:      true,
	}
}

// ParseStream reads an upstream Anthropic Messages SSE response into normalized IR stream events.
func (*Adapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	return parseStream(ctx, resp)
}

func invalidRequest(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupported(message string) *gwerr.Error {
	err := invalidRequest(message)
	err.Code = "unsupported_feature"
	return err
}
