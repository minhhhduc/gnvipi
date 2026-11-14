package providerstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// Route is one key-aware routing decision. Provider is a defensive copy with
// the selected key's auth materialized.
type Route struct {
	Provider         *provider.Provider
	UpstreamModel    string
	UpstreamProtocol ir.Protocol
	Alias            string
	KeyID            string
	ProviderID       string
	OwnerID          string
	FallbackStatuses []int
	Candidates       []Route
	Fallbacks        []Route
}

// Router resolves aliases against a Store's published snapshot. It never
// routes across users: personal candidates are selected only for their owner,
// and shared candidates only exist for auth_scheme none.
type Router struct {
	store *Store
	audit AuditFunc
}

// RouterOptions configures routing. Audit overrides the Store callback.
type RouterOptions struct {
	Audit AuditFunc
}

// NewRouter constructs a key-aware router over a snapshot store.
func NewRouter(store *Store) *Router {
	router := &Router{store: store}
	if store != nil {
		router.audit = store.audit
	}
	return router
}

// NewRouterWithOptions constructs a router with an explicit audit callback.
func NewRouterWithOptions(store *Store, opts RouterOptions) *Router {
	router := NewRouter(store)
	if opts.Audit != nil {
		router.audit = opts.Audit
	}
	return router
}

func (r *Router) auditUnavailable(userID, model string, chain []selected) {
	if r.audit == nil {
		return
	}
	event := AuditEvent{Code: AuditCredentialUnavailable, UserID: userID, Model: model}
	for _, selection := range chain {
		event.ProviderIDs = append(event.ProviderIDs, selection.candidate.providerID)
		event.KeyIDs = append(event.KeyIDs, selection.candidate.keyID)
	}
	r.audit(event)
}

type selected struct {
	candidate *candidate
	upstream  string
	alias     string
}

// Resolve returns the first usable candidate plus its fallback chain.
func (r *Router) Resolve(ctx context.Context, userID, clientModel string, protocol ir.Protocol) (Route, error) {
	routes, err := r.ResolveCandidates(ctx, userID, clientModel, protocol)
	if err != nil {
		return Route{}, err
	}
	primary := routes[0]
	primary.Candidates = routes
	primary.Fallbacks = routes[1:]
	return primary, nil
}

// ResolveCandidates returns the ordered, credential-filtered candidate list.
// Credential-unresolvable candidates are skipped; when none remain the result
// is ErrCredentialUnavailable (503). Unknown aliases are ErrModelNotFound
// (404) and aliases known but unusable by this user are ErrKeyRequired (403).
func (r *Router) ResolveCandidates(ctx context.Context, userID, clientModel string, protocol ir.Protocol) ([]Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(clientModel) == "" {
		return nil, fmt.Errorf("%w: model must not be empty", ErrModelNotFound)
	}
	snapshot := r.store.Current()
	if snapshot == nil {
		return nil, ErrNoSnapshot
	}
	primary, err := selectPrimary(snapshot, userID, clientModel)
	if err != nil {
		return nil, err
	}
	chain := append([]selected(nil), primary...)
	visited := map[string]bool{clientModel: true}
	for _, selection := range primary {
		appendFallbacks(snapshot, userID, selection.candidate.fallbacks, &chain, visited)
	}

	var routes []Route
	var credentialErr error
	for _, selection := range chain {
		route, err := r.materialize(ctx, selection)
		if err != nil {
			if isCredentialError(err) {
				credentialErr = err
			}
			continue
		}
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		r.auditUnavailable(userID, clientModel, chain)
		if credentialErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrCredentialUnavailable, credentialErr)
		}
		return nil, ErrCredentialUnavailable
	}
	return routes, nil
}

func appendFallbacks(snapshot *Snapshot, userID string, aliases []string, chain *[]selected, visited map[string]bool) {
	const maxCandidates = 8
	for _, raw := range aliases {
		if len(*chain) >= maxCandidates {
			return
		}
		alias := strings.TrimSpace(raw)
		if alias == "" || visited[alias] {
			continue
		}
		visited[alias] = true
		selections, err := selectExactOnly(snapshot, userID, alias, true)
		if err != nil {
			continue
		}
		for _, selection := range selections {
			if len(*chain) >= maxCandidates {
				return
			}
			*chain = append(*chain, selection)
			appendFallbacks(snapshot, userID, selection.candidate.fallbacks, chain, visited)
		}
	}
}

