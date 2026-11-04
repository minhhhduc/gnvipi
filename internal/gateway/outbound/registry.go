package outbound

import (
	"fmt"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

type named struct {
	adapter.Outbound
	protocol ir.Protocol
}

func (n *named) Protocol() ir.Protocol     { return n.protocol }
func (n *named) wireProtocol() ir.Protocol { return wireProtocol(n.Outbound) }

// RegisterCustom explicitly registers base under custom:<name> in registry.
// The adapter still speaks base's wire protocol; the name only selects it.
// Registrations are scoped to adapter.Registry, thread-safe and duplicate-safe.
// Invalid names, nil adapters and duplicates panic, matching the existing
// registry's wiring-error convention. There are no package-level registrations,
// init side effects, automatic standard adapters, or fallback on a missing name.
func RegisterCustom(registry *adapter.Registry, name string, base adapter.Outbound) {
	protocol, err := provider.ParseProtocol("custom:" + name)
	if err != nil {
		panic("outbound: " + err.Error())
	}
	if registry == nil || base == nil {
		panic("outbound: RegisterCustom with a nil registry or adapter")
	}
	registry.RegisterOutbound(&named{Outbound: base, protocol: protocol})
}

// Select looks up exactly p.Protocol in the supplied registry and applies p's
// declarative extensions. Standard adapters must also be explicitly registered
// by the wiring point; selecting an unregistered name never silently falls back.
// Provider validation accepts valid custom names independently of registration,
// while Select checks that this particular registry can actually serve them.
func Select(registry *adapter.Registry, p *provider.Provider) (adapter.Outbound, error) {
	if p == nil {
		return nil, fmt.Errorf("outbound: provider configuration is nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("outbound: nil registry")
	}
	protocol, err := provider.ParseProtocol(p.Protocol.String())
	if err != nil {
		return nil, err
	}
	base, ok := registry.Outbound(protocol)
	if !ok {
		return nil, fmt.Errorf("outbound: no adapter registered for %s", protocol)
	}
	return Configure(base, p)
}
