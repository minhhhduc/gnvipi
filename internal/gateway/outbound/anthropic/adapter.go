// Package anthropic adapts normalized gateway requests and responses to the
// Anthropic Messages protocol.
package anthropic

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

const defaultMaxTokens = 4096

// Adapter is an Anthropic Messages outbound adapter.
// DefaultMaxTokens is used when the IR has no output-token budget. Zero uses
// the protocol default of 4096.
// ToolEmulation enables structured output (Option A per issue #49) by forcing
// a single tool call with the schema and unwrapping the tool input as text.
// JSONModePrompt enables prompting for valid JSON on json_object format.
type Adapter struct {
	DefaultMaxTokens int
	ToolEmulation    bool
	JSONModePrompt   bool
}

var _ adapter.Outbound = (*Adapter)(nil)

// New returns an Anthropic Messages outbound adapter.
func New() *Adapter { return &Adapter{} }

// Protocol reports the wire protocol implemented by this adapter.
func (*Adapter) Protocol() ir.Protocol { return ir.ProtocolAnthropicMessages }

// Capabilities reports behavior implemented by this package.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Streaming:        true,
		Images:           true,
		Reasoning:        true,
		StructuredOutput: a.ToolEmulation || a.JSONModePrompt,
		TopK:             true,
	}
}

// WithProvider returns a configured copy of the adapter if provider extensions specify tool emulation.
func (a *Adapter) WithProvider(p *provider.Provider) adapter.Outbound {
	if p == nil || len(p.Extensions) == 0 {
		return a
	}
	clone := *a
	if raw, ok := p.Extensions["tool_emulation"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil {
			clone.ToolEmulation = enabled
		}
	}
	if raw, ok := p.Extensions["anthropic"]; ok {
		var opts struct {
			ToolEmulation  bool `json:"tool_emulation"`
			JSONModePrompt bool `json:"json_mode_prompt"`
		}
		if err := json.Unmarshal(raw, &opts); err == nil {
			if opts.ToolEmulation {
				clone.ToolEmulation = true
			}
			if opts.JSONModePrompt {
				clone.JSONModePrompt = true
			}
		}
	}
	if raw, ok := p.Extensions["structured_output"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil {
			clone.ToolEmulation = enabled
		}
		var mode string
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch mode {
			case "tool", "emulation", "tool_emulation":
				clone.ToolEmulation = true
			case "prompt":
				clone.JSONModePrompt = true
			}
		}
	}
	return &clone
}

// ParseStream reads an upstream Anthropic Messages SSE response into normalized IR stream events.
func (a *Adapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	return parseStream(ctx, resp, a.ToolEmulation)
}

func invalidRequest(message string) *gwerr.Error {
	return &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Message: message}
}

func unsupported(message string) *gwerr.Error {
	err := invalidRequest(message)
	err.Code = "unsupported_feature"
	return err
}
