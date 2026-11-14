package providerstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func exactModel(keyID, providerID, owner, alias, upstream string, fallbacks ...string) KeyModelRow {
	row := KeyModelRow{KeyID: keyID, ProviderID: providerID, OwnerID: owner, Alias: alias, UpstreamModel: upstream}
	if len(fallbacks) > 0 {
		raw, _ := json.Marshal(fallbacks) //nolint:errcheck // best-effort cleanup/best-effort parse
		row.Fallbacks = raw
	}
	return row
}

//nolint:unparam // test helper keeps explicit call-site shape
func wildcardModel(keyID, providerID, owner, alias, upstream string) KeyModelRow {
	return KeyModelRow{KeyID: keyID, ProviderID: providerID, OwnerID: owner, Alias: alias, UpstreamModel: upstream}
}

func testKey(id, providerID, owner, scheme string) KeyRow {
	return KeyRow{ID: id, ProviderID: providerID, OwnerID: owner, Name: id, Enabled: true, AuthScheme: scheme}
}

func routerFor(t *testing.T, rows Rows, opts StoreOptions) *Router {
	t.Helper()
	snapshot := mustBuildSnapshot(t, rows, 1, BuildOptions{Keyring: opts.Keyring, Refs: opts.Refs, IsProtected: opts.IsProtected, Now: fixedNow})
	return NewRouter(snapshotAsStore(snapshot, opts))
}

func TestRouterPrecedence(t *testing.T) {
	const model = "m/1"
	providers := []ProviderRow{validProviderRow("p1", true)}
	all := func() Rows {
		return Rows{
			Providers: providers,
			Keys: []KeyRow{
				testKey("kp-exact", "p1", "u1", SchemeNone),
				testKey("kp-wc", "p1", "u1", SchemeNone),
				testKey("ks-exact", "p1", "", SchemeNone),
				testKey("ks-wc", "p1", "", SchemeNone),
			},
			KeyModels: []KeyModelRow{
				exactModel("kp-exact", "p1", "u1", model, "up"),
				wildcardModel("kp-wc", "p1", "u1", "m/*", "*"),
				exactModel("ks-exact", "p1", "", model, "up"),
				wildcardModel("ks-wc", "p1", "", "m/*", "*"),
			},
		}
	}

	cases := []struct {
		name string
		drop []string
		want string
	}{
		{"personal exact", nil, "kp-exact"},
		{"personal wildcard", []string{"kp-exact"}, "kp-wc"},
		{"shared exact", []string{"kp-exact", "kp-wc"}, "ks-exact"},
		{"shared wildcard", []string{"kp-exact", "kp-wc", "ks-exact"}, "ks-wc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := all()
			drop := map[string]bool{}
			for _, key := range tc.drop {
				drop[key] = true
			}
			var keys []KeyRow
			var models []KeyModelRow
			for _, key := range rows.Keys {
				if !drop[key.ID] {
					keys = append(keys, key)
				}
			}
			for _, m := range rows.KeyModels {
				if !drop[m.KeyID] {
					models = append(models, m)
				}
			}
			rows.Keys, rows.KeyModels = keys, models
			route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", model, "openai-chat")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if route.KeyID != tc.want {
				t.Fatalf("key = %s, want %s", route.KeyID, tc.want)
			}
			if route.UpstreamModel != "up" && tc.want == "kp-exact" {
				t.Fatalf("upstream = %s", route.UpstreamModel)
			}
		})
	}
}

