package routing

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/models"
)

type Kind string

const (
	Custom      Kind = "custom"
	Fallthrough Kind = "fallthrough"
)

const (
	VirtualAutoMode = "automode"
	VirtualCycle    = "cycle"
)

// Route is one routing decision. Provider belongs to the immutable published
// snapshot; consumers must not modify it or its maps/slices.
type Route struct {
	Provider         *provider.Provider
	UpstreamModel    string
	UpstreamProtocol ir.Protocol
	Kind             Kind
	Candidates       []Route
	Fallbacks        []Route
	FallbackStatuses []int
	Alias            string
}

type Resolver interface {
	Resolve(context.Context, string, ir.Protocol) (Route, error)
	ResolveCandidates(context.Context, string, ir.Protocol) ([]Route, error)
	AdvanceCycle(failed string)
}

// VirtualOptions configures virtual aliases (automode, cycle) in the resolver.
type VirtualOptions struct {
	AutoModel              string
	AutoModelChatResponses bool
	CycleModels            []string
	CycleIndex             func() int
	AdvanceCycle           func(failed string)
}

var (
	_ Resolver = (*LegacyResolver)(nil)
	_ Resolver = (*RegistryResolver)(nil)
)

type resolvedRoute struct {
	route            Route
	alias            string
	hidden           bool
	fallbacks        []string
	fallbackStatuses []int
}

type snapshot struct {
	routes  map[string]resolvedRoute
	hidden  map[string]bool
	virtual VirtualOptions
}

// LegacyResolver preserves legacy flat-list precedence even when FromLegacy
// groups several nonadjacent entries into one provider. Only publication copies
// configuration; Resolve performs a single immutable snapshot lookup.
type LegacyResolver struct {
	mu                 sync.Mutex
	current            atomic.Pointer[snapshot]
	internalCycleIndex atomic.Int64
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
	var prevVirtual VirtualOptions
	if cur := r.current.Load(); cur != nil {
		prevVirtual = cur.virtual
	}
	next := &snapshot{
		routes:  make(map[string]resolvedRoute, 2*len(list)),
		hidden:  make(map[string]bool, len(hidden)),
		virtual: prevVirtual,
	}
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
	var prevVirtual VirtualOptions
	if cur := r.current.Load(); cur != nil {
		prevVirtual = cur.virtual
	}
	next := &snapshot{
		routes:  make(map[string]resolvedRoute, 2*len(rows)),
		hidden:  make(map[string]bool, len(hidden)),
		virtual: prevVirtual,
	}
	for _, id := range hidden {
		next.hidden[id] = true
	}
	for _, current := range rows {
		mapping, p := current.mapping, current.provider
		entry := resolvedRoute{
			route: Route{
				Provider:         p,
				UpstreamModel:    mapping.UpstreamModel,
				UpstreamProtocol: p.Protocol,
				Kind:             Custom,
				Alias:            mapping.Alias,
				FallbackStatuses: slices.Clone(mapping.FallbackStatuses),
			},
			alias:            mapping.Alias,
			hidden:           mapping.Hidden,
			fallbacks:        slices.Clone(mapping.Fallbacks),
			fallbackStatuses: slices.Clone(mapping.FallbackStatuses),
		}
		for _, model := range []string{mapping.Alias, mapping.UpstreamModel} {
			if _, exists := next.routes[model]; !exists {
				next.routes[model] = entry
			}
		}
	}
	r.current.Store(next)
	return nil
}

// UpdateVirtual updates the virtual alias configuration (automode, cycle).
// When cycle models change, the sticky cycle index is reset to 0.
func (r *LegacyResolver) UpdateVirtual(opts VirtualOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current.Load()
	if current == nil {
		current = &snapshot{routes: make(map[string]resolvedRoute), hidden: make(map[string]bool)}
	}
	if !slices.Equal(current.virtual.CycleModels, opts.CycleModels) {
		r.internalCycleIndex.Store(0)
	}
	next := *current
	next.virtual = opts
	r.current.Store(&next)
}

