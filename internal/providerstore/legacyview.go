package providerstore

import (
	"fmt"
	"sort"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// LegacyViewOptions controls which key metadata is visible to the caller.
type LegacyViewOptions struct {
	// VisibleKeyIDs filters keys and their models. Nil means every key.
	VisibleKeyIDs []string
	// IncludeKeyEnv and IncludeKeyRef expose names/references, never values.
	IncludeKeyEnv bool
	IncludeKeyRef bool
}

// LegacyView projects DB rows onto the existing v2 provider shape.
//
// Mapping rules:
//   - DB none -> v2 none; bearer -> bearer.
//   - DB header -> v2 x-api-key plus auth.header_name (omitted for x-api-key).
//   - DB basic -> v2 basic (additive; old clients must not edit it).
//   - auth.key is never returned. key_env/key_ref only per visibility options.
//
// A v2 provider has one auth object. When several keys are visible the first
// deterministic key (shared first, then key id) supplies the auth metadata;
// clients writable at provider level must use LegacyModelsTarget.
func LegacyView(row ProviderRow, keys []KeyRow, models []KeyModelRow, opts LegacyViewOptions) (provider.Provider, error) {
	definition, err := BuildProviderDefinition(row)
	if err != nil {
		return provider.Provider{}, err
	}
	visible := visibleKeys(keys, opts.VisibleKeyIDs)
	sorted := SortedKeys(visible)
	visibleByID := make(map[string]bool, len(sorted))
	for _, key := range sorted {
		visibleByID[key.ID] = true
	}
	if len(sorted) > 0 {
		definition.Auth = legacyAuth(sorted[0], opts)
	} else {
		definition.Auth = provider.Auth{Scheme: provider.AuthNone}
	}
	definition.Models = legacyModels(models, visibleByID)
	return definition, nil
}

func visibleKeys(keys []KeyRow, allowed []string) []KeyRow {
	if allowed == nil {
		return append([]KeyRow(nil), keys...)
	}
	allow := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		allow[id] = true
	}
	out := make([]KeyRow, 0, len(keys))
	for _, key := range keys {
		if allow[key.ID] {
			out = append(out, key)
		}
	}
	return out
}

func legacyAuth(key KeyRow, opts LegacyViewOptions) provider.Auth {
	var auth provider.Auth
	switch key.AuthScheme {
	case SchemeNone:
		auth = provider.Auth{Scheme: provider.AuthNone}
	case SchemeBearer:
		auth = provider.Auth{Scheme: provider.AuthBearer}
	case SchemeHeader:
		auth = provider.Auth{Scheme: provider.AuthAPIKey}
		if !strings.EqualFold(key.AuthHeaderName, "x-api-key") {
			auth.HeaderName = key.AuthHeaderName
		}
	case SchemeBasic:
		auth = provider.Auth{Scheme: provider.AuthScheme(SchemeBasic)}
	default:
		auth = provider.Auth{Scheme: provider.AuthNone}
	}
	if opts.IncludeKeyEnv {
		auth.KeyEnv = key.KeyEnv
	}
	if opts.IncludeKeyRef {
		auth.KeyRef = key.KeyRef
	}
	return auth
}

func legacyModels(rows []KeyModelRow, visibleKeys map[string]bool) []provider.ModelMapping {
	sorted := append([]KeyModelRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Alias != sorted[j].Alias {
			return sorted[i].Alias < sorted[j].Alias
		}
		return sorted[i].KeyID < sorted[j].KeyID
	})
	seen := make(map[string]bool, len(sorted))
	out := make([]provider.ModelMapping, 0, len(sorted))
	for _, row := range sorted {
		if !visibleKeys[row.KeyID] || seen[row.Alias] {
			continue
		}
		seen[row.Alias] = true
		fallbacks, _ := decodeFallbacks(row.Fallbacks)              //nolint:errcheck // best-effort cleanup/best-effort parse
		statuses, _ := decodeFallbackStatuses(row.FallbackStatuses) //nolint:errcheck // best-effort cleanup/best-effort parse
		out = append(out, provider.ModelMapping{
			Alias:            row.Alias,
			UpstreamModel:    row.UpstreamModel,
			Hidden:           row.Hidden,
			Fallbacks:        fallbacks,
			FallbackStatuses: statuses,
			LegacyPosition:   row.LegacyPosition,
			AllowShadow:      row.AllowShadow,
		})
	}
	return out
}

// LegacyModelsTarget returns the single visible key that may accept a legacy
// provider-level models write. More than one visible key is ErrModelsAmbiguous
// (HTTP 409); the client must use the per-key models endpoint.
func LegacyModelsTarget(keys []KeyRow, opts LegacyViewOptions) (string, error) {
	visible := visibleKeys(keys, opts.VisibleKeyIDs)
	if len(visible) != 1 {
		return "", fmt.Errorf("%w: %d visible keys", ErrModelsAmbiguous, len(visible))
	}
	return visible[0].ID, nil
}
