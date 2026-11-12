package providerstore

import (
	"context"
	"errors"
	"testing"
)

func TestBuildSnapshotValid(t *testing.T) {
	rows := baseRows()
	rows.Settings = []SettingRow{{
		Key:   "automode",
		Value: []byte(`{"model":"model-1","chat_responses":true}`),
	}}
	snapshot := mustBuildSnapshot(t, rows, 7, BuildOptions{Now: fixedNow})
	if snapshot.Version() != 7 {
		t.Fatalf("version = %d", snapshot.Version())
	}
	if !snapshot.LoadedAt().Equal(fixedNow()) {
		t.Fatalf("loadedAt = %v", snapshot.LoadedAt())
	}
	if snapshot.Settings().Automode.Model != "model-1" || !snapshot.Settings().Automode.ChatResponses {
		t.Fatalf("settings = %+v", snapshot.Settings())
	}
	definition, ok := snapshot.Provider("p1")
	if !ok || definition.ID != "p1" {
		t.Fatalf("provider = %+v, %v", definition, ok)
	}
	if len(snapshot.Diagnostics()) != 0 {
		t.Fatalf("diagnostics = %+v", snapshot.Diagnostics())
	}

	// Snapshot accessors return defensive copies.
	definition.Headers = map[string]string{"X-Mutated": "yes"}
	fresh, _ := snapshot.Provider("p1")
	if _, leaked := fresh.Headers["X-Mutated"]; leaked {
		t.Fatal("Provider returned an alias of the snapshot definition")
	}
}