// AdvanceCycle advances the sticky cycle index past the failed candidate model.
func (r *LegacyResolver) AdvanceCycle(failed string) {
	current := r.current.Load()
	if current == nil {
		return
	}
	if current.virtual.AdvanceCycle != nil {
		current.virtual.AdvanceCycle(failed)
		return
	}
	cycleModels := current.virtual.CycleModels
	if len(cycleModels) == 0 {
		return
	}
	idx := int(r.internalCycleIndex.Load()) % len(cycleModels)
	if idx < 0 {
		idx = 0
	}
	if cycleModels[idx] == failed {
		r.internalCycleIndex.Add(1)
	}
}

func (r *LegacyResolver) resolveEntry(current *snapshot, clientModel string) (Route, []string, error) {
	if current.hidden[clientModel] {
		return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
	}
	entry, found := current.routes[clientModel]
	if !found {
		return Route{Kind: Fallthrough, UpstreamModel: clientModel, Alias: clientModel}, nil, nil
	}
	if entry.hidden || current.hidden[clientModel] || current.hidden[entry.alias] {
		return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
	}
	return entry.route, entry.fallbacks, nil
}

func (r *LegacyResolver) resolveConcrete(ctx context.Context, current *snapshot, clientModel string, _ ir.Protocol) (Route, error) {
	return r.resolveWithFallbacks(ctx, current, clientModel, make(map[string]bool))
}

func (r *LegacyResolver) resolveWithFallbacks(ctx context.Context, current *snapshot, clientModel string, seen map[string]bool) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	route, fallbacksList, err := r.resolveEntry(current, clientModel)
	if err != nil {
		return Route{}, err
	}
	if route.Kind != Custom {
		return route, nil
	}
	seen[clientModel] = true
	if route.Alias != "" {
		seen[route.Alias] = true
	}
	var fallbacks []Route
	var resolveHop func(alias string)
	resolveHop = func(alias string) {
		alias = strings.TrimSpace(alias)
		if alias == "" || seen[alias] {
			return
		}
		seen[alias] = true
		fbRoute, nestedFbs, fbErr := r.resolveEntry(current, alias)
		if fbErr == nil && fbRoute.Kind == Custom && fbRoute.Provider != nil {
			fbRoute.Alias = alias
			fallbacks = append(fallbacks, fbRoute)
			for _, nextAlias := range nestedFbs {
				resolveHop(nextAlias)
			}
		}
	}
	for _, fbAlias := range fallbacksList {
		resolveHop(fbAlias)
	}
	route.Fallbacks = fallbacks
	return route, nil
}

func (r *LegacyResolver) resolveCycle(ctx context.Context, current *snapshot, protocol ir.Protocol) (Route, error) {
	cycleModels := current.virtual.CycleModels
	if len(cycleModels) == 0 {
		return Route{}, &gwerr.Error{Kind: gwerr.KindOverloaded, Status: http.StatusServiceUnavailable, Message: "no available cycle models; configure cycle in the gateway"}
	}
	start := 0
	if current.virtual.CycleIndex != nil {
		start = current.virtual.CycleIndex()
	} else {
		start = int(r.internalCycleIndex.Load())
	}
	n := len(cycleModels)
	if start < 0 || start >= n {
		start = 0
	}
	var candidates []Route
	for offset := 0; offset < n; offset++ {
		id := cycleModels[(start+offset)%n]
		if id == VirtualCycle || id == VirtualAutoMode {
			continue
		}
		if current.hidden[id] {
			continue
		}
		route, err := r.resolveConcrete(ctx, current, id, protocol)
		if err != nil {
			continue
		}
		route.Candidates = []Route{route}
		candidates = append(candidates, route)
	}
	if len(candidates) == 0 {
		return Route{}, &gwerr.Error{Kind: gwerr.KindOverloaded, Status: http.StatusServiceUnavailable, Message: "no available cycle models; configure cycle in the gateway"}
	}
	first := candidates[0]
	first.Candidates = candidates
	return first, nil
}