func TestRouterLongestWildcardAndSubstitution(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			testKey("short", "p1", "u1", SchemeNone),
			testKey("long", "p1", "u1", SchemeNone),
			testKey("fixed", "p1", "u1", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			wildcardModel("short", "p1", "u1", "m/*", "short/*"),
			wildcardModel("long", "p1", "u1", "m/su/*", "long/*"),
			wildcardModel("fixed", "p1", "u1", "fixed/*", "one-model"),
		},
	}
	router := routerFor(t, rows, StoreOptions{})

	route, err := router.Resolve(context.Background(), "u1", "m/su/x", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "long" || route.UpstreamModel != "long/x" {
		t.Fatalf("route = %+v", route)
	}

	route, err = router.Resolve(context.Background(), "u1", "m/other", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "short" || route.UpstreamModel != "short/other" {
		t.Fatalf("route = %+v", route)
	}

	route, err = router.Resolve(context.Background(), "u1", "fixed/anything", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.UpstreamModel != "one-model" {
		t.Fatalf("fixed upstream = %s", route.UpstreamModel)
	}
}

func TestRouterDeterministicOrdering(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true), validProviderRow("p2", true)},
		Keys: []KeyRow{
			testKey("k2", "p2", "u1", SchemeNone),
			testKey("k1", "p1", "u1", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("k2", "p2", "u1", "m", "up2"),
			exactModel("k1", "p1", "u1", "m", "up1"),
		},
	}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "k1" || route.ProviderID != "p1" {
		t.Fatalf("route = %+v", route)
	}

	// Legacy positions order positive values ascending.
	rows.KeyModels[0].LegacyPosition = 2
	rows.KeyModels[1].LegacyPosition = 1
	route, err = routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "k1" {
		t.Fatalf("legacy ordering route = %+v", route)
	}
}

func TestRouterCrossUserIsolation(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("k", "p1", "u2", SchemeNone)},
		KeyModels: []KeyModelRow{exactModel("k", "p1", "u2", "private", "up")},
	}
	router := routerFor(t, rows, StoreOptions{})
	if _, err := router.Resolve(context.Background(), "u1", "private", "openai-chat"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("error = %v, want ErrKeyRequired", err)
	}
	if _, err := router.Resolve(context.Background(), "u2", "private", "openai-chat"); err != nil {
		t.Fatalf("owner resolve: %v", err)
	}
}

func TestRouterSharedNoneKey(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("k", "p1", "", SchemeNone)},
		KeyModels: []KeyModelRow{exactModel("k", "p1", "", "shared", "up")},
	}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "shared", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "k" || route.OwnerID != "" {
		t.Fatalf("route = %+v", route)
	}
	if route.Provider.Auth.Scheme != provider.AuthNone {
		t.Fatalf("auth = %+v", route.Provider.Auth)
	}
}

func TestRouterHiddenModelAndMapping(t *testing.T) {
	base := func() Rows {
		return Rows{
			Providers: []ProviderRow{validProviderRow("p1", true)},
			Keys:      []KeyRow{testKey("k", "p1", "u1", SchemeNone)},
			KeyModels: []KeyModelRow{exactModel("k", "p1", "u1", "m", "up")},
		}
	}

	rows := base()
	rows.KeyModels[0].Hidden = true
	if _, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat"); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("hidden mapping error = %v", err)
	}

	rows = base()
	rows.ModelState = []ModelStateRow{{ModelID: "m", Hidden: true}}
	if _, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat"); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("hidden state error = %v", err)
	}

	rows = base()
	rows.ModelState = []ModelStateRow{{ModelID: "m", Deleted: true}}
	if _, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat"); !errors.Is(err, ErrModelHidden) {
		t.Fatalf("deleted state error = %v", err)
	}
}

func TestRouterProtectedModelShadow(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("k", "p1", "u1", SchemeNone)},
		KeyModels: []KeyModelRow{exactModel("k", "p1", "u1", "protected/model", "up")},
	}
	protected := func(model string) bool { return model == "protected/model" }
	if _, err := routerFor(t, rows, StoreOptions{IsProtected: protected}).Resolve(context.Background(), "u1", "protected/model", "openai-chat"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("shadow error = %v", err)
	}

	rows.KeyModels[0].AllowShadow = true
	route, err := routerFor(t, rows, StoreOptions{IsProtected: protected}).Resolve(context.Background(), "u1", "protected/model", "openai-chat")
	if err != nil || route.KeyID != "k" {
		t.Fatalf("allowed shadow route = %+v, %v", route, err)
	}
}

