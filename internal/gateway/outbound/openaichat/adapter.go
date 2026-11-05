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
	return adapter.Capabilities{Images: true, Reasoning: true, StructuredOutput: true, TopK: true}
}

// ParseStream rejects streaming truthfully without reading or closing the
// caller-owned response body. OpenAI Chat SSE parsing is implemented by #24.
func (*Adapter) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, unsupported("OpenAI Chat streaming responses are not supported")
}

func invalid(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupported(message string) error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: message}
}