func (r *LegacyResolver) Resolve(ctx context.Context, clientModel string, protocol ir.Protocol) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if strings.TrimSpace(clientModel) == "" {
		return Route{}, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "model must not be empty"}
	}
	current := r.current.Load()
	if current == nil {
		route := Route{Kind: Fallthrough, UpstreamModel: clientModel}
		route.Candidates = []Route{route}
		return route, nil
	}
	if clientModel == VirtualAutoMode {
		isAllowed := protocol == ir.ProtocolAnthropicMessages ||
			(current.virtual.AutoModelChatResponses &&
				(protocol == ir.ProtocolOpenAIChat || protocol == ir.ProtocolOpenAIResponses))
		if !isAllowed {
			route := Route{Kind: Fallthrough, UpstreamModel: VirtualAutoMode}
			route.Candidates = []Route{route}
			return route, nil
		}
		target := current.virtual.AutoModel
		if target == "" {
			target = models.DefaultModel
		}
		if current.hidden[target] {
			return Route{}, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "automode target is hidden"}
		}
		if entry, ok := current.routes[target]; ok && (entry.hidden || current.hidden[entry.alias]) {
			return Route{}, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "automode target is hidden"}
		}
		if target == VirtualAutoMode {
			return Route{}, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "invalid automode target"}
		}
		if target == VirtualCycle {
			return r.resolveCycle(ctx, current, protocol)
		}
		route, err := r.resolveConcrete(ctx, current, target, protocol)
		if err != nil {
			return Route{}, err
		}
		route.Candidates = []Route{route}
		return route, nil
	}
	if clientModel == VirtualCycle {
		return r.resolveCycle(ctx, current, protocol)
	}
	route, err := r.resolveConcrete(ctx, current, clientModel, protocol)
	if err != nil {
		return Route{}, err
	}
	route.Candidates = []Route{route}
	return route, nil
}

func (r *LegacyResolver) ResolveCandidates(ctx context.Context, clientModel string, protocol ir.Protocol) ([]Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	route, err := r.Resolve(ctx, clientModel, protocol)
	if err != nil {
		return nil, err
	}
	if len(route.Candidates) > 0 {
		return route.Candidates, nil
	}
	return []Route{route}, nil
}

// ----------------------------------------------------------------------------
// Registry-backed Resolver (v2)
// ----------------------------------------------------------------------------

type wildcardEntry struct {
	prefix           string
	alias            string
	upstreamModel    string
	provider         *provider.Provider
	protocol         ir.Protocol
	hidden           bool
	allowShadow      bool
	fallbacks        []string
	fallbackStatuses []int
}

type registrySnapshot struct {
	exactRoutes       map[string]resolvedRoute
	wildcards         []wildcardEntry
	bareRoutes        map[string]resolvedRoute
	hidden            map[string]bool
	disableBareModels bool
	allowNvidiaShadow bool
	isProtected       func(string) bool
	warnings          []string
	virtual           VirtualOptions
}

// RegistryOptions configures RegistryResolver behavior.
type RegistryOptions struct {
	DisableBareModels bool
	AllowNvidiaShadow bool
	IsProtectedModel  func(string) bool
	OnWarning         func(string)
	Virtual           VirtualOptions
}

type ResolverOption func(*RegistryOptions)

func WithVirtualOptions(opts VirtualOptions) ResolverOption {
	return func(o *RegistryOptions) {
		o.Virtual = opts
	}
}

func WithDisableBareModels(disable bool) ResolverOption {
	return func(o *RegistryOptions) {
		o.DisableBareModels = disable
	}
}

func WithAllowNvidiaShadow(allow bool) ResolverOption {
	return func(o *RegistryOptions) {
		o.AllowNvidiaShadow = allow
	}
}

func WithProtectedModelFunc(fn func(string) bool) ResolverOption {
	return func(o *RegistryOptions) {
		o.IsProtectedModel = fn
	}
}

