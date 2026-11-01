package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// The fakes prove both interfaces are implementable outside the package and
// let the registry tests compare the value they looked up against the one they
// registered.

type fakeInbound struct{ proto ir.Protocol }

func (f fakeInbound) Protocol() ir.Protocol { return f.proto }

func (f fakeInbound) DecodeRequest(r *http.Request) (*ir.Request, error) {
	return &ir.Request{Model: r.URL.Path}, nil
}

func (f fakeInbound) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return []byte(resp.ID), nil
}

func (f fakeInbound) EncodeStream(ctx context.Context, events Stream, w *sse.Writer) error {
	if _, err := events.Next(ctx); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	w.Flush()
	return nil
}

type fakeOutbound struct{ proto ir.Protocol }

func (f fakeOutbound) Protocol() ir.Protocol { return f.proto }

func (f fakeOutbound) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: f.proto == ir.ProtocolAnthropicMessages}
}

func (f fakeOutbound) BuildRequest(ctx context.Context, req *ir.Request, t Target) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/"+req.Model, http.NoBody)
}

func (f fakeOutbound) ParseResponse(resp *http.Response) (*ir.Response, error) {
	return &ir.Response{ID: resp.Status}, nil
}

func (f fakeOutbound) ParseStream(context.Context, *http.Response) (Stream, error) {
	return emptyStream{}, nil
}

type emptyStream struct{}

func (emptyStream) Next(context.Context) (ir.StreamEvent, error) { return ir.StreamEvent{}, io.EOF }
func (emptyStream) Close() error                                 { return nil }

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	chat := fakeInbound{proto: ir.ProtocolOpenAIChat}
	anthropic := fakeOutbound{proto: ir.ProtocolAnthropicMessages}
	r.RegisterInbound(chat)
	r.RegisterOutbound(anthropic)

	if got, ok := r.Inbound(ir.ProtocolOpenAIChat); !ok || got.Protocol() != chat.proto {
		t.Fatalf("Inbound(openai-chat) = %v, %v", got, ok)
	}
	if got, ok := r.Outbound(ir.ProtocolAnthropicMessages); !ok || got.Protocol() != anthropic.proto {
		t.Fatalf("Outbound(anthropic-messages) = %v, %v", got, ok)
	}
	if got, ok := r.Inbound(ir.ProtocolAnthropicMessages); ok || got != nil {
		t.Fatalf("Inbound(anthropic-messages) = %v, %v, want nil, false", got, ok)
	}
	if got, ok := r.Outbound(ir.ProtocolOpenAIResponses); ok || got != nil {
		t.Fatalf("Outbound(openai-responses) = %v, %v, want nil, false", got, ok)
	}
}

func TestRegistryRegistriesAreIndependent(t *testing.T) {
	first, second := NewRegistry(), NewRegistry()
	first.RegisterInbound(fakeInbound{proto: ir.ProtocolAnthropicMessages})
	if _, ok := second.Inbound(ir.ProtocolAnthropicMessages); ok {
		t.Fatal("a second registry sees the first one's adapters")
	}
}

func TestRegistryPanics(t *testing.T) {
	tests := []struct {
		name string
		reg  func(*Registry)
	}{
		{"nil inbound", func(r *Registry) { r.RegisterInbound(nil) }},
		{"nil outbound", func(r *Registry) { r.RegisterOutbound(nil) }},
		{"duplicate inbound", func(r *Registry) {
			r.RegisterInbound(fakeInbound{proto: ir.ProtocolOpenAIChat})
			r.RegisterInbound(fakeInbound{proto: ir.ProtocolOpenAIChat})
		}},
		{"duplicate outbound", func(r *Registry) {
			r.RegisterOutbound(fakeOutbound{proto: ir.ProtocolOpenAIChat})
			r.RegisterOutbound(fakeOutbound{proto: ir.ProtocolOpenAIChat})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration did not panic")
				}
			}()
			tt.reg(NewRegistry())
		})
	}
}

func TestRegistryConcurrentLookups(t *testing.T) {
	r := NewRegistry()
	r.RegisterInbound(fakeInbound{proto: ir.ProtocolOpenAIResponses})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, ok := r.Inbound(ir.ProtocolOpenAIResponses); !ok {
					t.Error("lookup lost a registered adapter")
					return
				}
			}
		}()
	}
	wg.Wait()
}
