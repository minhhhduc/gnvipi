package providerstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// KeyDiagnostic reports a key excluded from routing. Codes and messages never
// contain credential material.
type KeyDiagnostic struct {
	KeyID      string
	ProviderID string
	Code       string
	Message    string
}

type candidate struct {
	definition       *provider.Provider
	providerID       string
	keyID            string
	ownerID          string
	alias            string
	upstreamModel    string
	protocol         ir.Protocol
	hidden           bool
	allowShadow      bool
	shared           bool
	fallbacks        []string
	fallbackStatuses []int
	legacyPosition   int

	key         KeyRow
	secretRaw   string
	basic       BasicSecret
	credential  error
	hasResolved bool
}

// Snapshot is one fully validated, immutable configuration snapshot. Published
// snapshots are never modified; a reload builds a new value.
type Snapshot struct {
	version     int64
	loadedAt    time.Time
	providers   map[string]*provider.Provider
	personalEx  map[string]map[string][]*candidate
	personalWc  map[string][]*candidate
	sharedEx    map[string][]*candidate
	sharedWc    []*candidate
	knownEx     map[string]bool
	knownWc     []string
	modelState  map[string]ModelStateRow
	settings    Settings
	diagnostic  []KeyDiagnostic
	isProtected func(string) bool
}

// Version returns the config_state.version this snapshot was built from.
func (s *Snapshot) Version() int64 { return s.version }

// LoadedAt returns when the snapshot was published.
func (s *Snapshot) LoadedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.loadedAt
}

// Diagnostics returns a copy of key-level exclusion diagnostics.
func (s *Snapshot) Diagnostics() []KeyDiagnostic {
	if s == nil {
		return nil
	}
	return append([]KeyDiagnostic(nil), s.diagnostic...)
}

// Settings returns the validated global settings.
func (s *Snapshot) Settings() Settings {
	if s == nil {
		return Settings{}
	}
	return s.settings
}

// Provider returns a defensive copy of a provider definition.
func (s *Snapshot) Provider(id string) (provider.Provider, bool) {
	if s == nil {
		return provider.Provider{}, false
	}
	p, ok := s.providers[id]
	if !ok {
		return provider.Provider{}, false
	}
	return provider.Clone(p), true
}

// BuildOptions injects the process keyring and secret reference resolver.
type BuildOptions struct {
	Keyring     *Keyring
	Refs        RefResolver
	IsProtected func(string) bool
	Now         func() time.Time
}