func WithWarningFunc(fn func(string)) ResolverOption {
	return func(o *RegistryOptions) {
		o.OnWarning = fn
	}
}

func defaultIsProtected(model string) bool {
	_, ok := models.Models[model]
	return ok
}

func shadowsProtectedNvidiaModel(prefix string, isProtected func(string) bool) (string, bool) {
	for m := range models.Models {
		if strings.HasPrefix(m, prefix) {
			if isProtected == nil || isProtected(m) {
				return m, true
			}
		}
	}
	return "", false
}

// CollisionError reports a collision or unauthorized shadowing detected during save-time validation.
type CollisionError struct {
	Kind     string // "alias", "wildcard", "shadow"
	Alias    string
	Provider string
	Conflict string
	Message  string
}

func (e *CollisionError) Error() string {
	return e.Message
}

// CheckCollisions validates providers for alias collisions, wildcard collisions,
// and unauthorized NVIDIA model shadowing at save time.
func CheckCollisions(list []provider.Provider, opts ...ResolverOption) error {
	var options RegistryOptions
	for _, opt := range opts {
		opt(&options)
	}
	isProtected := options.IsProtectedModel
	if isProtected == nil {
		isProtected = defaultIsProtected
	}

	seenExact := make(map[string]string)
	seenWildcard := make(map[string]string)

	for i := range list {
		p := &list[i]
		if !p.Enabled {
			continue
		}
		if err := p.Validate(); err != nil {
			return err
		}
		for _, m := range p.Models {
			alias := m.Alias
			allowShadow := m.AllowShadow || options.AllowNvidiaShadow

			if strings.HasSuffix(alias, "/*") {
				prefix := strings.TrimSuffix(alias, "*")
				if prevID, exists := seenWildcard[prefix]; exists {
					return &CollisionError{
						Kind:     "wildcard",
						Alias:    alias,
						Provider: p.ID,
						Conflict: prevID,
						Message:  fmt.Sprintf("wildcard collision on %q: claimed by provider %q and %q", alias, prevID, p.ID),
					}
				}
				seenWildcard[prefix] = p.ID

				if !allowShadow {
					if shadowed, ok := shadowsProtectedNvidiaModel(prefix, isProtected); ok {
						return &CollisionError{
							Kind:     "shadow",
							Alias:    alias,
							Provider: p.ID,
							Conflict: shadowed,
							Message:  fmt.Sprintf("custom wildcard %q shadows protected NVIDIA model %q without allow_shadow", alias, shadowed),
						}
					}
				}
			} else {
				if prevID, exists := seenExact[alias]; exists {
					if m.LegacyPosition == 0 {
						return &CollisionError{
							Kind:     "alias",
							Alias:    alias,
							Provider: p.ID,
							Conflict: prevID,
							Message:  fmt.Sprintf("alias collision on %q: claimed by provider %q and %q", alias, prevID, p.ID),
						}
					}
				}
				seenExact[alias] = p.ID

				if !allowShadow && isProtected(alias) {
					return &CollisionError{
						Kind:     "shadow",
						Alias:    alias,
						Provider: p.ID,
						Conflict: alias,
						Message:  fmt.Sprintf("custom alias %q shadows protected NVIDIA model %q without allow_shadow", alias, alias),
					}
				}
			}
		}
	}
	return nil
}

// RegistryResolver implements the registry-backed model resolver built on the v2
// provider store, supporting exact aliases, wildcard mappings, and collision precedence.
type RegistryResolver struct {
	mu                 sync.Mutex
	current            atomic.Pointer[registrySnapshot]
	opts               RegistryOptions
	internalCycleIndex atomic.Int64
}

// NewRegistryResolver creates a new RegistryResolver from a list of providers and hidden IDs.
func NewRegistryResolver(list []provider.Provider, hidden []string, opts ...ResolverOption) (*RegistryResolver, error) {
	r := &RegistryResolver{}
	for _, opt := range opts {
		opt(&r.opts)
	}
	if err := r.UpdateProviders(list, hidden); err != nil {
		return nil, err
	}
	return r, nil
}

