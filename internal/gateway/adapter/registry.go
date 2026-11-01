package adapter

import (
	"sync"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

// Registry maps protocols to the adapters that implement them. The wiring
// point (cmd/serve) builds one, registers every adapter it wants, and hands
// it to the dispatcher.
//
// Registration panics on a nil adapter or a duplicate protocol: both are
// wiring mistakes that would otherwise surface as a silently missing or
// shadowed adapter in production.
type Registry struct {
	mu       sync.RWMutex
	inbound  map[ir.Protocol]Inbound
	outbound map[ir.Protocol]Outbound
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		inbound:  make(map[ir.Protocol]Inbound),
		outbound: make(map[ir.Protocol]Outbound),
	}
}

// RegisterInbound adds a client-protocol adapter. It panics if a is nil or if
// its protocol is already registered.
func (r *Registry) RegisterInbound(a Inbound) {
	if a == nil {
		panic("adapter: RegisterInbound with a nil adapter")
	}
	p := a.Protocol()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.inbound[p]; dup {
		panic("adapter: duplicate inbound registration for protocol " + p.String())
	}
	r.inbound[p] = a
}

// RegisterOutbound adds an upstream-protocol adapter. It panics if a is nil
// or if its protocol is already registered.
func (r *Registry) RegisterOutbound(a Outbound) {
	if a == nil {
		panic("adapter: RegisterOutbound with a nil adapter")
	}
	p := a.Protocol()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.outbound[p]; dup {
		panic("adapter: duplicate outbound registration for protocol " + p.String())
	}
	r.outbound[p] = a
}

// Inbound returns the client-protocol adapter, or nil and false when the
// protocol has none registered.
func (r *Registry) Inbound(p ir.Protocol) (Inbound, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.inbound[p]
	return a, ok
}

// Outbound returns the upstream-protocol adapter, or nil and false when the
// protocol has none registered.
func (r *Registry) Outbound(p ir.Protocol) (Outbound, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.outbound[p]
	return a, ok
}
