package providerstore

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// Account views are cached per (user, config version) for a short TTL. They are
// built lazily and never swept: a config write bumps the snapshot version, so
// old entries are simply never routed from again and expire by TTL or LRU.
//
// ponytail: per-process cache. Another gateway process keeps its own copy; only
// revisit if cross-instance hit rate ever matters.
const (
	accountViewCapacity = 1024
	accountViewTTL      = 60 * time.Second
)

// VisibleModel is one alias or wildcard pattern an account can reach. It
// carries identity only: never key ids, hashes, ciphertext or secret_last4.
type VisibleModel struct {
	Alias         string
	ProviderID    string
	UpstreamModel string
	// allowShadow applies to wildcard patterns only: a protected model may be
	// served only when the mapping opted in. The check runs against the
	// requested model at lookup time, never against the pattern string.
	allowShadow bool
}

// accountView is one account's visible-model view for one config version.
// Aliases are held in request-time precedence tiers: personal exact, personal
// wildcard, shared exact, shared wildcard. retired records aliases that were
// visible in the account's previous version and disappeared; it exists so a
// formerly mapped alias can answer one typed 404 and then be dropped.
type accountView struct {
	version          int64
	personalExact    map[string]VisibleModel
	personalWildcard []VisibleModel
	sharedExact      map[string]VisibleModel
	sharedWildcard   []VisibleModel
	hidden           map[string]bool
	retired          map[string]VisibleModel
	mu               sync.Mutex
}

// newAccountView builds a view from the snapshot. Exact aliases reuse
// AvailableModels, which already applies model_state hidden/deleted, hidden
// mappings, protected shadows, credential failures and personal-over-shared.
func newAccountView(version int64, snapshot *Snapshot, userID string, previous *accountView) *accountView {
	view := &accountView{
		version:          version,
		personalExact:    make(map[string]VisibleModel),
		personalWildcard: snapshot.visibleWildcards(snapshot.personalWc[userID]),
		sharedExact:      make(map[string]VisibleModel),
		sharedWildcard:   snapshot.visibleWildcards(snapshot.sharedWc),
		hidden:           make(map[string]bool, len(snapshot.modelState)),
		retired:          make(map[string]VisibleModel),
	}
	for _, model := range snapshot.AvailableModels(userID, false) {
		entry := VisibleModel{Alias: model.Alias, ProviderID: model.ProviderID, UpstreamModel: model.UpstreamModel}
		if model.OwnerID == "" {
			view.sharedExact[model.Alias] = entry
		} else {
			view.personalExact[model.Alias] = entry
		}
	}
	for id, state := range snapshot.modelState {
		if state.Hidden || state.Deleted {
			view.hidden[id] = true
		}
	}
	if previous != nil && previous.version != version {
		view.retired = view.retiredFrom(previous)
	}
	return view
}

func (s *Snapshot) visibleWildcards(candidates []*candidate) []VisibleModel {
	out := make([]VisibleModel, 0, len(candidates))
	for _, c := range candidates {
		if c.hidden || c.credential != nil {
			continue
		}
		out = append(out, VisibleModel{
			Alias:         c.alias,
			ProviderID:    c.providerID,
			UpstreamModel: c.upstreamModel,
			allowShadow:   c.allowShadow,
		})
	}
	return out
}

// retiredFrom records entries the previous view served that the new view no
// longer reaches. It is computed once at build: old views are never routed
// from after a version change.
func (v *accountView) retiredFrom(previous *accountView) map[string]VisibleModel {
	previous.mu.Lock()
	defer previous.mu.Unlock()
	out := make(map[string]VisibleModel)
	retireExact := func(alias string, model VisibleModel) {
		if _, _, ok := v.lookup(alias, nil); !ok {
			out[alias] = model
		}
	}
	for alias, model := range previous.personalExact {
		retireExact(alias, model)
	}
	for alias, model := range previous.sharedExact {
		retireExact(alias, model)
	}
	for _, model := range previous.personalWildcard {
		if !v.hasPattern(model.Alias) {
			out[model.Alias] = model
		}
	}
	for _, model := range previous.sharedWildcard {
		if !v.hasPattern(model.Alias) {
			out[model.Alias] = model
		}
	}
	return out
}

func (v *accountView) hasPattern(alias string) bool {
	for _, model := range v.personalWildcard {
		if model.Alias == alias {
			return true
		}
	}
	for _, model := range v.sharedWildcard {
		if model.Alias == alias {
			return true
		}
	}
	return false
}

// lookup resolves the alias against the cached view in request-time precedence
// order. It returns the serving entry and its key so eviction drops the exact
// alias or the wildcard pattern that actually served the request.
func (v *accountView) lookup(model string, isProtected func(string) bool) (VisibleModel, string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lookupLocked(model, isProtected)
}

func (v *accountView) lookupLocked(model string, isProtected func(string) bool) (VisibleModel, string, bool) {
	if v.hidden[model] {
		return VisibleModel{}, "", false
	}
	if entry, ok := v.personalExact[model]; ok {
		return entry, model, true
	}
	if entry, ok := matchWildcard(v.personalWildcard, model, isProtected); ok {
		return entry, entry.Alias, true
	}
	if entry, ok := v.sharedExact[model]; ok {
		return entry, model, true
	}
	if entry, ok := matchWildcard(v.sharedWildcard, model, isProtected); ok {
		return entry, entry.Alias, true
	}
	return VisibleModel{}, "", false
}