func TestRouterFallbacksAreExactOnlyAndCycleSafe(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			testKey("k-main", "p1", "u1", SchemeNone),
			testKey("k-fb", "p1", "u1", SchemeNone),
			testKey("k-wild", "p1", "u1", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("k-main", "p1", "u1", "main", "up", "fb"),
			exactModel("k-fb", "p1", "u1", "fb", "up", "main"),
			wildcardModel("k-wild", "p1", "u1", "wild/*", "*"),
		},
	}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "main", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(route.Candidates) != 2 || route.Candidates[1].KeyID != "k-fb" {
		t.Fatalf("candidates = %+v", route.Candidates)
	}

	// A fallback alias is never satisfied by a wildcard mapping.
	rows.KeyModels[0].Fallbacks, _ = json.Marshal([]string{"wild/thing"}) //nolint:errcheck // best-effort cleanup/best-effort parse
	route, err = routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "main", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(route.Candidates) != 1 {
		t.Fatalf("wildcard fallback matched: %+v", route.Candidates)
	}
}

func TestRouterFallbackChainIsCappedAtEight(t *testing.T) {
	const size = 12
	aliases := make([]string, size)
	keys := make([]KeyRow, size)
	models := make([]KeyModelRow, size)
	for i := 0; i < size; i++ {
		alias := string(rune('a' + i))
		aliases[i] = alias
		keyID := "k-" + alias
		keys[i] = testKey(keyID, "p1", "u1", SchemeNone)
		var fallbacks []string
		if i+1 < size {
			fallbacks = []string{string(rune('a' + i + 1))}
		}
		models[i] = exactModel(keyID, "p1", "u1", alias, "up", fallbacks...)
	}
	rows := Rows{Providers: []ProviderRow{validProviderRow("p1", true)}, Keys: keys, KeyModels: models}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "a", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(route.Candidates) != 8 {
		t.Fatalf("candidates = %d, want 8", len(route.Candidates))
	}
}

func TestRouterTriesAllCandidatesInWinningTier(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			{ID: "k1-env", ProviderID: "p1", OwnerID: "u1", Name: "env", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "GNVIPI_KEY_ROUTER_TEST_UNSET"},
			testKey("k2-ok", "p1", "u1", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("k1-env", "p1", "u1", "m", "up-env"),
			exactModel("k2-ok", "p1", "u1", "m", "up-ok"),
		},
	}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "m", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "k2-ok" || route.UpstreamModel != "up-ok" {
		t.Fatalf("route = %+v", route)
	}
	if len(route.Candidates) != 1 {
		t.Fatalf("candidates = %+v", route.Candidates)
	}
}

func TestRouterAuditEvents(t *testing.T) {
	var events []AuditEvent
	audit := func(event AuditEvent) { events = append(events, event) }

	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "env", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "GNVIPI_KEY_ROUTER_TEST_UNSET"},
		},
		KeyModels: []KeyModelRow{exactModel("k1", "p1", "u1", "m", "up")},
	}
	router := routerFor(t, rows, StoreOptions{Audit: audit})
	if _, err := router.Resolve(context.Background(), "u1", "m", "openai-chat"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("resolve error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Code != AuditCredentialUnavailable || events[0].UserID != "u1" || events[0].Model != "m" {
		t.Fatalf("event = %+v", events[0])
	}
	if len(events[0].KeyIDs) != 1 || events[0].KeyIDs[0] != "k1" || events[0].ProviderIDs[0] != "p1" {
		t.Fatalf("event IDs = %+v", events[0])
	}

	// Snapshot-time decrypt failures emit the decrypt code with IDs only.
	ring := testKeyring(t)
	key := sealKey(t, ring, testKey("k1", "p1", "u1", SchemeBearer), []byte("sk-secret"))
	key.SecretEnc[0] ^= 0xff
	rows = Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{key},
		KeyModels: []KeyModelRow{exactModel("k1", "p1", "u1", "m", "up")},
	}
	mustBuildSnapshot(t, rows, 1, BuildOptions{Keyring: ring, Audit: audit})
	if len(events) != 2 || events[1].Code != AuditDecryptFailed || events[1].KeyIDs[0] != "k1" {
		t.Fatalf("events = %+v", events)
	}
	if rendered := fmt.Sprintf("%+v", events[1]); rendered != "" && strings.Contains(rendered, "sk-secret") {
		t.Fatalf("audit event leaks a secret: %s", rendered)
	}
}

