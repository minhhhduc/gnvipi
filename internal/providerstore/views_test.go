package providerstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func viewRows() Rows {
	return Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			testKey("k-u1", "p1", "u1", SchemeNone),
			testKey("k-u2", "p1", "u2", SchemeNone),
			testKey("k-shared", "p1", "", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("k-u1", "p1", "u1", "alpha", "up-alpha"),
			exactModel("k-u2", "p1", "u2", "beta", "up-beta"),
			exactModel("k-shared", "p1", "", "shared", "up-shared"),
			wildcardModel("k-shared", "p1", "", "fam/*", "*"),
		},
	}
}

func TestAccountViewPerUserIsolation(t *testing.T) {
	router := routerFor(t, viewRows(), StoreOptions{})
	snapshot := router.store.Current()

	u1 := router.accountView(snapshot, "u1")
	u2 := router.accountView(snapshot, "u2")
	if u1.version != snapshot.Version() || u2.version != snapshot.Version() {
		t.Fatalf("view versions = %d, %d want %d", u1.version, u2.version, snapshot.Version())
	}
	if model, key, ok := u1.lookup("alpha", nil); !ok || key != "alpha" || model.ProviderID != "p1" || model.UpstreamModel != "up-alpha" {
		t.Fatalf("u1 alpha = %+v, %q, %v", model, key, ok)
	}
	if u1.contains("beta") {
		t.Fatal("u1 view leaked u2's personal alias")
	}
	if !u2.contains("beta") || u2.contains("alpha") {
		t.Fatal("u2 view is not isolated to its own keys")
	}
	for _, view := range []*accountView{u1, u2} {
		if !view.contains("shared") || !view.contains("fam/*") || !view.contains("fam/child") {
			t.Fatal("shared mappings missing from account view")
		}
	}
}

func TestAccountViewPersonalWildcardPrecedesSharedExact(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true), validProviderRow("p2", true)},
		Keys: []KeyRow{
			testKey("k-personal", "p1", "u1", SchemeNone),
			testKey("k-shared", "p2", "", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			wildcardModel("k-personal", "p1", "u1", "fam/*", "*"),
			exactModel("k-shared", "p2", "", "fam/x", "up-shared"),
		},
	}
	router := routerFor(t, rows, StoreOptions{})
	entry, key, ok := router.accountView(router.store.Current(), "u1").lookup("fam/x", nil)
	if !ok || key != "fam/*" || entry.ProviderID != "p1" || entry.UpstreamModel != "x" {
		t.Fatalf("lookup = %+v, %q, %v; personal wildcard must win", entry, key, ok)
	}
	route, err := router.Resolve(context.Background(), "u1", "fam/x", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.ProviderID != "p1" || route.UpstreamModel != "x" {
		t.Fatalf("route = %+v, want personal wildcard", route)
	}
}

func TestAccountViewFiltersHiddenModelState(t *testing.T) {
	rows := viewRows()
	rows.ModelState = []ModelStateRow{{ModelID: "alpha", Hidden: true}}
	router := routerFor(t, rows, StoreOptions{})
	view := router.accountView(router.store.Current(), "u1")
	if view.contains("alpha") {
		t.Fatal("hidden model_state alias stayed visible")
	}
	if _, err := router.Resolve(context.Background(), "u1", "alpha", "openai-chat"); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("resolve error = %v, want ErrModelHidden", err)
	}
}

func TestAccountViewEvictsServingWildcardPattern(t *testing.T) {
	router := routerFor(t, viewRows(), StoreOptions{})
	view := router.accountView(router.store.Current(), "u1")
	view.personalWildcard = append(view.personalWildcard, VisibleModel{
		Alias: "legacy/*", ProviderID: "p1", UpstreamModel: "*", allowShadow: true,
	})

	_, err := router.Resolve(context.Background(), "u1", "legacy/thing", "openai-chat")
	if !errors.Is(err, ErrModelNotAvailable) {
		t.Fatalf("error = %v, want ErrModelNotAvailable", err)
	}
	if view.hasPattern("legacy/*") {
		t.Fatal("eviction dropped the literal alias instead of the serving pattern")
	}
}