// matchWildcard returns the longest eligible pattern match. Entries are kept
// in descending pattern length, so the first eligible one wins, mirroring
// pickWildcard.
func matchWildcard(entries []VisibleModel, model string, isProtected func(string) bool) (VisibleModel, bool) {
	for _, entry := range entries {
		prefix := strings.TrimSuffix(entry.Alias, "*")
		if !strings.HasPrefix(model, prefix) || len(model) <= len(prefix) {
			continue
		}
		if isProtected != nil && isProtected(model) && !entry.allowShadow {
			continue
		}
		entry.UpstreamModel = wildcardUpstreamValue(entry.UpstreamModel, model[len(prefix):])
		return entry, true
	}
	return VisibleModel{}, false
}

// retiredKey returns the retired entry still matching the requested alias.
func (v *accountView) retiredKey(model string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	bestKey := ""
	bestPrefix := -1
	for key := range v.retired {
		prefix := strings.TrimSuffix(key, "*")
		if key == prefix {
			if key == model {
				return key, true
			}
			continue
		}
		if strings.HasPrefix(model, prefix) && len(model) > len(prefix) && len(prefix) > bestPrefix {
			bestKey, bestPrefix = key, len(prefix)
		}
	}
	return bestKey, bestKey != ""
}

// evict drops the entry that served a request whose mapping stopped resolving.
// Lazy and per-request: config writes never fan out or sweep.
func (v *accountView) evict(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.personalExact, key)
	delete(v.sharedExact, key)
	delete(v.retired, key)
	v.personalWildcard = dropPattern(v.personalWildcard, key)
	v.sharedWildcard = dropPattern(v.sharedWildcard, key)
}

func dropPattern(entries []VisibleModel, alias string) []VisibleModel {
	for i, entry := range entries {
		if entry.Alias == alias {
			return append(entries[:i], entries[i+1:]...)
		}
	}
	return entries
}

// contains reports whether the view currently reaches alias. Used by tests and
// diagnostics; routing also gets the serving key from lookup.
func (v *accountView) contains(alias string) bool {
	_, _, ok := v.lookup(alias, nil)
	return ok
}

func (v *accountView) snapshot() map[string]VisibleModel {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]VisibleModel, len(v.personalExact)+len(v.sharedExact)+len(v.personalWildcard)+len(v.sharedWildcard)+len(v.retired))
	for alias, model := range v.personalExact {
		out[alias] = model
	}
	for alias, model := range v.sharedExact {
		out[alias] = model
	}
	for _, model := range v.personalWildcard {
		out[model.Alias] = model
	}
	for _, model := range v.sharedWildcard {
		out[model.Alias] = model
	}
	for alias, model := range v.retired {
		out[alias] = model
	}
	return out
}

type viewKey struct {
	userID  string
	version int64
}

type viewEntry struct {
	view     *accountView
	storedAt time.Time
	elem     *list.Element
}

type viewCall struct {
	done chan struct{}
	view *accountView
}

// viewCache is a bounded LRU of account views with a TTL backstop. Single-flight
// per key makes a burst of requests build a view once; unrelated users never
// wait on each other.
type viewCache struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	now      func() time.Time
	entries  map[viewKey]*viewEntry
	order    *list.List
	inflight map[viewKey]*viewCall
}

func newViewCache(capacity int, now func() time.Time) *viewCache {
	if now == nil {
		now = time.Now
	}
	return &viewCache{
		max:      capacity,
		ttl:      accountViewTTL,
		now:      now,
		entries:  make(map[viewKey]*viewEntry),
		order:    list.New(),
		inflight: make(map[viewKey]*viewCall),
	}
}

// previous returns the newest cached view for the user, whatever its version.
// It is only consulted while building that user's successor view.
//
// ponytail: O(cache) scan at build time; 1024 entries is cheap and builds are
// once per user per version.
func (c *viewCache) previous(userID string) *accountView {
	c.mu.Lock()
	defer c.mu.Unlock()
	var previous *accountView
	var previousVersion int64
	for key, entry := range c.entries {
		if key.userID != userID {
			continue
		}
		if previous == nil || key.version > previousVersion {
			previous, previousVersion = entry.view, key.version
		}
	}
	return previous
}

func (c *viewCache) get(key viewKey, build func() *accountView) *accountView {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if c.now().Sub(entry.storedAt) < c.ttl {
			c.order.MoveToFront(entry.elem)
			view := entry.view
			c.mu.Unlock()
			return view
		}
		delete(c.entries, key)
		c.order.Remove(entry.elem)
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.view
	}
	call := &viewCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	view := build()

	c.mu.Lock()
	delete(c.inflight, key)
	entry := &viewEntry{view: view, storedAt: c.now()}
	entry.elem = c.order.PushFront(key)
	c.entries[key] = entry
	for len(c.entries) > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		delete(c.entries, oldest.Value.(viewKey))
		c.order.Remove(oldest)
	}
	call.view = view
	close(call.done)
	c.mu.Unlock()
	return view
}

// accountView returns the cached view for this account and the snapshot's
// version, building it on first use. Version is part of the key, so a config
// write makes old entries unreachable without a sweep.
func (r *Router) accountView(snapshot *Snapshot, userID string) *accountView {
	version := snapshot.Version()
	build := func() *accountView {
		var previous *accountView
		if r.views != nil {
			previous = r.views.previous(userID)
		}
		return newAccountView(version, snapshot, userID, previous)
	}
	if r.views == nil {
		return build()
	}
	return r.views.get(viewKey{userID: userID, version: version}, build)
}

func wildcardUpstreamValue(pattern, suffix string) string {
	if pattern == "" || pattern == "*" {
		return suffix
	}
	if strings.HasSuffix(pattern, "/*") {
		return strings.TrimSuffix(pattern, "*") + suffix
	}
	return pattern
}