// BuildSnapshot validates every row and JSONB contract before returning a
// snapshot. Any structural error prevents publication and leaves the previous
// snapshot untouched. A key whose secret cannot be decrypted is excluded from
// routing and reported as a diagnostic, never replaced by another credential.
func BuildSnapshot(rows Rows, version int64, opts BuildOptions) (*Snapshot, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if version < 0 {
		return nil, fmt.Errorf("%w: negative config version %d", ErrInvalidConfig, version)
	}
	snap := &Snapshot{
		version:     version,
		loadedAt:    now(),
		providers:   make(map[string]*provider.Provider, len(rows.Providers)),
		personalEx:  make(map[string]map[string][]*candidate),
		personalWc:  make(map[string][]*candidate),
		sharedEx:    make(map[string][]*candidate),
		knownEx:     make(map[string]bool),
		modelState:  make(map[string]ModelStateRow, len(rows.ModelState)),
		isProtected: opts.IsProtected,
	}

	// Providers first: keys and models must reference a valid definition.
	for _, row := range rows.Providers {
		if _, exists := snap.providers[row.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate provider %q", ErrInvalidConfig, row.ID)
		}
		definition, err := BuildProviderDefinition(row)
		if err != nil {
			return nil, err
		}
		if problems := ValidateSensitiveConfig(row, definition); len(problems) > 0 {
			return nil, fmt.Errorf("%w: provider %q: %s", ErrInvalidConfig, row.ID, formatProblems(problems))
		}
		snap.providers[row.ID] = &definition
	}

	keys := make(map[string]KeyRow, len(rows.Keys))
	for _, key := range rows.Keys {
		if _, exists := keys[key.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate provider key %q", ErrInvalidConfig, key.ID)
		}
		if err := ValidateKeyRow(key); err != nil {
			return nil, fmt.Errorf("%w: key %q: %v", ErrInvalidConfig, key.ID, err)
		}
		if _, ok := snap.providers[key.ProviderID]; !ok {
			return nil, fmt.Errorf("%w: key %q references unknown provider %q", ErrInvalidConfig, key.ID, key.ProviderID)
		}
		keys[key.ID] = key
	}

	for _, model := range rows.KeyModels {
		if err := ValidateKeyModelRow(model); err != nil {
			return nil, err
		}
		key, ok := keys[model.KeyID]
		if !ok {
			return nil, fmt.Errorf("%w: key model %q references unknown key %q", ErrInvalidConfig, model.Alias, model.KeyID)
		}
		if model.ProviderID != key.ProviderID {
			return nil, fmt.Errorf("%w: key model %q provider drift", ErrInvalidConfig, model.Alias)
		}
		if model.OwnerID != key.OwnerID {
			return nil, fmt.Errorf("%w: key model %q owner drift", ErrInvalidConfig, model.Alias)
		}
		definition := snap.providers[key.ProviderID]
		fallbacks, _ := decodeFallbacks(model.Fallbacks)
		statuses, _ := decodeFallbackStatuses(model.FallbackStatuses)
		c := &candidate{
			definition:       definition,
			providerID:       key.ProviderID,
			keyID:            key.ID,
			ownerID:          key.OwnerID,
			alias:            model.Alias,
			upstreamModel:    model.UpstreamModel,
			protocol:         definition.Protocol,
			hidden:           model.Hidden,
			allowShadow:      model.AllowShadow,
			shared:           key.OwnerID == "",
			fallbacks:        fallbacks,
			fallbackStatuses: statuses,
			legacyPosition:   model.LegacyPosition,
			key:              key,
		}
		if !definition.Enabled || !key.Enabled {
			continue
		}
		if key.AuthScheme != SchemeNone && key.HasSecret() {
			credential, err := ResolveKeyCredential(context.Background(), key, opts.Keyring, nil)
			if err != nil {
				c.credential = err
				snap.diagnostic = append(snap.diagnostic, KeyDiagnostic{
					KeyID:      key.ID,
					ProviderID: key.ProviderID,
					Code:       "provider_key_decrypt_failed",
					Message:    "stored provider secret cannot be decrypted; key excluded from routing",
				})
			} else {
				c.applyCredential(credential)
			}
		}
		snap.index(c)
	}

	for _, state := range rows.ModelState {
		if state.ModelID == "" {
			return nil, fmt.Errorf("%w: model_state row has empty model_id", ErrInvalidConfig)
		}
		if _, exists := snap.modelState[state.ModelID]; exists {
			return nil, fmt.Errorf("%w: duplicate model_state %q", ErrInvalidConfig, state.ModelID)
		}
		snap.modelState[state.ModelID] = state
	}

	settings, err := DecodeSettings(rows.Settings)
	if err != nil {
		return nil, err
	}
	snap.settings = settings

	snap.sort()
	return snap, nil
}

func (c *candidate) applyCredential(credential ResolvedCredential) {
	c.hasResolved = true
	c.secretRaw = credential.Raw
	c.basic = credential.Basic
}

func (s *Snapshot) index(c *candidate) {
	if strings.HasSuffix(c.alias, "/*") {
		s.knownWc = append(s.knownWc, strings.TrimSuffix(c.alias, "*"))
		if c.shared {
			s.sharedWc = append(s.sharedWc, c)
		} else {
			s.personalWc[c.ownerID] = append(s.personalWc[c.ownerID], c)
		}
		return
	}
	s.knownEx[c.alias] = true
	if c.shared {
		s.sharedEx[c.alias] = append(s.sharedEx[c.alias], c)
		return
	}
	byAlias := s.personalEx[c.ownerID]
	if byAlias == nil {
		byAlias = make(map[string][]*candidate)
		s.personalEx[c.ownerID] = byAlias
	}
	byAlias[c.alias] = append(byAlias[c.alias], c)
}

func (s *Snapshot) sort() {
	for _, byAlias := range s.personalEx {
		for alias := range byAlias {
			sortCandidates(byAlias[alias])
		}
	}
	for owner := range s.personalWc {
		sortWildcards(s.personalWc[owner])
	}
	for alias := range s.sharedEx {
		sortCandidates(s.sharedEx[alias])
	}
	sortWildcards(s.sharedWc)
	sort.SliceStable(s.knownWc, func(i, j int) bool {
		if len(s.knownWc[i]) != len(s.knownWc[j]) {
			return len(s.knownWc[i]) > len(s.knownWc[j])
		}
		return s.knownWc[i] < s.knownWc[j]
	})
	s.knownWc = dedupePrefixes(s.knownWc)
	sort.SliceStable(s.diagnostic, func(i, j int) bool {
		if s.diagnostic[i].ProviderID != s.diagnostic[j].ProviderID {
			return s.diagnostic[i].ProviderID < s.diagnostic[j].ProviderID
		}
		return s.diagnostic[i].KeyID < s.diagnostic[j].KeyID
	})
}