func TestAccountViewTyped404OnlyForPreviouslyMappedAliases(t *testing.T) {
	router := routerFor(t, viewRows(), StoreOptions{})
	_, err := router.Resolve(context.Background(), "u1", "typo-alias", "openai-chat")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("error = %v, want ErrModelNotFound", err)
	}
	if errors.Is(err, ErrModelNotAvailable) {
		t.Fatalf("never-mapped alias answered typed 404: %v", err)
	}
	if err.Error() != ErrModelNotFound.Error() {
		t.Fatalf("never-mapped alias message = %q", err)
	}
}

func TestAccountViewVersionInvalidationAndTyped404(t *testing.T) {
	router := routerFor(t, viewRows(), StoreOptions{})
	if _, err := router.Resolve(context.Background(), "u1", "shared", "openai-chat"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if !router.accountView(router.store.Current(), "u1").contains("shared") {
		t.Fatal("version 1 view is missing the alias")
	}

	// A shared-key write bumps config_state.version and removes the mapping.
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      viewRows().Keys,
		KeyModels: []KeyModelRow{
			exactModel("k-u1", "p1", "u1", "alpha", "up-alpha"),
			exactModel("k-u2", "p1", "u2", "beta", "up-beta"),
		},
	}
	next := mustBuildSnapshot(t, rows, 2, BuildOptions{Now: fixedNow})
	router.store.current.Store(next)

	_, err := router.Resolve(context.Background(), "u1", "shared", "openai-chat")
	if !errors.Is(err, ErrModelNotAvailable) || !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("stale alias error = %v, want ErrModelNotAvailable matching ErrModelNotFound", err)
	}
	if !strings.Contains(err.Error(), "no longer available for this account") {
		t.Fatalf("error %q does not explain the model is gone for this account", err)
	}
	if StatusForError(err) != 404 {
		t.Fatalf("status = %d, want 404", StatusForError(err))
	}
	view := router.accountView(next, "u1")
	if view.contains("shared") {
		t.Fatal("new version view still contains the removed alias")
	}
	if _, ok := view.retiredKey("shared"); ok {
		t.Fatal("typed 404 did not lazily drop the retired alias")
	}

	// Once the retired alias is consumed, the same request is a plain 404.
	_, again := router.Resolve(context.Background(), "u1", "shared", "openai-chat")
	if !errors.Is(again, ErrModelNotFound) || errors.Is(again, ErrModelNotAvailable) {
		t.Fatalf("repeat error = %v, want plain ErrModelNotFound", again)
	}
	if !view.contains("alpha") {
		t.Fatal("unaffected aliases must survive a version change")
	}
}

func TestAccountViewProtectedShadowChecksRequestedModel(t *testing.T) {
	protected := func(model string) bool { return model == "nvidia/llama-x" }
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("k-u1", "p1", "u1", SchemeNone)},
		KeyModels: []KeyModelRow{wildcardModel("k-u1", "p1", "u1", "nvidia/*", "*")},
	}
	router := routerFor(t, rows, StoreOptions{IsProtected: protected})
	view := router.accountView(router.store.Current(), "u1")

	if _, _, ok := view.lookup("nvidia/llama-x", protected); ok {
		t.Fatal("lookup ignored the protected-shadow rule")
	}
	if _, _, ok := view.lookup("nvidia/other", protected); !ok {
		t.Fatal("unprotected model must stay visible through the wildcard")
	}
}

