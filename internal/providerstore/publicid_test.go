package providerstore

import (
	"errors"
	"testing"
)

// TestPublicModelIDIsStable pins the derived id. Clients paste this value into
// their configuration, so a new algorithm would break every existing caller:
// the golden string is the compatibility contract, not an implementation detail.
func TestPublicModelIDIsStable(t *testing.T) {
	if got := PublicModelID("p1", "m/1"); got != "nbyemxqy6utyviyf47wvomo7ao" {
		t.Fatalf("PublicModelID(p1, m/1) = %q, want the pinned id", got)
	}
	if PublicModelID("p1", "m/1") == PublicModelID("p2", "m/1") {
		t.Fatal("the same alias on two providers must not share an id")
	}
}

// TestSharedModelIDResolvesSharedMapping covers the routing contract: the id
// addresses the shared mapping, never echoes the alias, and stops working once
// the account has a personal mapping for that alias.
func TestSharedModelIDResolvesSharedMapping(t *testing.T) {
	const alias = "m/1"
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			testKey("kp", "p1", "u1", SchemeNone),
			testKey("ks", "p1", "", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("kp", "p1", "u1", alias, "up-personal"),
			exactModel("ks", "p1", "", alias, "up-shared"),
		},
	}
	router := routerFor(t, rows, StoreOptions{})
	id := PublicModelID("p1", alias)

	route, err := router.Resolve(t.Context(), "u2", id, "")
	if err != nil {
		t.Fatalf("resolve shared id: %v", err)
	}
	if route.KeyID != "ks" || route.UpstreamModel != "up-shared" {
		t.Fatalf("route = key %q upstream %q, want the shared mapping", route.KeyID, route.UpstreamModel)
	}
	if route.Alias != id {
		t.Fatalf("route alias = %q, want the caller's id so the mapping stays unread", route.Alias)
	}

	// u1 owns a personal mapping for the alias, so its own key outranks the
	// shared one and the id stops resolving, exactly as the console stops
	// advertising it.
	if _, err := router.Resolve(t.Context(), "u1", id, ""); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("resolve shared id as the owner of a personal mapping = %v, want ErrModelNotFound", err)
	}
	if route, err := router.Resolve(t.Context(), "u1", alias, ""); err != nil || route.KeyID != "kp" {
		t.Fatalf("personal alias = %v key %q, want the personal mapping", err, route.KeyID)
	}
}

// TestPublicModelIDOnlyAddressesSharedMappings keeps the id space honest: a
// caller cannot derive the id of someone else's personal mapping and route
// through it.
func TestPublicModelIDOnlyAddressesSharedMappings(t *testing.T) {
	const alias = "m/1"
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("kp", "p1", "u1", SchemeNone)},
		KeyModels: []KeyModelRow{exactModel("kp", "p1", "u1", alias, "up-personal")},
	}
	router := routerFor(t, rows, StoreOptions{})

	if _, err := router.Resolve(t.Context(), "u2", PublicModelID("p1", alias), ""); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("resolve a personal mapping's id = %v, want ErrModelNotFound", err)
	}
	if _, err := router.Resolve(t.Context(), "u2", alias, ""); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("resolve a personal alias without a key = %v, want ErrKeyRequired", err)
	}
}

// TestHiddenSharedMappingHidesItsID keeps an id that a mapping no longer
// advertises from resolving, and keeps a newly hidden one a 404.
func TestHiddenSharedMappingHidesItsID(t *testing.T) {
	const alias = "m/1"
	hidden := exactModel("ks", "p1", "", alias, "up-shared")
	hidden.Hidden = true
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("ks", "p1", "", SchemeNone)},
		KeyModels: []KeyModelRow{hidden},
	}
	router := routerFor(t, rows, StoreOptions{})

	if _, err := router.Resolve(t.Context(), "u1", PublicModelID("p1", alias), ""); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("resolve a hidden shared mapping's id = %v, want ErrModelHidden", err)
	}
}

// TestAvailableModelsReportsPublicID pins the console contract: the picker gets
// the id for a shared mapping and the alias for the caller's own.
func TestAvailableModelsReportsPublicID(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			testKey("kp", "p1", "u1", SchemeNone),
			testKey("ks", "p1", "", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("kp", "p1", "u1", "own/1", "up-own"),
			exactModel("ks", "p1", "", "shared/1", "up-shared"),
		},
	}
	snapshot := mustBuildSnapshot(t, rows, 1, BuildOptions{Now: fixedNow})

	got := map[string]string{}
	for _, model := range snapshot.AvailableModels("u1", false) {
		got[model.Alias] = model.PublicID
	}
	if id := got["own/1"]; id != "" {
		t.Fatalf("personal mapping public id = %q, want empty so the alias stays the id", id)
	}
	if id := got["shared/1"]; id != PublicModelID("p1", "shared/1") {
		t.Fatalf("shared mapping public id = %q, want the derived id", id)
	}
}
