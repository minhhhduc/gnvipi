package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func persistenceProvider(id string) Provider {
	return Provider{ID: id, Name: id, BaseURL: "https://up.invalid/v1", Protocol: ir.ProtocolOpenAIChat,
		Auth: Auth{Scheme: AuthBearer, Key: "secret"}, Enabled: true, TLS: TLSChrome,
		Headers:  map[string]string{"X-Tenant": "tenant"},
		Models:   []ModelMapping{{Alias: id + "/model", UpstreamModel: "model"}, {Alias: id + "/hidden", UpstreamModel: "hidden", Hidden: true}},
		Timeouts: Timeouts{Connect: time.Second, ResponseHeader: 2 * time.Second, Idle: 3 * time.Second}}
}

func TestDocumentMigratesLegacyModesAndPreservesCatalog(t *testing.T) {
	raw := []byte(`{"_meta":{"revision":"keep"},"groups":[{"publisher":"local","extra":7,"models":[{"model":"local/m","slug":"s","function_id":"f","extra":true}]}],"masks_id":["vendor/a"],"deleted_id":["vendor/b"],"extra":{"large":9007199254740993},"providers":[{"name":"vendor","base_url":"https://up.invalid/v1","api_key":"k","model":"a"},{"name":"vendor","base_url":"https://up.invalid/v1","api_key":"k","model":"b","alias":"custom"},{"name":"vendor","base_url":"https://up.invalid/v1","model":"msg","messages_only":true},{"name":"vendor","base_url":"https://up.invalid/v1","model":"rsp","responses_only":true}]}`)
	doc, err := DecodeDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != 3 || len(doc.Providers[0].Models) != 2 {
		t.Fatalf("migration = %#v", doc.Providers)
	}
	view := LegacyView(doc.Providers)
	if len(view) != 4 || view[0].Alias != "vendor/a" || view[1].Alias != "custom" || !view[2].MessagesOnly || !view[3].ResponsesOnly {
		t.Fatalf("legacy routing = %#v", view)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if _, found := after["providers"]; found {
		t.Fatal("legacy providers written after migration")
	}
	for _, field := range []string{"_meta", "groups", "masks_id", "deleted_id", "extra"} {
		if !bytes.Equal(before[field], after[field]) {
			t.Fatalf("%s changed: %s", field, after[field])
		}
	}
	reloaded, err := DecodeDocument(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(LegacyView(reloaded.Providers), view) {
		t.Fatal("routing changed after reload")
	}
}

func TestDocumentFullV2RoundtripAndLegacyEdit(t *testing.T) {
	original := persistenceProvider("vendor")
	original.Auth = Auth{Scheme: AuthAPIKey, KeyEnv: "PERSISTENCE_TEST_KEY"}
	t.Setenv("PERSISTENCE_TEST_KEY", "env-secret")
	doc := Document{Providers: []Provider{original}, Fields: map[string]json.RawMessage{"unrelated": json.RawMessage(`{"keep":true}`)}}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := DecodeDocument(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Providers, doc.Providers) {
		t.Fatalf("v2 roundtrip = %#v", loaded.Providers)
	}
	view := LegacyView(loaded.Providers)
	view[0].BaseURL = "https://changed.invalid/v1"
	updated, err := MergeLegacy(loaded.Providers, view)
	if err != nil {
		t.Fatal(err)
	}
	want := original
	want.BaseURL = view[0].BaseURL
	want.Models[0].LegacyPosition = 1
	if !reflect.DeepEqual(updated, []Provider{want}) {
		t.Fatalf("legacy update lost v2 settings: %#v", updated)
	}
}

func TestStoreConcurrentUpdatesPublishAfterDiskAndNotifyInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	var callbackErr error
	var counts []int
	store, err := OpenStore(path, func(list []Provider) {
		disk, err := LoadDocument(path)
		if err != nil {
			callbackErr = err
			return
		}
		if !reflect.DeepEqual(disk.Providers, list) {
			callbackErr = fmt.Errorf("callback ran before persistence")
		}
		counts = append(counts, len(list))
		list[0].Headers["X-Tenant"] = "mutated callback"
	})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := persistenceProvider(fmt.Sprintf("p%d", i))
			errs <- store.Upsert(&p)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	for i, count := range counts {
		if count != i+1 {
			t.Fatalf("callback order = %v", counts)
		}
	}
	if len(counts) != workers || len(store.List()) != workers {
		t.Fatalf("lost concurrent upserts: %v", counts)
	}
	snapshot := store.List()
	snapshot[0].Models[0].Alias = "changed"
	snapshot[0].Headers["X-Tenant"] = "changed"
	got, ok := store.Get(snapshot[0].ID)
	if !ok || got.Models[0].Alias == "changed" || got.Headers["X-Tenant"] != "tenant" {
		t.Fatalf("snapshot escaped: %#v", got)
	}
	if err := store.Delete(got.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(got.ID); ok {
		t.Fatal("deleted provider remains")
	}
}

func TestStoreFailureLeavesDiskSnapshotAndCallbacksUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	calls := 0
	store, err := OpenStore(path, func([]Provider) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	good := persistenceProvider("vendor")
	if err := store.Upsert(&good); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.BaseURL = "not a URL"
	if err := store.Upsert(&bad); err == nil {
		t.Fatal("invalid update accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("validation failure changed disk")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "old")
	if err := os.WriteFile(marker, before, 0o600); err != nil {
		t.Fatal(err)
	}
	good.BaseURL = "https://changed.invalid/v1"
	if err := store.Upsert(&good); err == nil {
		t.Fatal("rename failure accepted")
	}
	if err := store.Delete(good.ID); err == nil {
		t.Fatal("failed delete accepted")
	}
	got, _ := store.Get(good.ID)
	if got.BaseURL != "https://up.invalid/v1" || calls != 1 {
		t.Fatalf("failed update published: %#v, calls %d", got, calls)
	}
	markerData, err := os.ReadFile(marker)
	if err != nil || !bytes.Equal(markerData, before) {
		t.Fatalf("previous file lost: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file leaked: %v", err)
	}
}

func TestStoreRejectsMalformedAndFutureDocumentsWithoutWriting(t *testing.T) {
	for _, raw := range []string{`{"providers_v2":`, `null`, `{"schema_version":3,"providers_v2":[]}`, `{"schema_version":2}`, `{"schema_version":2,"providers_v2":null}`} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenStore(path, nil); err == nil {
				t.Fatal("unsafe document accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != raw {
				t.Fatalf("unsafe document overwritten: %v", err)
			}
		})
	}
}

func TestMergeLegacyPreservesDistinctV2ProviderSettings(t *testing.T) {
	first, second := persistenceProvider("first-id"), persistenceProvider("second-id")
	first.Name, second.Name = "vendor", "vendor"
	second.Auth = Auth{Scheme: AuthAPIKey, KeyEnv: "DISTINCT_V2_KEY"}
	t.Setenv("DISTINCT_V2_KEY", first.Auth.Key)
	second.Headers["X-Tenant"] = "second"
	second.Timeouts.Idle = 7 * time.Second
	second.TLS = TLSDefault
	// Both providers have only visible mappings, as in the destructive case.
	first.Models, second.Models = first.Models[:1], second.Models[:1]
	current := []Provider{first, second}
	rows := append(LegacyView(current), LegacyProvider{Name: "other", BaseURL: "https://other.invalid/v1", Model: "other", Alias: "other"})
	got, err := MergeLegacy(current, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("distinct providers were coalesced: %#v", got)
	}
	first.Models[0].LegacyPosition, second.Models[0].LegacyPosition = 1, 2
	if !reflect.DeepEqual(got[:2], []Provider{first, second}) {
		t.Fatalf("existing IDs/settings changed: %#v", got[:2])
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := WriteDocument(path, Document{Providers: got}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Providers, got) {
		t.Fatalf("settings changed after restart: %#v", reloaded.Providers)
	}
}

func TestDocumentMigrationReservesOriginalAndGeneratedIDs(t *testing.T) {
	raw := []byte(`{"providers":[{"name":"vendor","base_url":"https://up.invalid/v1","model":"chat"},{"name":"vendor","base_url":"https://up.invalid/v1","model":"message","messages_only":true},{"name":"vendor-2","base_url":"https://other.invalid/v1","model":"other"},{"name":"vendor","base_url":"https://up.invalid/v1","model":"response","responses_only":true},{"name":"vendor-3","base_url":"https://third.invalid/v1","model":"third"}]}`)
	doc, err := DecodeDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range doc.Providers {
		ids = append(ids, p.ID)
	}
	if want := []string{"vendor", "vendor-4", "vendor-2", "vendor-5", "vendor-3"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("migration IDs = %v, want %v", ids, want)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := WriteDocument(path, doc); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Providers, doc.Providers) {
		t.Fatal("migration IDs/routes changed on restart")
	}
}

func TestMergeLegacyRejectsAmbiguousProviderEdit(t *testing.T) {
	first, second := persistenceProvider("first"), persistenceProvider("second")
	first.Name, second.Name = "vendor", "vendor"
	first.Models, second.Models = first.Models[:1], second.Models[:1]
	second.Models[0] = first.Models[0]
	current := []Provider{first, second}
	rows := LegacyView(current)
	rows[0].BaseURL = "https://changed.invalid/v1"
	if _, err := MergeLegacy(current, rows); err == nil {
		t.Fatal("an edit with indistinguishable original owners was accepted")
	}
}

func TestMergeLegacySplitsEndpointEditWithoutLosingV2Settings(t *testing.T) {
	original := persistenceProvider("stable")
	original.Models = []ModelMapping{{Alias: "first", UpstreamModel: "first"}, {Alias: "second", UpstreamModel: "second"}, {Alias: "hidden", UpstreamModel: "private", Hidden: true}}
	rows := LegacyView([]Provider{original})
	rows[1].BaseURL = "https://changed.invalid/v1"
	got, err := MergeLegacy([]Provider{original}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != original.ID || got[1].ID == original.ID {
		t.Fatalf("split identities = %#v", got)
	}
	for _, p := range got {
		if !reflect.DeepEqual(p.Headers, original.Headers) || p.Auth != original.Auth || p.Timeouts != original.Timeouts || p.TLS != original.TLS {
			t.Fatalf("split lost settings: %#v", p)
		}
	}
	if !got[0].Models[1].Hidden || got[1].BaseURL != rows[1].BaseURL || got[1].Models[0].UpstreamModel != "second" {
		t.Fatalf("split lost mappings: %#v", got)
	}
}

func TestMergeLegacyReservesRetainedIDsAndNewNames(t *testing.T) {
	original := persistenceProvider("vendor-2")
	original.Name = "vendor"
	original.Models = original.Models[:1]
	rows := append(LegacyView([]Provider{original}),
		LegacyProvider{Name: "vendor", BaseURL: "https://new.invalid/v1", Model: "new", Alias: "new"},
		LegacyProvider{Name: "vendor-3", BaseURL: "https://third.invalid/v1", Model: "third", Alias: "third"},
		LegacyProvider{Name: "vendor", BaseURL: "https://message.invalid/v1", Model: "message", Alias: "message", MessagesOnly: true})
	got, err := MergeLegacy([]Provider{original}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if want := []string{"vendor-2", "vendor", "vendor-3", "vendor-4"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("admin migration IDs = %v, want %v", ids, want)
	}
}