func TestAccountViewTyped404DoesNotRevealOtherAccounts(t *testing.T) {
	router := routerFor(t, viewRows(), StoreOptions{})
	_, first := router.Resolve(context.Background(), "u1", "no-such-alias", "openai-chat")
	_, second := router.Resolve(context.Background(), "u1", "another-unknown", "openai-chat")
	if !errors.Is(first, ErrModelNotFound) || !errors.Is(second, ErrModelNotFound) {
		t.Fatalf("errors = %v, %v", first, second)
	}
	if first.Error() != second.Error() {
		t.Fatalf("answers differ: %q vs %q", first, second)
	}
	for _, hidden := range []string{"beta", "up-beta", "p1"} {
		if strings.Contains(first.Error(), hidden) {
			t.Fatalf("404 reveals %q: %v", hidden, first)
		}
	}
}

func TestViewCacheSingleFlight(t *testing.T) {
	cache := newViewCache(accountViewCapacity, nil)
	var builds atomic.Int32
	build := func() *accountView {
		builds.Add(1)
		time.Sleep(time.Millisecond)
		return &accountView{version: 0, personalExact: map[string]VisibleModel{"a": {Alias: "a"}}}
	}
	key := viewKey{userID: "u1", version: 1}

	const callers = 32
	views := make([]*accountView, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			views[i] = cache.get(key, build)
		}(i)
	}
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Fatalf("builds = %d, want 1", got)
	}
	for _, view := range views {
		if view != views[0] {
			t.Fatal("single-flight returned different views")
		}
	}
}

func TestViewCacheTTLExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := newViewCache(accountViewCapacity, func() time.Time { return now })
	var builds int
	build := func() *accountView {
		builds++
		return &accountView{version: 0}
	}
	key := viewKey{userID: "u1", version: 1}

	cache.get(key, build)
	now = now.Add(59 * time.Second)
	cache.get(key, build)
	if builds != 1 {
		t.Fatalf("builds within TTL = %d, want 1", builds)
	}
	now = now.Add(2 * time.Second)
	cache.get(key, build)
	if builds != 2 {
		t.Fatalf("builds after TTL = %d, want 2", builds)
	}
}

func TestViewCacheIsBounded(t *testing.T) {
	cache := newViewCache(2, nil)
	build := func() *accountView { return &accountView{version: 0} }
	sameVersion := func(user string) viewKey { return viewKey{userID: user, version: 1} }

	cache.get(sameVersion("u1"), build)
	cache.get(sameVersion("u1"), build)
	cache.get(sameVersion("u2"), build)
	cache.get(sameVersion("u3"), build)

	if got := len(cache.entries); got > 2 {
		t.Fatalf("cache holds %d entries, want <= 2", got)
	}
	if _, ok := cache.entries[sameVersion("u1")]; ok {
		t.Fatal("least recently used entry was not evicted")
	}
}

func TestAccountViewRedactionCanary(t *testing.T) {
	const (
		secret = "sk-canary-do-not-leak"
		keyID  = "k-canary"
	)
	ring := testKeyring(t)
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{sealKey(t, ring, testKey(keyID, "p1", "u1", SchemeBearer), []byte(secret))},
		KeyModels: []KeyModelRow{exactModel(keyID, "p1", "u1", "canary-model", "up")},
	}
	var audited []AuditEvent
	router := routerFor(t, rows, StoreOptions{Keyring: ring, Audit: func(event AuditEvent) { audited = append(audited, event) }})

	view := router.accountView(router.store.Current(), "u1")
	if _, err := router.Resolve(context.Background(), "u1", "canary-model", "openai-chat"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, err := router.Resolve(context.Background(), "u1", "no-such-alias", "openai-chat")

	for name, text := range map[string]string{
		"view":  fmt.Sprintf("%#v", view.snapshot()),
		"error": err.Error(),
	} {
		if strings.Contains(text, secret) || strings.Contains(text, keyID) {
			t.Fatalf("%s leaked key material: %s", name, text)
		}
	}
	if auditDump := fmt.Sprintf("%#v", audited); strings.Contains(auditDump, secret) {
		t.Fatalf("audit leaked key material: %s", auditDump)
	}
}
