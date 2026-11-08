package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"gopkg.in/yaml.v3"
)

func TestModelPrefsV2SurvivesUnrelatedChangesAndLegacyAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := gatewayprovider.Provider{ID: "stable-id", Name: "vendor", BaseURL: "https://up.invalid/v1", Protocol: ir.ProtocolOpenAIChat,
		Auth: gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, KeyEnv: "MODEL_PREFS_V2_KEY"}, Enabled: true, TLS: gatewayprovider.TLSChrome,
		Headers: map[string]string{"X-Tenant": "keep"}, Models: []gatewayprovider.ModelMapping{{Alias: "alias", UpstreamModel: "model"}, {Alias: "private", UpstreamModel: "private", Hidden: true}}}
	t.Setenv("MODEL_PREFS_V2_KEY", "env-secret")
	doc := gatewayprovider.Document{Providers: []gatewayprovider.Provider{original}, Fields: map[string]json.RawMessage{
		"_meta": json.RawMessage(`{"revision":"keep"}`), "groups": json.RawMessage(`[]`),
		"masks_id": json.RawMessage(`["already-hidden"]`), "deleted_id": json.RawMessage(`["already-deleted"]`),
		"extension": json.RawMessage(`{"number":9007199254740993}`)}}
	if err := gatewayprovider.WriteDocument(path, doc); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	if err := p.setHidden([]string{"mask"}); err != nil {
		t.Fatal(err)
	}
	if err := p.addDeleted("removed"); err != nil {
		t.Fatal(err)
	}
	if err := p.setAutoModel("alias"); err != nil {
		t.Fatal(err)
	}
	if err := p.setSearchURL("https://example.invalid/?q={query}"); err != nil {
		t.Fatal(err)
	}
	if err := p.setCycle(cycleConfig{Models: []string{"alias"}}); err != nil {
		t.Fatal(err)
	}
	legacy := p.listProviders()
	if len(legacy) != 1 || legacy[0].APIKey != "env-secret" {
		t.Fatalf("legacy view = %#v", legacy)
	}
	legacy[0].BaseURL = "https://changed.invalid/v1"
	if err := p.setProviders(legacy); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	original.BaseURL = legacy[0].BaseURL
	original.Models[0].LegacyPosition = 1
	got, err := loaded.listProvidersV2()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []gatewayprovider.Provider{original}) {
		t.Fatalf("v2 fields lost: %#v", got)
	}
	if !loaded.isHidden("mask") || !loaded.isDeleted("already-deleted") || !loaded.isDeleted("removed") {
		t.Fatal("model masks/deletions lost")
	}
	persisted, err := gatewayprovider.LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"_meta", "groups", "extension"} {
		var a, b any
		if err := json.Unmarshal(doc.Fields[key], &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(persisted.Fields[key], &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s changed", key)
		}
	}
}