func TestRouterSkipsUnresolvableCredential(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{
			{ID: "k-env", ProviderID: "p1", OwnerID: "u1", Name: "env", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "GNVIPI_KEY_ROUTER_TEST_UNSET"},
			testKey("k-fb", "p1", "u1", SchemeNone),
		},
		KeyModels: []KeyModelRow{
			exactModel("k-env", "p1", "u1", "main", "up", "fb"),
			exactModel("k-fb", "p1", "u1", "fb", "up"),
		},
	}
	route, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "main", "openai-chat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.KeyID != "k-fb" || len(route.Candidates) != 1 {
		t.Fatalf("route = %+v", route)
	}

	rows.Keys = rows.Keys[:1]
	rows.KeyModels = rows.KeyModels[:1]
	if _, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "main", "openai-chat"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("all unavailable error = %v", err)
	}
}

func TestRouterMaterializesAuthSchemes(t *testing.T) {
	ring := testKeyring(t)
	bearer := sealKey(t, ring, testKey("k-bearer", "p1", "u1", SchemeBearer), []byte("sk-bearer"))
	header := testKey("k-header", "p1", "u1", SchemeHeader)
	header.AuthHeaderName = "X-Custom-Key"
	header = sealKey(t, ring, header, []byte("header-secret"))
	apiKey := testKey("k-apikey", "p1", "u1", SchemeHeader)
	apiKey.AuthHeaderName = "x-api-key"
	apiKey = sealKey(t, ring, apiKey, []byte("apikey-secret"))
	basic := sealKey(t, ring, testKey("k-basic", "p1", "u1", SchemeBasic), []byte(`{"username":"alice","password":"p@ss"}`))

	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{bearer, header, apiKey, basic},
		KeyModels: []KeyModelRow{
			exactModel("k-bearer", "p1", "u1", "bearer-model", "up"),
			exactModel("k-header", "p1", "u1", "header-model", "up"),
			exactModel("k-apikey", "p1", "u1", "apikey-model", "up"),
			exactModel("k-basic", "p1", "u1", "basic-model", "up"),
		},
	}
	router := routerFor(t, rows, StoreOptions{Keyring: ring})

	route, err := router.Resolve(context.Background(), "u1", "bearer-model", "openai-chat")
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	if route.Provider.Auth.Scheme != provider.AuthBearer || route.Provider.Auth.Key != "sk-bearer" {
		t.Fatalf("bearer auth = %+v", route.Provider.Auth)
	}

	route, err = router.Resolve(context.Background(), "u1", "header-model", "openai-chat")
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if route.Provider.Auth.Scheme != provider.AuthAPIKey || route.Provider.Auth.Key != "header-secret" || route.Provider.Auth.HeaderName != "X-Custom-Key" {
		t.Fatalf("header auth = %+v", route.Provider.Auth)
	}

	route, err = router.Resolve(context.Background(), "u1", "apikey-model", "openai-chat")
	if err != nil {
		t.Fatalf("apikey: %v", err)
	}
	if route.Provider.Auth.HeaderName != "" {
		t.Fatalf("x-api-key header name = %q", route.Provider.Auth.HeaderName)
	}

	route, err = router.Resolve(context.Background(), "u1", "basic-model", "openai-chat")
	if err != nil {
		t.Fatalf("basic: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p@ss"))
	if route.Provider.Auth.Scheme != provider.AuthAPIKey || route.Provider.Auth.Key != want || route.Provider.Auth.HeaderName != "Authorization" {
		t.Fatalf("basic auth = %+v", route.Provider.Auth)
	}
}

func TestRouterWithoutSnapshot(t *testing.T) {
	store := NewStore(&stubSource{
		load:    func(context.Context) (Rows, int64, error) { return Rows{}, 0, nil },
		version: func(context.Context) (int64, error) { return 0, nil },
	}, StoreOptions{})
	if _, err := NewRouter(store).Resolve(context.Background(), "u1", "model-1", "openai-chat"); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("error = %v", err)
	}
}

func TestRouterExcludesUndecryptableKey(t *testing.T) {
	ring := testKeyring(t)
	key := sealKey(t, ring, testKey("k1", "p1", "u1", SchemeBearer), []byte("sk-secret"))
	key.SecretEnc[0] ^= 0xff
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{key},
		KeyModels: []KeyModelRow{exactModel("k1", "p1", "u1", "model-1", "up")},
	}
	router := routerFor(t, rows, StoreOptions{Keyring: ring})
	if _, err := router.Resolve(context.Background(), "u1", "model-1", "openai-chat"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("resolve error = %v", err)
	}
}

