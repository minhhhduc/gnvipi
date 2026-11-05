// Package openairesponses converts normalized requests and responses to and
// from the non-streaming OpenAI Responses wire format.
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

// New returns a non-streaming OpenAI Responses outbound adapter.
func New() *Adapter { return &Adapter{} }

func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolOpenAIResponses }

func (*Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Images: true, Reasoning: true, StructuredOutput: true}
}

// ParseStream is intentionally unavailable until issue #28. Rejection does
// not read or close the response body, so ownership remains with the caller.
func (*Adapter) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, unsupported("streaming response parsing is not implemented")
}

func invalid(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: "openairesponses: " + message}
}

func unsupported(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: "openairesponses: " + message}
}
