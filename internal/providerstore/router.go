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
}

// NewRouter constructs a key-aware router over a snapshot store.
func NewRouter(store *Store) *Router { return &Router{store: store} }

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
	chain := []selected{primary}
	visited := map[string]bool{clientModel: true}
	appendFallbacks(snapshot, userID, primary.candidate.fallbacks, &chain, visited)

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
		if credentialErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrCredentialUnavailable, credentialErr)
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
		selection, err := selectExactOnly(snapshot, userID, alias, true)
		if err != nil {
			continue
		}
		*chain = append(*chain, selection)
		appendFallbacks(snapshot, userID, selection.candidate.fallbacks, chain, visited)
	}
}

// selectPrimary implements the normative precedence: personal exact, personal
// wildcard (longest prefix), shared none exact, shared none wildcard. exactOnly
// is used for fallback aliases, which never match wildcards.
func selectPrimary(snapshot *Snapshot, userID, model string) (selected, error) {
	return selectExactOnly(snapshot, userID, model, false)
}

func selectExactOnly(snapshot *Snapshot, userID, model string, exactOnly bool) (selected, error) {
	if state, ok := snapshot.modelState[model]; ok && (state.Hidden || state.Deleted) {
		return selected{}, ErrModelHidden
	}
	if list := snapshot.personalEx[userID][model]; len(list) > 0 {
		if selection, ok, err := pickExact(list, model, snapshot.isProtected); ok || err != nil {
			return selection, err
		}
	}
	if !exactOnly {
		if selection, ok, err := pickWildcard(snapshot.personalWc[userID], model, snapshot.isProtected); ok || err != nil {
			return selection, err
		}
	}
	if list := snapshot.sharedEx[model]; len(list) > 0 {
		if selection, ok, err := pickExact(list, model, snapshot.isProtected); ok || err != nil {
			return selection, err
		}
	}
	if !exactOnly {
		if selection, ok, err := pickWildcard(snapshot.sharedWc, model, snapshot.isProtected); ok || err != nil {
			return selection, err
		}
	}
	if snapshot.aliasKnown(model) {
		return selected{}, ErrKeyRequired
	}
	return selected{}, ErrModelNotFound
}

func pickExact(list []*candidate, model string, isProtected func(string) bool) (selected, bool, error) {
	for _, c := range list {
		if isProtected != nil && isProtected(model) && !c.allowShadow {
			continue
		}
		if c.hidden {
			return selected{}, false, ErrModelHidden
		}
		return selected{candidate: c, upstream: c.upstreamModel, alias: model}, true, nil
	}
	return selected{}, false, nil
}

func pickWildcard(list []*candidate, model string, isProtected func(string) bool) (selected, bool, error) {
	for _, c := range list {
		prefix := strings.TrimSuffix(c.alias, "*")
		if !strings.HasPrefix(model, prefix) || len(model) <= len(prefix) {
			continue
		}
		if isProtected != nil && isProtected(model) && !c.allowShadow {
			continue
		}
		if c.hidden {
			return selected{}, false, ErrModelHidden
		}
		suffix := model[len(prefix):]
		upstream := suffix
		if c.upstreamModel != "" && c.upstreamModel != "*" {
			if strings.HasSuffix(c.upstreamModel, "/*") {
				upstream = strings.TrimSuffix(c.upstreamModel, "*") + suffix
			} else {
				upstream = c.upstreamModel
			}
		}
		return selected{candidate: c, upstream: upstream, alias: model}, true, nil
	}
	return selected{}, false, nil
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
