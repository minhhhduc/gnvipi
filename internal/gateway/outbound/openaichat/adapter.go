// Package openaichat adapts normalized gateway requests and complete responses
// to the OpenAI Chat Completions wire format.
package openaichat

import (
	"context"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Adapter is safe for concurrent requests after its configuration fields are
// set. InstructionRole defaults to system; developer is the other valid value.
// UseMaxCompletionTokens selects the newer token-budget field.
type Adapter struct {
	InstructionRole        ir.InstructionRole
	UseMaxCompletionTokens bool
}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns an OpenAI Chat outbound adapter with conservative defaults.
func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIChat }

func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming:         true,
		Images:            adapter.ImageCapabilityBoth,
		Reasoning:         true,
		StructuredOutput:  true,
		TopK:              true,
		Tools:             true,
		ParallelToolCalls: true,
	}
}

// ParseStream reads an upstream OpenAI Chat SSE response into normalized IR stream events.
func (*Adapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	return parseStream(ctx, resp)
}

func invalid(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}