func TestRouterInvalidInputs(t *testing.T) {
	rows := baseRows()
	router := routerFor(t, rows, StoreOptions{})
	if _, err := router.Resolve(context.Background(), "u1", " ", "openai-chat"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("empty model error = %v", err)
	}
	if _, err := router.Resolve(context.Background(), "u1", "unknown", "openai-chat"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("unknown model error = %v", err)
	}
}

func TestStatusForErrorAndFallbackTriggered(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrModelNotFound, http.StatusNotFound},
		{ErrModelHidden, http.StatusNotFound},
		{ErrKeyRequired, http.StatusForbidden},
		{ErrCredentialUnavailable, http.StatusServiceUnavailable},
		{ErrUnsupportedReference, http.StatusServiceUnavailable},
		{ErrNoSnapshot, http.StatusServiceUnavailable},
		{ErrMissingMasterKey, http.StatusServiceUnavailable},
		{ErrDecrypt, http.StatusServiceUnavailable},
		{ErrUnknownKeyVersion, http.StatusServiceUnavailable},
		{errors.New("other"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := StatusForError(tc.err); got != tc.want {
			t.Fatalf("StatusForError(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}

	if !FallbackTriggered(http.StatusRequestTimeout, nil) || !FallbackTriggered(http.StatusTooManyRequests, nil) || !FallbackTriggered(http.StatusBadGateway, nil) {
		t.Fatal("default fallback statuses must include 408, 429 and 5xx")
	}
	if FallbackTriggered(http.StatusBadRequest, nil) {
		t.Fatal("400 must not trigger a default fallback")
	}
	if !FallbackTriggered(http.StatusBadRequest, []int{400}) || FallbackTriggered(500, []int{400}) {
		t.Fatal("explicit fallback statuses must override the default")
	}
}

func TestRouterKeyRequiredNamesProviderAndAction(t *testing.T) {
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{testKey("k-other", "p1", "u2", SchemeNone)},
		KeyModels: []KeyModelRow{exactModel("k-other", "p1", "u2", "private", "up")},
	}
	_, err := routerFor(t, rows, StoreOptions{}).Resolve(context.Background(), "u1", "private", "openai-chat")
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("error = %v, want ErrKeyRequired", err)
	}
	for _, want := range []string{"p1", "private", "add an enabled provider key"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "up") {
		t.Fatalf("error leaks the other user's mapping: %v", err)
	}
}

func TestRouterCredentialSourceMatrix(t *testing.T) {
	ring := testKeyring(t)
	stored := sealKey(t, ring, testKey("k-secret", "p1", "u1", SchemeBearer), []byte("stored-secret"))
	t.Setenv("GNVIPI_KEY_ROUTER_REF_ENV", "env-secret")
	env := testKey("k-env", "p1", "u1", SchemeBearer)
	env.KeyEnv = "GNVIPI_KEY_ROUTER_REF_ENV"
	ref := testKey("k-ref", "p1", "u1", SchemeBearer)
	ref.KeyRef = "import:router"
	rows := Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys:      []KeyRow{stored, env, ref},
		KeyModels: []KeyModelRow{
			exactModel("k-secret", "p1", "u1", "secret", "up-secret"),
			exactModel("k-env", "p1", "u1", "env", "up-env"),
			exactModel("k-ref", "p1", "u1", "ref", "up-ref"),
		},
	}
	router := routerFor(t, rows, StoreOptions{Keyring: ring, Refs: MapRefResolver{"import:router": "ref-secret"}})
	for alias, want := range map[string]string{"secret": "stored-secret", "env": "env-secret", "ref": "ref-secret"} {
		route, err := router.Resolve(context.Background(), "u1", alias, "openai-chat")
		if err != nil {
			t.Fatalf("%s: resolve: %v", alias, err)
		}
		if route.Provider.Auth.Key != want {
			t.Fatalf("%s: auth key = %q, want %q", alias, route.Provider.Auth.Key, want)
		}
	}
}
