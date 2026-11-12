package providerstore

import (
	"errors"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestLegacyViewAuthMapping(t *testing.T) {
	row := validProviderRow("p1", true)
	cases := []struct {
		name       string
		scheme     string
		headerName string
		want       provider.AuthScheme
		wantHeader string
	}{
		{"none", SchemeNone, "", provider.AuthNone, ""},
		{"bearer", SchemeBearer, "", provider.AuthBearer, ""},
		{"header custom", SchemeHeader, "X-Custom-Key", provider.AuthAPIKey, "X-Custom-Key"},
		{"header x-api-key", SchemeHeader, "x-api-key", provider.AuthAPIKey, ""},
		{"basic", SchemeBasic, "", provider.AuthScheme(SchemeBasic), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key", Enabled: true, AuthScheme: tc.scheme, AuthHeaderName: tc.headerName}
			view, err := LegacyView(row, []KeyRow{key}, nil, LegacyViewOptions{})
			if err != nil {
				t.Fatalf("legacy view: %v", err)
			}
			if view.Auth.Scheme != tc.want || view.Auth.HeaderName != tc.wantHeader {
				t.Fatalf("auth = %+v", view.Auth)
			}
			if view.Auth.Key != "" {
				t.Fatalf("auth.key must never be returned, got %q", view.Auth.Key)
			}
		})
	}
}

func TestLegacyViewNeverReturnsStoredSecret(t *testing.T) {
	ring := testKeyring(t)
	key := sealKey(t, ring, KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key", Enabled: true, AuthScheme: SchemeBearer}, []byte("sk-secret"))
	view, err := LegacyView(validProviderRow("p1", true), []KeyRow{key}, nil, LegacyViewOptions{})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if view.Auth.Key != "" {
		t.Fatalf("auth.key = %q", view.Auth.Key)
	}
}

func TestLegacyViewKeyEnvAndRefVisibility(t *testing.T) {
	key := KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "SOME_ENV", KeyRef: "import:foo"}

	view, err := LegacyView(validProviderRow("p1", true), []KeyRow{key}, nil, LegacyViewOptions{})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if view.Auth.KeyEnv != "" || view.Auth.KeyRef != "" {
		t.Fatalf("hidden metadata leaked: %+v", view.Auth)
	}

	view, err = LegacyView(validProviderRow("p1", true), []KeyRow{key}, nil, LegacyViewOptions{IncludeKeyEnv: true, IncludeKeyRef: true})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if view.Auth.KeyEnv != "SOME_ENV" || view.Auth.KeyRef != "import:foo" {
		t.Fatalf("metadata = %+v", view.Auth)
	}
}

func TestLegacyViewDeterministicKeySelection(t *testing.T) {
	keys := []KeyRow{
		{ID: "zz-personal", ProviderID: "p1", OwnerID: "u1", Name: "personal", Enabled: true, AuthScheme: SchemeBearer},
		{ID: "aa-shared", ProviderID: "p1", OwnerID: "", Name: "shared", Enabled: true, AuthScheme: SchemeNone},
	}
	view, err := LegacyView(validProviderRow("p1", true), keys, nil, LegacyViewOptions{})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if view.Auth.Scheme != provider.AuthNone {
		t.Fatalf("shared key should supply auth metadata: %+v", view.Auth)
	}
}

func TestLegacyViewModelsFilterAndDedupe(t *testing.T) {
	rows := []KeyModelRow{
		{KeyID: "k1", ProviderID: "p1", OwnerID: "u1", Alias: "b", UpstreamModel: "up-b", LegacyPosition: 2},
		{KeyID: "k1", ProviderID: "p1", OwnerID: "u1", Alias: "a", UpstreamModel: "up-a"},
		{KeyID: "k2", ProviderID: "p1", OwnerID: "u2", Alias: "a", UpstreamModel: "up-a2"},
		{KeyID: "k2", ProviderID: "p1", OwnerID: "u2", Alias: "c", UpstreamModel: "up-c"},
	}
	keys := []KeyRow{
		{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "k1", Enabled: true, AuthScheme: SchemeNone},
		{ID: "k2", ProviderID: "p1", OwnerID: "u2", Name: "k2", Enabled: true, AuthScheme: SchemeNone},
	}

	view, err := LegacyView(validProviderRow("p1", true), keys, rows, LegacyViewOptions{})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if len(view.Models) != 3 {
		t.Fatalf("models = %+v", view.Models)
	}
	if view.Models[0].Alias != "a" || view.Models[0].UpstreamModel != "up-a" {
		t.Fatalf("dedupe should keep the lowest key id: %+v", view.Models[0])
	}

	view, err = LegacyView(validProviderRow("p1", true), keys, rows, LegacyViewOptions{VisibleKeyIDs: []string{"k2"}})
	if err != nil {
		t.Fatalf("legacy view: %v", err)
	}
	if len(view.Models) != 2 || view.Models[0].UpstreamModel != "up-a2" || view.Models[1].Alias != "c" {
		t.Fatalf("filtered models = %+v", view.Models)
	}
}

func TestLegacyModelsTargetRequiresOneVisibleKey(t *testing.T) {
	keys := []KeyRow{
		{ID: "k1", ProviderID: "p1", OwnerID: "u1", Enabled: true, AuthScheme: SchemeNone},
		{ID: "k2", ProviderID: "p1", OwnerID: "u1", Enabled: true, AuthScheme: SchemeNone},
	}
	if _, err := LegacyModelsTarget(keys, LegacyViewOptions{}); !errors.Is(err, ErrModelsAmbiguous) {
		t.Fatalf("multiple keys error = %v", err)
	}
	if _, err := LegacyModelsTarget(nil, LegacyViewOptions{}); !errors.Is(err, ErrModelsAmbiguous) {
		t.Fatalf("no keys error = %v", err)
	}
	target, err := LegacyModelsTarget(keys, LegacyViewOptions{VisibleKeyIDs: []string{"k2"}})
	if err != nil || target != "k2" {
		t.Fatalf("target = %q, %v", target, err)
	}
}