func sortCandidates(list []*candidate) {
	sort.SliceStable(list, func(i, j int) bool {
		return candidateLess(list[i], list[j])
	})
}

func sortWildcards(list []*candidate) {
	sort.SliceStable(list, func(i, j int) bool {
		left, right := len(list[i].alias), len(list[j].alias)
		if left != right {
			return left > right
		}
		return candidateLess(list[i], list[j])
	})
}

func candidateLess(a, b *candidate) bool {
	aPositive, bPositive := a.legacyPosition > 0, b.legacyPosition > 0
	if aPositive != bPositive {
		return aPositive
	}
	if a.legacyPosition != b.legacyPosition {
		return a.legacyPosition < b.legacyPosition
	}
	if a.providerID != b.providerID {
		return a.providerID < b.providerID
	}
	return a.keyID < b.keyID
}

func dedupePrefixes(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, prefix := range sorted[1:] {
		if prefix != out[len(out)-1] {
			out = append(out, prefix)
		}
	}
	return out
}

func formatProblems(problems []ConfigProblem) string {
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.Field+": "+problem.Problem)
	}
	return strings.Join(parts, "; ")
}

// errorBox keeps atomic.Pointer type-safe for an error value.
type errorBox struct{ err error }

// StoreOptions configures a snapshot Store.
type StoreOptions struct {
	Keyring     *Keyring
	Refs        RefResolver
	IsProtected func(string) bool
	Now         func() time.Time
}

// Store loads and publishes snapshots. A failed reload keeps the last-known-good
// snapshot and records the error.
type Store struct {
	source      RowSource
	keyring     *Keyring
	refs        RefResolver
	isProtected func(string) bool
	now         func() time.Time

	current atomic.Pointer[Snapshot]
	lastErr atomic.Pointer[errorBox]
	reloadM sync.Mutex
}

// NewStore constructs a Store over a row source.
func NewStore(source RowSource, opts StoreOptions) *Store {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		source:      source,
		keyring:     opts.Keyring,
		refs:        opts.Refs,
		isProtected: opts.IsProtected,
		now:         now,
	}
}

// Current returns the published snapshot, or nil before the first success.
// The returned value is immutable.
func (s *Store) Current() *Snapshot { return s.current.Load() }

// Version returns the published snapshot version, or zero.
func (s *Store) Version() int64 {
	if snap := s.Current(); snap != nil {
		return snap.Version()
	}
	return 0
}

// LastError returns the most recent load error, if any.
func (s *Store) LastError() error {
	if box := s.lastErr.Load(); box != nil {
		return box.err
	}
	return nil
}

// Keyring exposes the configured keyring for request-time reference resolution.
func (s *Store) Keyring() *Keyring { return s.keyring }

// Refs exposes the configured reference resolver.
func (s *Store) Refs() RefResolver { return s.refs }

// Reload loads a snapshot under the source's repeatable-read transaction and
// publishes it only after full validation. If config_state.version advanced
// during the load, it reloads immediately to avoid publishing a stale version.
func (s *Store) Reload(ctx context.Context) error {
	s.reloadM.Lock()
	defer s.reloadM.Unlock()

	const maxReloads = 8
	for attempt := 0; attempt < maxReloads; attempt++ {
		rows, version, err := s.source.LoadConfig(ctx)
		if err != nil {
			s.recordError(err)
			return err
		}
		snapshot, err := BuildSnapshot(rows, version, BuildOptions{
			Keyring:     s.keyring,
			Refs:        s.refs,
			IsProtected: s.isProtected,
			Now:         s.now,
		})
		if err != nil {
			s.recordError(err)
			return err
		}
		// Do not publish a snapshot whose version already advanced: the next
		// iteration reloads, and the previous snapshot stays last-known-good.
		latest, err := s.source.Version(ctx)
		if err != nil {
			s.recordError(err)
			return err
		}
		if latest <= version {
			s.current.Store(snapshot)
			s.lastErr.Store(nil)
			return nil
		}
	}
	err := fmt.Errorf("%w: config version kept advancing during reload", ErrInvalidConfig)
	s.recordError(err)
	return err
}

func (s *Store) recordError(err error) {
	if err == nil {
		s.lastErr.Store(nil)
		return
	}
	s.lastErr.Store(&errorBox{err: err})
}

// configVersionChanged reports whether config_state.version differs from the
// published snapshot. It is used by the notification watcher's poll fallback.
func (s *Store) configVersionChanged(ctx context.Context) (bool, error) {
	version, err := s.source.Version(ctx)
	if err != nil {
		return false, err
	}
	return version != s.Version(), nil
}