// selectPrimary implements the normative precedence: personal exact, personal
// wildcard (longest prefix), shared none exact, shared none wildcard. exactOnly
// is used for fallback aliases, which never match wildcards. A tier can yield
// several candidates; all of them are tried before the next tier or fallback.
func selectPrimary(snapshot *Snapshot, userID, model string) ([]selected, error) {
	return selectExactOnly(snapshot, userID, model, false)
}

func selectExactOnly(snapshot *Snapshot, userID, model string, exactOnly bool) ([]selected, error) {
	if state, ok := snapshot.modelState[model]; ok && (state.Hidden || state.Deleted) {
		return nil, ErrModelHidden
	}
	if list := snapshot.personalEx[userID][model]; len(list) > 0 {
		if selections, err := pickExact(list, model, snapshot.isProtected); err != nil {
			return nil, err
		} else if len(selections) > 0 {
			return selections, nil
		}
	}
	if !exactOnly {
		if selections, err := pickWildcard(snapshot.personalWc[userID], model, snapshot.isProtected); err != nil {
			return nil, err
		} else if len(selections) > 0 {
			return selections, nil
		}
	}
	if list := snapshot.sharedEx[model]; len(list) > 0 {
		if selections, err := pickExact(list, model, snapshot.isProtected); err != nil {
			return nil, err
		} else if len(selections) > 0 {
			return selections, nil
		}
	}
	if !exactOnly {
		if selections, err := pickWildcard(snapshot.sharedWc, model, snapshot.isProtected); err != nil {
			return nil, err
		} else if len(selections) > 0 {
			return selections, nil
		}
	}
	if snapshot.aliasKnown(model) {
		if providerID := snapshot.knownProvider(model); providerID != "" {
			return nil, fmt.Errorf("%w: provider %q requires a key for alias %q; add an enabled provider key",
				ErrKeyRequired, providerID, model)
		}
		return nil, ErrKeyRequired
	}
	return nil, ErrModelNotFound
}

func pickExact(list []*candidate, model string, isProtected func(string) bool) ([]selected, error) {
	var out []selected
	hidden := false
	for _, c := range list {
		if isProtected != nil && isProtected(model) && !c.allowShadow {
			continue
		}
		if c.hidden {
			hidden = true
			continue
		}
		out = append(out, selected{candidate: c, upstream: c.upstreamModel, alias: model})
	}
	if len(out) > 0 {
		return out, nil
	}
	if hidden {
		return nil, ErrModelHidden
	}
	return nil, nil
}

func pickWildcard(list []*candidate, model string, isProtected func(string) bool) ([]selected, error) {
	var out []selected
	bestPrefix := -1
	hidden := false
	for _, c := range list {
		prefix := strings.TrimSuffix(c.alias, "*")
		if !strings.HasPrefix(model, prefix) || len(model) <= len(prefix) {
			continue
		}
		if isProtected != nil && isProtected(model) && !c.allowShadow {
			continue
		}
		if c.hidden {
			hidden = true
			continue
		}
		if bestPrefix < 0 {
			bestPrefix = len(prefix)
		}
		if len(prefix) != bestPrefix {
			continue
		}
		out = append(out, selected{
			candidate: c,
			upstream:  wildcardUpstream(c, model[len(prefix):]),
			alias:     model,
		})
	}
	if len(out) > 0 {
		return out, nil
	}
	if hidden {
		return nil, ErrModelHidden
	}
	return nil, nil
}

func wildcardUpstream(c *candidate, suffix string) string {
	if c.upstreamModel == "" || c.upstreamModel == "*" {
		return suffix
	}
	if strings.HasSuffix(c.upstreamModel, "/*") {
		return strings.TrimSuffix(c.upstreamModel, "*") + suffix
	}
	return c.upstreamModel
}

func (s *Snapshot) aliasKnown(model string) bool {
	if s.knownEx[model] {
		return true
	}
	for _, prefix := range s.knownWc {
		if strings.HasPrefix(model, prefix) && len(model) > len(prefix) {
			return true
		}
	}
	return false
}

// knownProvider names the provider behind a known alias so the missing-key
// error can tell the user what to configure. It follows resolution tier order
// and returns only an identifier, never credential material.
func (s *Snapshot) knownProvider(model string) string {
	var personalExact *candidate
	for _, byAlias := range s.personalEx {
		if c := minCandidate(byAlias[model]); c != nil && (personalExact == nil || candidateLess(c, personalExact)) {
			personalExact = c
		}
	}
	if personalExact != nil {
		return personalExact.providerID
	}
	var lists [][]*candidate
	for _, list := range s.personalWc {
		lists = append(lists, list)
	}
	if c := longestWildcard(lists, model); c != nil {
		return c.providerID
	}
	if c := minCandidate(s.sharedEx[model]); c != nil {
		return c.providerID
	}
	if c := longestWildcard([][]*candidate{s.sharedWc}, model); c != nil {
		return c.providerID
	}
	return ""
}