func TestMigratedProvidersKeepLegacyRuntimeLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{"masks_id":["vendor/hidden"],"deleted_id":["removed"],"providers":[{"name":"vendor","base_url":"https://up.invalid/v1","model":"chat"},{"name":"vendor","base_url":"https://up.invalid/v1","model":"message","alias":"messages","messages_only":true},{"name":"vendor","base_url":"https://up.invalid/v1","model":"response","alias":"responses","responses_only":true},{"name":"vendor","base_url":"https://up.invalid/v1","model":"hidden"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes := func(p *modelPrefs) {
		t.Helper()
		for _, id := range []string{"chat", "vendor/chat"} {
			got := lookupCustomProvider(p, id)
			if got == nil || got.Model != "chat" || got.MessagesOnly || got.ResponsesOnly {
				t.Fatalf("chat route %q = %#v", id, got)
			}
		}
		if got := lookupCustomProvider(p, "messages"); got == nil || !got.MessagesOnly || got.Model != "message" {
			t.Fatalf("messages route = %#v", got)
		}
		if got := lookupCustomProvider(p, "responses"); got == nil || !got.ResponsesOnly || got.Model != "response" {
			t.Fatalf("responses route = %#v", got)
		}
		if got := lookupCustomProvider(p, "vendor/hidden"); got != nil {
			t.Fatalf("masked route = %#v", got)
		}
	}
	assertRoutes(p)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes(loaded)
}

func TestModelPrefsFailedChangesNeverPublishOrNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	p := &modelPrefs{path: path}
	good := []customProvider{{Name: "vendor", BaseURL: "https://up.invalid/v1", Model: "m"}}
	if err := p.setProviders(good); err != nil {
		t.Fatal(err)
	}
	if err := p.setHidden([]string{"hidden"}); err != nil {
		t.Fatal(err)
	}
	if err := p.addDeleted("deleted"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.onChange = func([]customProvider) { calls++ }
	p.onMaskChange = func() { calls++ }
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "old"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func() error{
		func() error { return p.setHidden([]string{"changed"}) },
		func() error { return p.addDeleted("new") },
		func() error { return p.removeDeleted("deleted") },
		func() error { return p.setAutoModel("changed") },
		func() error { return p.setSearchURL("changed") },
		func() error { return p.setCycle(cycleConfig{Models: []string{"vendor/m"}}) },
		func() error {
			return p.setProviders([]customProvider{{Name: "vendor", BaseURL: "https://changed.invalid/v1", Model: "m"}})
		},
	} {
		if err := mutate(); err == nil {
			t.Fatal("failed write was accepted")
		}
	}
	if !p.isHidden("hidden") || p.isHidden("changed") || !p.isDeleted("deleted") || p.isDeleted("new") || p.searchTemplate() != "" || p.listProviders()[0].BaseURL != good[0].BaseURL || calls != 0 {
		t.Fatalf("failed candidate published, calls %d", calls)
	}
	if cfg, _ := p.cycleSnapshot(); len(cfg.Models) != 0 {
		t.Fatal("failed cycle published")
	}
}

func TestModelPrefsConcurrentV2UpdatesKeepMasksAndCallbackOrder(t *testing.T) {
	p := &modelPrefs{path: filepath.Join(t.TempDir(), "models.json")}
	if err := p.setHidden([]string{"mask"}); err != nil {
		t.Fatal(err)
	}
	var counts []int
	p.onChange = func(list []customProvider) { counts = append(counts, len(list)) }
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two", "three", "four"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := p.upsertProviderV2(&gatewayprovider.Provider{ID: id, Name: id, BaseURL: "https://up.invalid/v1", Protocol: ir.ProtocolOpenAIChat, Enabled: true,
				Auth: gatewayprovider.Auth{Scheme: gatewayprovider.AuthNone}, Models: []gatewayprovider.ModelMapping{{Alias: id, UpstreamModel: id}}}); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if !reflect.DeepEqual(counts, []int{1, 2, 3, 4}) || !p.isHidden("mask") {
		t.Fatalf("ordering or masks lost: %v", counts)
	}
	if err := p.deleteProviderV2("one"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.getProviderV2("one"); err != nil || ok {
		t.Fatal("provider was not deleted")
	}
}

func TestV2YAMLIncludesOnlyEnabledChatProviders(t *testing.T) {
	var list []gatewayprovider.Provider
	for _, protocol := range []ir.Protocol{ir.ProtocolOpenAIChat, ir.ProtocolAnthropicMessages, ir.ProtocolOpenAIResponses} {
		list = append(list, gatewayprovider.Provider{ID: protocol.String(), Name: protocol.String(), Protocol: protocol, Enabled: true, BaseURL: "https://up.invalid/v1",
			Models: []gatewayprovider.ModelMapping{{Alias: protocol.String(), UpstreamModel: "m"}, {Alias: "hidden", UpstreamModel: "private", Hidden: true}}})
	}
	disabled := list[0]
	disabled.ID, disabled.Name, disabled.Enabled = "disabled", "disabled", false
	list = append(list, disabled)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := rewriteConfigYAML(path, compatProviders(list)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers []struct {
			Name   string `yaml:"name"`
			Models []struct {
				Name string `yaml:"name"`
			} `yaml:"models"`
		} `yaml:"openai-compatibility"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != 1 || doc.Providers[0].Name != ir.ProtocolOpenAIChat.String() || len(doc.Providers[0].Models) != 1 || doc.Providers[0].Models[0].Name != "m" {
		t.Fatalf("wrong YAML routes: %s", raw)
	}
	if got := gatewayprovider.LegacyView(list); len(got) != 3 || !got[1].MessagesOnly || !got[2].ResponsesOnly {
		t.Fatalf("local passthrough view = %#v", got)
	}
}

func TestModelPrefsDoesNotOverwriteUnsafeFiles(t *testing.T) {
	for _, raw := range []string{`{"schema_version":3,"providers_v2":[]}`, `{"groups":42,"providers":[]}`, `{"providers":`} {
		path := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		p := &modelPrefs{}
		if err := p.load(path); err == nil {
			t.Fatal("unsafe document accepted")
		}
		if err := p.setHidden([]string{"mask"}); err == nil {
			t.Fatal("unsafe file overwritten")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != raw {
			t.Fatalf("unsafe file changed: %v", err)
		}
	}
}

func TestMigratedProvidersPreserveInterleavedBareModelPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{"providers":[{"name":"a","base_url":"https://a.invalid/v1","model":"first"},{"name":"b","base_url":"https://b.invalid/v1","model":"shared"},{"name":"a","base_url":"https://a.invalid/v1","model":"shared"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes := func(p *modelPrefs) {
		t.Helper()
		for model, endpoint := range map[string]string{"first": "https://a.invalid/v1", "shared": "https://b.invalid/v1", "a/shared": "https://a.invalid/v1", "b/shared": "https://b.invalid/v1"} {
			if got := customProviderForModel(p, model); got == nil || got.BaseURL != endpoint {
				t.Fatalf("route %q = %#v, want %s", model, got, endpoint)
			}
		}
	}
	assertRoutes(p)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes(loaded)
	// Reordering the admin list must also survive grouping and persistence.
	rows := loaded.listProviders()
	rows[1], rows[2] = rows[2], rows[1]
	if err := loaded.setProviders(rows); err != nil {
		t.Fatal(err)
	}
	reordered := &modelPrefs{}
	if err := reordered.load(path); err != nil {
		t.Fatal(err)
	}
	if got := customProviderForModel(reordered, "shared"); got == nil || got.BaseURL != "https://a.invalid/v1" {
		t.Fatalf("reordered bare route = %#v", got)
	}
}

func TestMigratedDuplicateAliasesKeepBareModelRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{"providers":[{"name":"vendor","base_url":"https://up.invalid/v1","model":"first","alias":"shared"},{"name":"vendor","base_url":"https://up.invalid/v1","model":"second","alias":"shared"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes := func(p *modelPrefs) {
		t.Helper()
		for alias, upstream := range map[string]string{"shared": "first", "first": "first", "second": "second"} {
			if got := customProviderForModel(p, alias); got == nil || got.Model != upstream {
				t.Fatalf("route %q = %#v, want %q", alias, got, upstream)
			}
		}
	}
	assertRoutes(p)
	rows := append(p.listProviders(), customProvider{Name: "other", BaseURL: "https://other.invalid/v1", Model: "other"})
	if err := p.setProviders(rows); err != nil {
		t.Fatal(err)
	}
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	assertRoutes(loaded)
}

func TestMaskToggleSucceedsWithCollidingOrShadowingProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{
		"schema_version": 2,
		"providers_v2": [
			{
				"id": "p1",
				"name": "p1",
				"base_url": "https://p1.invalid/v1",
				"protocol": "openai-chat",
				"enabled": true,
				"models": [
					{"alias": "shared-alias", "upstream_model": "m1"}
				]
			},
			{
				"id": "p2",
				"name": "p2",
				"base_url": "https://p2.invalid/v1",
				"protocol": "openai-chat",
				"enabled": true,
				"models": [
					{"alias": "shared-alias", "upstream_model": "m2"},
					{"alias": "nvidia/nemotron-3.5-lightning-30b-a3b", "upstream_model": "m3"}
				]
			}
		]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &modelPrefs{}
	if err := p.load(path); err != nil {
		t.Fatalf("load should succeed with collision/shadow: %v", err)
	}

	// Toggling model masks must succeed even though existing loaded providers have collisions/shadows
	if err := p.setHidden([]string{"target-model"}); err != nil {
		t.Fatalf("setHidden failed: %v", err)
	}
	if !p.isHidden("target-model") {
		t.Fatalf("expected target-model to be hidden")
	}

	// Reload from disk and verify mask persisted
	loaded := &modelPrefs{}
	if err := loaded.load(path); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !loaded.isHidden("target-model") {
		t.Fatalf("reloaded mask was not preserved")
	}

	// Untoggling mask also succeeds
	if err := p.setHidden(nil); err != nil {
		t.Fatalf("unmasking failed: %v", err)
	}
	if p.isHidden("target-model") {
		t.Fatalf("expected target-model to be unhidden")
	}

	// Deleting model mask also succeeds
	if err := p.addDeleted("deleted-model"); err != nil {
		t.Fatalf("addDeleted failed: %v", err)
	}
	if !p.isDeleted("deleted-model") {
		t.Fatalf("expected deleted-model to be marked deleted")
	}
}
