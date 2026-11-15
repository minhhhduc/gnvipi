package providerstore

import (
	"container/list"
	"sync"
	"time"
)

// Account views are cached per (user, config version) for a short TTL. They
// are built lazily and never swept: a config write bumps the snapshot version,
// so old entries are simply never read again and expire by TTL or LRU.
//
// ponytail: per-process cache. Another gateway process keeps its own copy;
// only revisit if cross-instance hit rate ever matters.
const (
	accountViewCapacity = 1024
	accountViewTTL      = 60 * time.Second
)

// VisibleModel is one alias an account can reach. It carries identity only:
// never key ids, hashes, ciphertext or secret_last4.
type VisibleModel struct {
	Alias         string
	ProviderID    string
	UpstreamModel string
}

// accountView is one account's visible-model view for one config version.
// Requests may lazily evict aliases, so models is guarded.
type accountView struct {
	version int64
	models  map[string]VisibleModel
	mu      sync.Mutex
}

func newAccountView(version int64, models map[string]VisibleModel) *accountView {
	if models == nil {
		models = make(map[string]VisibleModel)
	}
	return &accountView{version: version, models: models}
}

func (v *accountView) contains(alias string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.models[alias]
	return ok
}

// evict drops an alias whose mapping stopped resolving for this account. It is
// lazy and per-request: config writes never fan out or sweep.
func (v *accountView) evict(alias string) {
	v.mu.Lock()
	delete(v.models, alias)
	v.mu.Unlock()
}

func (v *accountView) snapshot() map[string]VisibleModel {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]VisibleModel, len(v.models))
	for alias, model := range v.models {
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
// version, building it on first use. Unknown or missing aliases never leak
// another account's mapping: the view only holds what this user can reach.
func (r *Router) accountView(snapshot *Snapshot, userID string) *accountView {
	version := snapshot.Version()
	build := func() *accountView { return newAccountView(version, snapshot.visibleModels(userID)) }
	if r.views == nil {
		return build()
	}
	return r.views.get(viewKey{userID: userID, version: version}, build)
}

// visibleModels mirrors request-time precedence for the account: personal
// exact, personal wildcard, shared exact, shared wildcard. A personal mapping
// that matched but is hidden or protected-shadowed suppresses the shared one,
// exactly as selectExactOnly does.
func (s *Snapshot) visibleModels(userID string) map[string]VisibleModel {
	out := make(map[string]VisibleModel)
	shadowed := make(map[string]bool)
	add := func(alias string, c *candidate) {
		if _, exists := out[alias]; exists {
			return
		}
		out[alias] = VisibleModel{Alias: alias, ProviderID: c.providerID, UpstreamModel: c.upstreamModel}
	}
	eligible := func(alias string, c *candidate) (matched, visible bool) {
		if s.isProtected != nil && s.isProtected(alias) && !c.allowShadow {
			return false, false
		}
		if c.hidden || c.credential != nil {
			return true, false
		}
		return true, true
	}
	for alias, list := range s.personalEx[userID] {
		for _, c := range list {
			matched, visible := eligible(alias, c)
			if !matched {
				continue
			}
			shadowed[alias] = true
			if visible {
				add(alias, c)
			}
		}
	}
	for alias, list := range s.sharedEx {
		if shadowed[alias] {
			continue
		}
		for _, c := range list {
			if _, visible := eligible(alias, c); visible {
				add(alias, c)
			}
		}
	}
	for _, c := range s.personalWc[userID] {
		if _, visible := eligible(c.alias, c); visible {
			add(c.alias, c)
		}
	}
	for _, c := range s.sharedWc {
		if _, visible := eligible(c.alias, c); visible {
			add(c.alias, c)
		}
	}
	return out
}