// Validate performs save-time collision check against configured options.
func (r *RegistryResolver) Validate(providers []provider.Provider) error {
	return CheckCollisions(providers, func(o *RegistryOptions) {
		*o = r.opts
	})
}

// Warnings returns all warnings emitted during the most recent snapshot build.
func (r *RegistryResolver) Warnings() []string {
	current := r.current.Load()
	if current == nil {
		return nil
	}
	return append([]string(nil), current.warnings...)
}

// Update converts legacy providers to v2 and updates the routing snapshot.
func (r *RegistryResolver) Update(list []provider.LegacyProvider, hidden []string) error {
	converted, err := provider.FromLegacy(uniqueLegacyAliases(list))
	if err != nil {
		return err
	}
	return r.UpdateProviders(converted, hidden)
}

// UpdateProviders validates provider configurations and precomputes immutable lookup maps.
// Collisions at load time produce warnings and pick a deterministic winner (earlier configuration order).
func (r *RegistryResolver) UpdateProviders(list []provider.Provider, hidden []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	isProtected := r.opts.IsProtectedModel
	if isProtected == nil {
		isProtected = defaultIsProtected
	}

	owned := make([]provider.Provider, len(list))
	type row struct {
		provider *provider.Provider
		mapping  *provider.ModelMapping
		index    int
	}
	var rows []row
	rowIndex := 0
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
			rows = append(rows, row{provider: p, mapping: &p.Models[j], index: rowIndex})
			rowIndex++
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
		if first != 0 && second != 0 {
			if c := cmp.Compare(first, second); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.index, b.index)
	})

	var warnings []string
	addWarning := func(msg string) {
		warnings = append(warnings, msg)
		if r.opts.OnWarning != nil {
			r.opts.OnWarning(msg)
		}
	}

	exactRoutes := make(map[string]resolvedRoute, len(rows))
	bareRoutes := make(map[string]resolvedRoute, len(rows))
	seenWildcards := make(map[string]string)
	var wildcards []wildcardEntry

	for _, current := range rows {
		mapping, p := current.mapping, current.provider
		alias := mapping.Alias
		allowShadow := mapping.AllowShadow || r.opts.AllowNvidiaShadow

		if strings.HasSuffix(alias, "/*") {
			prefix := strings.TrimSuffix(alias, "*")
			if winnerID, exists := seenWildcards[prefix]; exists {
				addWarning(fmt.Sprintf("wildcard collision on %q: provider %q conflicts with %q; keeping %q", alias, p.ID, winnerID, winnerID))
				continue
			}
			seenWildcards[prefix] = p.ID

			if !allowShadow {
				if shadowed, ok := shadowsProtectedNvidiaModel(prefix, isProtected); ok {
					addWarning(fmt.Sprintf("wildcard %q matches protected NVIDIA model %q without allow_shadow; NVIDIA models will take precedence", alias, shadowed))
				}
			}

			wildcards = append(wildcards, wildcardEntry{
				prefix:           prefix,
				alias:            alias,
				upstreamModel:    mapping.UpstreamModel,
				provider:         p,
				protocol:         p.Protocol,
				hidden:           mapping.Hidden,
				allowShadow:      allowShadow,
				fallbacks:        slices.Clone(mapping.Fallbacks),
				fallbackStatuses: slices.Clone(mapping.FallbackStatuses),
			})
		} else {
			if !allowShadow && isProtected(alias) {
				addWarning(fmt.Sprintf("custom alias %q shadows protected NVIDIA model without allow_shadow; ignoring custom alias", alias))
			} else {
				if existing, exists := exactRoutes[alias]; exists {
					winnerID := existing.route.Provider.ID
					addWarning(fmt.Sprintf("alias collision on %q: provider %q conflicts with %q; keeping %q", alias, p.ID, winnerID, winnerID))
				} else {
					exactRoutes[alias] = resolvedRoute{
						route: Route{
							Provider:         p,
							UpstreamModel:    mapping.UpstreamModel,
							UpstreamProtocol: p.Protocol,
							Kind:             Custom,
							Alias:            alias,
							FallbackStatuses: slices.Clone(mapping.FallbackStatuses),
						},
						alias:            alias,
						hidden:           mapping.Hidden,
						fallbacks:        slices.Clone(mapping.Fallbacks),
						fallbackStatuses: slices.Clone(mapping.FallbackStatuses),
					}
				}
			}
		}

		if mapping.UpstreamModel != "" && !strings.Contains(mapping.UpstreamModel, "*") {
			bare := mapping.UpstreamModel
			if !allowShadow && isProtected(bare) {
				// Protected NVIDIA model not shadowed bare
			} else if _, exists := bareRoutes[bare]; !exists {
				bareRoutes[bare] = resolvedRoute{
					route: Route{
						Provider:         p,
						UpstreamModel:    mapping.UpstreamModel,
						UpstreamProtocol: p.Protocol,
						Kind:             Custom,
						Alias:            alias,
						FallbackStatuses: slices.Clone(mapping.FallbackStatuses),
					},
					alias:            alias,
					hidden:           mapping.Hidden,
					fallbacks:        slices.Clone(mapping.Fallbacks),
					fallbackStatuses: slices.Clone(mapping.FallbackStatuses),
				}
			}
		}
	}

	// Longest prefix match: sort wildcards by descending prefix length
	slices.SortStableFunc(wildcards, func(a, b wildcardEntry) int {
		return cmp.Compare(len(b.prefix), len(a.prefix))
	})

	hiddenMap := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		hiddenMap[id] = true
	}

	var prevVirtual VirtualOptions
	if cur := r.current.Load(); cur != nil {
		prevVirtual = cur.virtual
	} else {
		prevVirtual = r.opts.Virtual
	}

	next := &registrySnapshot{
		exactRoutes:       exactRoutes,
		wildcards:         wildcards,
		bareRoutes:        bareRoutes,
		hidden:            hiddenMap,
		disableBareModels: r.opts.DisableBareModels,
		allowNvidiaShadow: r.opts.AllowNvidiaShadow,
		isProtected:       isProtected,
		warnings:          warnings,
		virtual:           prevVirtual,
	}

	r.current.Store(next)
	return nil
}

