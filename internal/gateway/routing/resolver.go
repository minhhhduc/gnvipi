package routing

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

type Kind string

const (
	Custom      Kind = "custom"
	Fallthrough Kind = "fallthrough"
)

// Route is one routing decision. Provider belongs to the immutable published
// snapshot; consumers must not modify it or its maps/slices.
type Route struct {
	Provider         *provider.Provider
	UpstreamModel    string
	UpstreamProtocol ir.Protocol
	Kind             Kind
}

type Resolver interface {
	Resolve(context.Context, string, ir.Protocol) (Route, error)
}

type resolvedRoute struct {
	route  Route
	alias  string
	hidden bool
}

type snapshot struct {
	routes map[string]resolvedRoute
	hidden map[string]bool
}

// LegacyResolver preserves legacy flat-list precedence even when FromLegacy
// groups several nonadjacent entries into one provider. Only publication copies
// configuration; Resolve performs a single immutable snapshot lookup.
type LegacyResolver struct {
	mu      sync.Mutex
	current atomic.Pointer[snapshot]
}

func NewLegacyResolver(list []provider.LegacyProvider, hidden []string) (*LegacyResolver, error) {
	r := &LegacyResolver{}
	if err := r.Update(list, hidden); err != nil {
		return nil, err
	}
	return r, nil
}

// Update validates a complete replacement before publishing it. Failed updates
// leave the previous routing snapshot untouched.
func (r *LegacyResolver) Update(list []provider.LegacyProvider, hidden []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	converted, err := provider.FromLegacy(uniqueLegacyAliases(list))
	if err != nil {
		return err
	}
	type identity struct {
		name, base, key string
		protocol        ir.Protocol
	}
	byProvider := make(map[identity]*provider.Provider, len(converted))
	for i := range converted {
		p := &converted[i]
		byProvider[identity{p.Name, p.BaseURL, p.Auth.Key, p.Protocol}] = p
	}
	next := &snapshot{routes: make(map[string]resolvedRoute, 2*len(list)), hidden: make(map[string]bool, len(hidden))}
	for _, id := range hidden {
		next.hidden[id] = true
	}
	for i := range list {
		legacy := &list[i]
		protocol, _ := provider.LegacyProtocol(legacy)
		p := byProvider[identity{legacy.Name, legacy.BaseURL, legacy.APIKey, protocol}]
		alias := provider.LegacyAlias(legacy)
		entry := resolvedRoute{route: Route{Provider: p, UpstreamModel: legacy.Model, UpstreamProtocol: protocol, Kind: Custom}, alias: alias}
		for _, model := range []string{alias, legacy.Model} {
			if _, exists := next.routes[model]; !exists {
				next.routes[model] = entry
			}
		}
	}
	r.current.Store(next)
	return nil
}

// UpdateProviders retains complete v2 provider policies while preserving the
// existing exact-alias/bare-model precedence. Wildcard registry routing belongs
// to the registry resolver, not this legacy-precedence resolver.
func (r *LegacyResolver) UpdateProviders(list []provider.Provider, hidden []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	owned := make([]provider.Provider, len(list))
	type row struct {
		provider *provider.Provider
		mapping  *provider.ModelMapping
	}
	var rows []row
	for i := range list {
		owned[i] = provider.Clone(&list[i])
		p := &owned[i]
		if err := p.Validate(); err != nil {
			return err
		}
		if !p.Enabled {
			continue
		}
		for j := range p.Models {
			rows = append(rows, row{provider: p, mapping: &p.Models[j]})
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		first, second := a.mapping.LegacyPosition, b.mapping.LegacyPosition
		if first == 0 && second != 0 {
			return 1
		}
		if second == 0 && first != 0 {
			return -1
		}
		return cmp.Compare(first, second)
	})
	next := &snapshot{routes: make(map[string]resolvedRoute, 2*len(rows)), hidden: make(map[string]bool, len(hidden))}
	for _, id := range hidden {
		next.hidden[id] = true
	}
	for _, current := range rows {
		mapping, p := current.mapping, current.provider
		entry := resolvedRoute{route: Route{Provider: p, UpstreamModel: mapping.UpstreamModel, UpstreamProtocol: p.Protocol, Kind: Custom}, alias: mapping.Alias, hidden: mapping.Hidden}
		for _, model := range []string{mapping.Alias, mapping.UpstreamModel} {
			if _, exists := next.routes[model]; !exists {
				next.routes[model] = entry
			}
		}
	}
	r.current.Store(next)
	return nil
}

func (r *LegacyResolver) Resolve(ctx context.Context, clientModel string, _ ir.Protocol) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if strings.TrimSpace(clientModel) == "" {
		return Route{}, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "model must not be empty"}
	}
	current := r.current.Load()
	if current == nil {
		return Route{Kind: Fallthrough}, nil
	}
	entry, found := current.routes[clientModel]
	if !found {
		return Route{Kind: Fallthrough}, nil
	}
	if entry.hidden || current.hidden[clientModel] || current.hidden[entry.alias] {
		return Route{}, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
	}
	return entry.route, nil
}
