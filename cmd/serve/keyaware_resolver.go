package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/core"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// keyAwareResolver routes a gateway-token authenticated user through the
// database snapshot and delegates anonymous requests, file mode and virtual
// models to the existing resolver unchanged.
type keyAwareResolver struct {
	runtime *providerRuntime
	legacy  routing.Resolver
}

func (r *keyAwareResolver) Resolve(ctx context.Context, model string, protocol ir.Protocol) (routing.Route, error) {
	userID := keyAwareUserID(ctx)
	if userID == "" || r.runtime == nil || r.runtime.router == nil ||
		model == routing.VirtualAutoMode || model == routing.VirtualCycle {
		return r.legacy.Resolve(ctx, model, protocol)
	}
	route, err := r.runtime.router.Resolve(ctx, userID, model, protocol)
	if err != nil {
		return routing.Route{}, keyAwareRouteError(err)
	}
	return keyAwareRoutingRoute(&route), nil
}

func (r *keyAwareResolver) ResolveCandidates(ctx context.Context, model string, protocol ir.Protocol) ([]routing.Route, error) {
	userID := keyAwareUserID(ctx)
	if userID == "" || r.runtime == nil || r.runtime.router == nil ||
		model == routing.VirtualAutoMode || model == routing.VirtualCycle {
		return r.legacy.ResolveCandidates(ctx, model, protocol)
	}
	routes, err := r.runtime.router.ResolveCandidates(ctx, userID, model, protocol)
	if err != nil {
		return nil, keyAwareRouteError(err)
	}
	out := make([]routing.Route, 0, len(routes))
	for i := range routes {
		out = append(out, keyAwareRoutingRoute(&routes[i]))
	}
	return out, nil
}

func (r *keyAwareResolver) AdvanceCycle(failed string) {
	if r.legacy != nil {
		r.legacy.AdvanceCycle(failed)
	}
}

func keyAwareUserID(ctx context.Context) string {
	if rc := core.RequestContextFrom(ctx); rc != nil {
		return rc.UserID
	}
	return ""
}

func keyAwareRoutingRoute(route *providerstore.Route) routing.Route {
	out := routing.Route{
		Provider:         route.Provider,
		UpstreamModel:    route.UpstreamModel,
		UpstreamProtocol: route.UpstreamProtocol,
		Kind:             routing.Custom,
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