// UpdateVirtual updates the virtual alias configuration (automode, cycle).
// When cycle models change, the sticky cycle index is reset to 0.
func (r *RegistryResolver) UpdateVirtual(opts VirtualOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current.Load()
	if current == nil {
		current = &registrySnapshot{
			exactRoutes: make(map[string]resolvedRoute),
			bareRoutes:  make(map[string]resolvedRoute),
			hidden:      make(map[string]bool),
		}
	}
	if !slices.Equal(current.virtual.CycleModels, opts.CycleModels) {
		r.internalCycleIndex.Store(0)
	}
	next := *current
	next.virtual = opts
	r.current.Store(&next)
}

// AdvanceCycle advances the sticky cycle index past the failed candidate model.
func (r *RegistryResolver) AdvanceCycle(failed string) {
	current := r.current.Load()
	if current == nil {
		return
	}
	if current.virtual.AdvanceCycle != nil {
		current.virtual.AdvanceCycle(failed)
		return
	}
	cycleModels := current.virtual.CycleModels
	if len(cycleModels) == 0 {
		return
	}
	idx := int(r.internalCycleIndex.Load()) % len(cycleModels)
	if idx < 0 {
		idx = 0
	}
	if cycleModels[idx] == failed {
		r.internalCycleIndex.Add(1)
	}
}