func minCandidate(list []*candidate) *candidate {
	var best *candidate
	for _, c := range list {
		if best == nil || candidateLess(c, best) {
			best = c
		}
	}
	return best
}

// longestWildcard returns the candidate with the longest matching prefix,
// breaking ties with the normal candidate order.
func longestWildcard(lists [][]*candidate, model string) *candidate {
	bestLen := -1
	var best *candidate
	for _, list := range lists {
		for _, c := range list {
			prefix := strings.TrimSuffix(c.alias, "*")
			if !strings.HasPrefix(model, prefix) || len(model) <= len(prefix) {
				continue
			}
			if len(prefix) > bestLen || (len(prefix) == bestLen && (best == nil || candidateLess(c, best))) {
				bestLen = len(prefix)
				best = c
			}
		}
	}
	return best
}

func (r *Router) materialize(ctx context.Context, selection selected) (Route, error) {
	c := selection.candidate
	var credential ResolvedCredential
	switch c.key.AuthScheme {
	case SchemeNone:
	case SchemeBearer, SchemeHeader, SchemeBasic:
		if c.key.HasSecret() {
			if c.credential != nil {
				return Route{}, c.credential
			}
			if !c.hasResolved {
				return Route{}, ErrCredentialUnavailable
			}
			credential = ResolvedCredential{Raw: c.secretRaw, Basic: c.basic}
		} else {
			resolved, err := ResolveKeyCredential(ctx, c.key, r.store.Keyring(), r.store.Refs())
			if err != nil {
				return Route{}, err
			}
			credential = resolved
		}
	default:
		return Route{}, fmt.Errorf("%w: key %s has unsupported scheme", ErrInvalidCredentialSource, c.key.ID)
	}

	definition := provider.Clone(c.definition)
	switch c.key.AuthScheme {
	case SchemeNone:
		definition.Auth = provider.Auth{Scheme: provider.AuthNone}
	case SchemeBearer:
		definition.Auth = provider.Auth{Scheme: provider.AuthBearer, Key: credential.Raw}
	case SchemeHeader:
		definition.Auth = provider.Auth{Scheme: provider.AuthAPIKey, Key: credential.Raw}
		if !strings.EqualFold(c.key.AuthHeaderName, "x-api-key") {
			definition.Auth.HeaderName = c.key.AuthHeaderName
		}
	case SchemeBasic:
		token := credential.Basic.Username + ":" + credential.Basic.Password
		definition.Auth = provider.Auth{
			Scheme:     provider.AuthAPIKey,
			Key:        "Basic " + base64.StdEncoding.EncodeToString([]byte(token)),
			HeaderName: "Authorization",
		}
	}
	return Route{
		Provider:         &definition,
		UpstreamModel:    selection.upstream,
		UpstreamProtocol: definition.Protocol,
		Alias:            selection.alias,
		KeyID:            c.key.ID,
		ProviderID:       c.providerID,
		OwnerID:          c.ownerID,
		FallbackStatuses: append([]int(nil), c.fallbackStatuses...),
	}, nil
}

func isCredentialError(err error) bool {
	for _, sentinel := range []error{
		ErrCredentialUnavailable,
		ErrUnsupportedReference,
		ErrMissingMasterKey,
		ErrDecrypt,
		ErrUnknownKeyVersion,
		ErrInvalidCredentialSource,
		ErrInvalidNonce,
		ErrInvalidCiphertext,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// FallbackTriggered applies fallback_statuses semantics. Empty means the
// default 408, 429 and all 5xx set.
func FallbackTriggered(status int, configured []int) bool {
	if len(configured) > 0 {
		for _, candidate := range configured {
			if candidate == status {
				return true
			}
		}
		return false
	}
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		(status >= 500 && status <= 599)
}

// StatusForError maps routing errors to the HTTP statuses in the design.
func StatusForError(err error) int {
	switch {
	case errors.Is(err, ErrModelNotFound), errors.Is(err, ErrModelHidden):
		return http.StatusNotFound
	case errors.Is(err, ErrKeyRequired):
		return http.StatusForbidden
	case errors.Is(err, ErrCredentialUnavailable),
		errors.Is(err, ErrUnsupportedReference),
		errors.Is(err, ErrNoSnapshot),
		errors.Is(err, ErrMissingMasterKey),
		errors.Is(err, ErrDecrypt),
		errors.Is(err, ErrUnknownKeyVersion):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
