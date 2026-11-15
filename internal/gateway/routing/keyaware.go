package routing

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// KeyedRouteSource resolves database-backed candidates for one authenticated
// user. *providerstore.Router satisfies it.
type KeyedRouteSource interface {
	Resolve(ctx context.Context, userID, clientModel string, protocol ir.Protocol) (providerstore.Route, error)
	ResolveCandidates(ctx context.Context, userID, clientModel string, protocol ir.Protocol) ([]providerstore.Route, error)
}

// KeyAwareResolver routes an authenticated database user through their keyed
// candidates and delegates everyone else to the registry resolver.
type KeyAwareResolver struct {
	keyed  KeyedRouteSource
	legacy Resolver
}

func NewKeyAwareResolver(keyed KeyedRouteSource, legacy Resolver) *KeyAwareResolver {
	return &KeyAwareResolver{keyed: keyed, legacy: legacy}
}

func (r *KeyAwareResolver) Resolve(ctx context.Context, model string, protocol ir.Protocol) (Route, error) {
	userID := keyAwareUserID(ctx)
	if !r.usesKeyed(userID, model) {
		return r.legacy.Resolve(ctx, model, protocol)
	}
	route, err := r.keyed.Resolve(ctx, userID, model, protocol)
	if err != nil {
		if delegated, ok := r.delegate(ctx, model, protocol, err); ok {
			return delegated[0], nil
		}
		return Route{}, keyAwareRouteError(err)
	}
	return keyAwareRoutingRoute(&route), nil
}

func (r *KeyAwareResolver) ResolveCandidates(ctx context.Context, model string, protocol ir.Protocol) ([]Route, error) {
	userID := keyAwareUserID(ctx)
	if !r.usesKeyed(userID, model) {
		return r.legacy.ResolveCandidates(ctx, model, protocol)
	}
	routes, err := r.keyed.ResolveCandidates(ctx, userID, model, protocol)
	if err != nil {
		if delegated, ok := r.delegate(ctx, model, protocol, err); ok {
			return delegated, nil
		}
		return nil, keyAwareRouteError(err)
	}
	out := make([]Route, 0, len(routes))
	for i := range routes {
		out = append(out, keyAwareRoutingRoute(&routes[i]))
	}
	return out, nil
}

func (r *KeyAwareResolver) AdvanceCycle(failed string) {
	if r.legacy != nil {
		r.legacy.AdvanceCycle(failed)
	}
}

func (r *KeyAwareResolver) usesKeyed(userID, model string) bool {
	return userID != "" && r.keyed != nil && r.legacy != nil &&
		model != VirtualAutoMode && model != VirtualCycle
}

// delegate keeps gateway-wide routes reachable for database users: protected
// NVIDIA models (the shared captcha pool has no per-user credential) and
// registry-known models the database snapshot does not know. A hidden or
// key-required database alias is authoritative and never delegates.
func (r *KeyAwareResolver) delegate(ctx context.Context, model string, protocol ir.Protocol, err error) ([]Route, bool) {
	if !errors.Is(err, providerstore.ErrModelNotFound) {
		return nil, false
	}
	routes, legacyErr := r.legacy.ResolveCandidates(ctx, model, protocol)
	if legacyErr != nil || len(routes) == 0 {
		return nil, false
	}
	// The registry falls through for a protected model, which is exactly the
	// shared captcha-pool path; anything else must be a configured registry route.
	if routes[0].Kind != Custom && !defaultIsProtected(model) {
		return nil, false
	}
	return routes, true
}

func keyAwareUserID(ctx context.Context) string {
	if rc := core.RequestContextFrom(ctx); rc != nil {
		return rc.UserID
	}
	return ""
}

func keyAwareRoutingRoute(route *providerstore.Route) Route {
	out := Route{
		Provider:         route.Provider,
		UpstreamModel:    route.UpstreamModel,
		UpstreamProtocol: route.UpstreamProtocol,
		Kind:             Custom,
		Alias:            route.Alias,
		KeyID:            route.KeyID,
	}
	if len(route.FallbackStatuses) > 0 {
		out.FallbackStatuses = append([]int(nil), route.FallbackStatuses...)
	}
	for i := range route.Fallbacks {
		out.Fallbacks = append(out.Fallbacks, keyAwareRoutingRoute(&route.Fallbacks[i]))
	}
	return out
}

// keyAwareRouteError maps providerstore sentinels onto the gateway error model.
// Messages carry provider and alias identifiers only, never key material.
func keyAwareRouteError(err error) error {
	message := strings.TrimPrefix(err.Error(), "providerstore: ")
	switch {
	case errors.Is(err, providerstore.ErrModelNotAvailable):
		return &gwerr.Error{
			Kind:    gwerr.KindNotFound,
			Status:  http.StatusNotFound,
			Code:    "model_not_available",
			Message: message,
		}
	case errors.Is(err, providerstore.ErrModelNotFound), errors.Is(err, providerstore.ErrModelHidden):
		return &gwerr.Error{Kind: gwerr.KindNotFound, Status: http.StatusNotFound, Message: message}
	case errors.Is(err, providerstore.ErrKeyRequired):
		return &gwerr.Error{Kind: gwerr.KindPermission, Status: http.StatusForbidden, Message: message}
	case errors.Is(err, providerstore.ErrCredentialUnavailable),
		errors.Is(err, providerstore.ErrUnsupportedReference),
		errors.Is(err, providerstore.ErrNoSnapshot),
		errors.Is(err, providerstore.ErrMissingMasterKey),
		errors.Is(err, providerstore.ErrDecrypt),
		errors.Is(err, providerstore.ErrUnknownKeyVersion):
		return &gwerr.Error{Kind: gwerr.KindOverloaded, Status: http.StatusServiceUnavailable, Message: message}
	default:
		return err
	}
}