func (r *RegistryResolver) resolveEntry(current *registrySnapshot, clientModel string) (Route, []string, error) {
	if current.hidden[clientModel] {
		return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
	}

	// 1. Exact alias match (O(1))
	if entry, ok := current.exactRoutes[clientModel]; ok {
		if entry.hidden || current.hidden[clientModel] || current.hidden[entry.alias] {
			return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
		}
		return entry.route, entry.fallbacks, nil
	}

	// 2. Wildcard prefix match (O(prefixes))
	for i := range current.wildcards {
		wc := &current.wildcards[i]
		if !strings.HasPrefix(clientModel, wc.prefix) || len(clientModel) <= len(wc.prefix) {
			continue
		}
		if !wc.allowShadow && !current.allowNvidiaShadow && current.isProtected != nil && current.isProtected(clientModel) {
			// Protected NVIDIA model wins over unprivileged wildcard
			continue
		}
		if wc.hidden || current.hidden[clientModel] || current.hidden[wc.alias] {
			return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
		}
		suffix := clientModel[len(wc.prefix):]
		upstreamModel := suffix
		if wc.upstreamModel != "" && wc.upstreamModel != "*" {
			if strings.HasSuffix(wc.upstreamModel, "/*") {
				upstreamModel = strings.TrimSuffix(wc.upstreamModel, "*") + suffix
			} else {
				upstreamModel = wc.upstreamModel
			}
		}
		return Route{
			Provider:         wc.provider,
			UpstreamModel:    upstreamModel,
			UpstreamProtocol: wc.protocol,
			Kind:             Custom,
			Alias:            clientModel,
			FallbackStatuses: slices.Clone(wc.fallbackStatuses),
		}, wc.fallbacks, nil
	}

	// 3. Bare upstream-name match (O(1), if enabled)
	if !current.disableBareModels {
		if entry, ok := current.bareRoutes[clientModel]; ok {
			if entry.hidden || current.hidden[clientModel] || current.hidden[entry.alias] {
				return Route{}, nil, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "model is hidden"}
			}
			return entry.route, entry.fallbacks, nil
		}
	}

	// 4. Fallthrough
	return Route{Kind: Fallthrough, UpstreamModel: clientModel, Alias: clientModel}, nil, nil
}

func (r *RegistryResolver) resolveConcrete(ctx context.Context, current *registrySnapshot, clientModel string, _ ir.Protocol) (Route, error) {
	return r.resolveWithFallbacks(ctx, current, clientModel, make(map[string]bool))
}

func (r *RegistryResolver) resolveWithFallbacks(ctx context.Context, current *registrySnapshot, clientModel string, seen map[string]bool) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	route, fallbackAliases, err := r.resolveEntry(current, clientModel)
	if err != nil {
		return Route{}, err
	}
	if route.Kind != Custom {
		return route, nil
	}
	seen[clientModel] = true
	if route.Alias != "" {
		seen[route.Alias] = true
	}
	var fallbacks []Route
	var resolveHop func(alias string)
	resolveHop = func(alias string) {
		alias = strings.TrimSpace(alias)
		if alias == "" || seen[alias] {
			return
		}
		seen[alias] = true
		fbRoute, nestedFbs, fbErr := r.resolveEntry(current, alias)
		if fbErr == nil && fbRoute.Kind == Custom && fbRoute.Provider != nil {
			fbRoute.Alias = alias
			fallbacks = append(fallbacks, fbRoute)
			for _, nextAlias := range nestedFbs {
				resolveHop(nextAlias)
			}
		}
	}
	for _, fbAlias := range fallbackAliases {
		resolveHop(fbAlias)
	}
	route.Fallbacks = fallbacks
	return route, nil
}