func TestBuildSnapshotRejectsInvalidRows(t *testing.T) {
	mutations := map[string]func(*Rows){
		"duplicate provider": func(rows *Rows) {
			rows.Providers = append(rows.Providers, rows.Providers[0])
		},
		"invalid protocol": func(rows *Rows) {
			rows.Providers[0].Protocol = "not-a-protocol"
		},
		"invalid jsonb": func(rows *Rows) {
			rows.Providers[0].Headers = []byte(`{"broken":`)
		},
		"duplicate key": func(rows *Rows) {
			rows.Keys = append(rows.Keys, rows.Keys[0])
		},
		"key unknown provider": func(rows *Rows) {
			rows.Keys[0].ProviderID = "missing"
		},
		"key model unknown key": func(rows *Rows) {
			rows.KeyModels[0].KeyID = "missing"
		},
		"key model provider drift": func(rows *Rows) {
			rows.KeyModels[0].ProviderID = "other"
		},
		"key model owner drift": func(rows *Rows) {
			rows.KeyModels[0].OwnerID = "other"
		},
		"duplicate model state": func(rows *Rows) {
			rows.ModelState = []ModelStateRow{{ModelID: "m"}, {ModelID: "m"}}
		},
		"empty model state": func(rows *Rows) {
			rows.ModelState = []ModelStateRow{{}}
		},
		"unknown setting": func(rows *Rows) {
			rows.Settings = []SettingRow{{Key: "nope", Value: []byte(`{}`)}}
		},
		"invalid setting": func(rows *Rows) {
			rows.Settings = []SettingRow{{Key: "automode", Value: []byte(`[]`)}}
		},
		"sensitive config": func(rows *Rows) {
			rows.Providers[0].BaseURL = "https://user:pass@example.com/v1"
		},
		"shared credential key": func(rows *Rows) {
			rows.Keys[0].OwnerID = ""
			rows.Keys[0].AuthScheme = SchemeBearer
			rows.Keys[0].KeyEnv = "GNVIPI_TEST_KEY_ENV"
			rows.KeyModels[0].OwnerID = ""
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			rows := baseRows()
			mutate(&rows)
			if _, err := BuildSnapshot(rows, 1, BuildOptions{}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := BuildSnapshot(baseRows(), -1, BuildOptions{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative version error = %v", err)
	}
}

func TestBuildSnapshotDecryptFailureIsDiagnosticAndExcluded(t *testing.T) {
	ring := testKeyring(t)
	rows := baseRows()
	key := KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key", Enabled: true, AuthScheme: SchemeBearer}
	key = sealKey(t, ring, key, []byte("sk-secret"))
	key.SecretEnc[0] ^= 0xff
	rows.Keys = []KeyRow{key}

	snapshot := mustBuildSnapshot(t, rows, 1, BuildOptions{Keyring: ring})
	diagnostics := snapshot.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Code != "provider_key_decrypt_failed" || diagnostics[0].KeyID != "k1" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if _, ok := snapshot.personalEx["u1"]["model-1"]; !ok {
		t.Fatal("excluded candidate was not indexed; router coverage expects it to exist")
	}
}

func TestBuildSnapshotDisabledProviderAndKeyAreNotRoutable(t *testing.T) {
	for name, mutate := range map[string]func(*Rows){
		"disabled provider": func(rows *Rows) { rows.Providers[0].Enabled = false },
		"disabled key":      func(rows *Rows) { rows.Keys[0].Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			rows := baseRows()
			mutate(&rows)
			snapshot := mustBuildSnapshot(t, rows, 1, BuildOptions{})
			router := NewRouter(snapshotAsStore(snapshot, StoreOptions{}))
			if _, err := router.Resolve(context.Background(), "u1", "model-1", "openai-chat"); !errors.Is(err, ErrModelNotFound) {
				t.Fatalf("resolve error = %v, want ErrModelNotFound", err)
			}
		})
	}
}

func TestStorePublishesAndKeepsLastKnownGood(t *testing.T) {
	rows := baseRows()
	var loadErr error
	var loadCalls int
	source := &stubSource{
		load: func(context.Context) (Rows, int64, error) {
			loadCalls++
			if loadErr != nil {
				return Rows{}, 0, loadErr
			}
			return rows, 3, nil
		},
		version: func(context.Context) (int64, error) { return 3, nil },
	}
	store := NewStore(source, StoreOptions{Now: fixedNow})
	if store.Current() != nil || store.Version() != 0 {
		t.Fatal("store published before reload")
	}
	if err := store.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if store.Version() != 3 || store.LastError() != nil {
		t.Fatalf("version=%d lastErr=%v", store.Version(), store.LastError())
	}
	published := store.Current()

	loadErr = errors.New("database unavailable")
	if err := store.Reload(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("reload error = %v", err)
	}
	if store.Current() != published {
		t.Fatal("failed reload replaced the published snapshot")
	}
	if store.LastError() == nil {
		t.Fatal("last error not recorded")
	}

	// A structurally invalid row keeps the previous snapshot as well.
	loadErr = nil
	rows = baseRows()
	rows.Providers[0].Protocol = "broken"
	if err := store.Reload(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid reload error = %v", err)
	}
	if store.Current() != published {
		t.Fatal("invalid reload replaced the published snapshot")
	}
	if loadCalls < 3 {
		t.Fatalf("loadCalls = %d", loadCalls)
	}
}

func TestStoreReloadsWhenVersionAdvances(t *testing.T) {
	versions := []int64{1, 2}
	var loadCalls int
	source := &stubSource{
		load: func(context.Context) (Rows, int64, error) {
			version := versions[loadCalls]
			loadCalls++
			return baseRows(), version, nil
		},
		version: func(context.Context) (int64, error) {
			if loadCalls == 1 {
				return 2, nil
			}
			return 2, nil
		},
	}
	store := NewStore(source, StoreOptions{Now: fixedNow})
	if err := store.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loadCalls != 2 || store.Version() != 2 {
		t.Fatalf("loadCalls=%d version=%d", loadCalls, store.Version())
	}
}

func TestStoreGivesUpWhenVersionKeepsAdvancing(t *testing.T) {
	var version int64
	source := &stubSource{
		load: func(context.Context) (Rows, int64, error) {
			version++
			return baseRows(), version, nil
		},
		version: func(context.Context) (int64, error) { return version + 1, nil },
	}
	store := NewStore(source, StoreOptions{Now: fixedNow})
	if err := store.Reload(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("reload error = %v", err)
	}
	if store.LastError() == nil {
		t.Fatal("last error not recorded")
	}
}
