// Package openairesponses converts normalized requests and responses to and
// from the OpenAI Responses wire format.
package openairesponses

import (
	"context"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Adapter is stateless and safe to share between requests.
type Adapter struct{}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns an OpenAI Responses outbound adapter.
func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIResponses }

func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming:        true,
		Images:           adapter.ImageCapabilityBoth,
		Reasoning:        true,
		StructuredOutput: true,
	}
}

// ParseStream reads an upstream OpenAI Responses SSE stream into normalized IR events.
func (*Adapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	return parseStream(ctx, resp)
}

func invalid(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: "openairesponses: " + message}
}

func unsupported(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: "openairesponses: " + message}
}