func (r *RegistryResolver) resolveCycle(ctx context.Context, current *registrySnapshot, protocol ir.Protocol) (Route, error) {
	cycleModels := current.virtual.CycleModels
	if len(cycleModels) == 0 {
		return Route{}, &gwerr.Error{Kind: gwerr.KindOverloaded, Status: http.StatusServiceUnavailable, Message: "no available cycle models; configure cycle in the gateway"}
	}
	start := 0
	if current.virtual.CycleIndex != nil {
		start = current.virtual.CycleIndex()
	} else {
		start = int(r.internalCycleIndex.Load())
	}
	n := len(cycleModels)
	if start < 0 || start >= n {
		start = 0
	}
	var candidates []Route
	for offset := 0; offset < n; offset++ {
		id := cycleModels[(start+offset)%n]
		if id == VirtualCycle || id == VirtualAutoMode {
			continue
		}
		if current.hidden[id] {
			continue
		}
		route, err := r.resolveConcrete(ctx, current, id, protocol)
		if err != nil {
			continue
		}
		route.Candidates = []Route{route}
		candidates = append(candidates, route)
	}
	if len(candidates) == 0 {
		return Route{}, &gwerr.Error{Kind: gwerr.KindOverloaded, Status: http.StatusServiceUnavailable, Message: "no available cycle models; configure cycle in the gateway"}
	}
	first := candidates[0]
	first.Candidates = candidates
	return first, nil
}

// Resolve maps a requested model to its route using strict precedence:
// Exact alias match > Wildcard prefix match > Bare upstream-name match > Fallthrough.
// Virtual models (automode, cycle) are resolved to their target or active candidate.
func (r *RegistryResolver) Resolve(ctx context.Context, clientModel string, protocol ir.Protocol) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if strings.TrimSpace(clientModel) == "" {
		return Route{}, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "model must not be empty"}
	}
	current := r.current.Load()
	if current == nil {
		route := Route{Kind: Fallthrough, UpstreamModel: clientModel}
		route.Candidates = []Route{route}
		return route, nil
	}

	if clientModel == VirtualAutoMode {
		// Decision recorded: By default, automode only routes on Anthropic Messages protocol (/v1/messages).
		// Chat/Responses protocols fall through to preserve default OpenAI behavior unless AutoModelChatResponses is configured.
		isAllowed := protocol == ir.ProtocolAnthropicMessages ||
			(current.virtual.AutoModelChatResponses &&
				(protocol == ir.ProtocolOpenAIChat || protocol == ir.ProtocolOpenAIResponses))
		if !isAllowed {
			route := Route{Kind: Fallthrough, UpstreamModel: VirtualAutoMode}
			route.Candidates = []Route{route}
			return route, nil
		}
		target := current.virtual.AutoModel
		if target == "" {
			target = models.DefaultModel
		}
		if current.hidden[target] {
			return Route{}, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "automode target is hidden"}
		}
		if entry, ok := current.exactRoutes[target]; ok && (entry.hidden || current.hidden[entry.alias]) {
			return Route{}, &gwerr.Error{Kind: gwerr.KindNotFound, Message: "automode target is hidden"}
		}
		if target == VirtualAutoMode {
			return Route{}, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "invalid automode target"}
		}
		if target == VirtualCycle {
			return r.resolveCycle(ctx, current, protocol)
		}
		route, err := r.resolveConcrete(ctx, current, target, protocol)
		if err != nil {
			return Route{}, err
		}
		route.Candidates = []Route{route}
		return route, nil
	}

	if clientModel == VirtualCycle {
		return r.resolveCycle(ctx, current, protocol)
	}

	route, err := r.resolveConcrete(ctx, current, clientModel, protocol)
	if err != nil {
		return Route{}, err
	}
	route.Candidates = []Route{route}
	return route, nil
}

// ResolveCandidates returns an ordered list of candidate routes for virtual models (e.g. cycle)
// or a single-element slice containing the resolved route for non-virtual targets.
func (r *RegistryResolver) ResolveCandidates(ctx context.Context, clientModel string, protocol ir.Protocol) ([]Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	route, err := r.Resolve(ctx, clientModel, protocol)
	if err != nil {
		return nil, err
	}
	if len(route.Candidates) > 0 {
		return route.Candidates, nil
	}
	return []Route{route}, nil
}
