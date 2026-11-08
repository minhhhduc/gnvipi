package provider

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestMaskSecretAndIsRedactedKey(t *testing.T) {
	cases := []struct {
		input      string
		wantMask   string
		isRedacted bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"[REDACTED]", "[REDACTED]", true},
		{"unchanged", "[REDACTED]", true},
		{"UNCHANGED", "[REDACTED]", true},
		{"sk-...abcd", "sk-...abcd", true},
		{"sk-12345678", "sk-...5678", false},
		{"sk-long-secret-key-123456", "sk-...3456", false},
		{"api-key-value-9999", "api...9999", false},
		{"short", "[REDACTED]", false},
	}

	for _, c := range cases {
		masked := MaskSecret(c.input)
		if masked != c.wantMask {
			t.Errorf("MaskSecret(%q) = %q, want %q", c.input, masked, c.wantMask)
		}
		redacted := IsRedactedKey(c.input)
		if redacted != c.isRedacted {
			t.Errorf("IsRedactedKey(%q) = %v, want %v", c.input, redacted, c.isRedacted)
		}
	}
}

func TestCheckPOSIXPermissions(t *testing.T) {
	if err := checkPOSIXPermissions(0o600); err != nil {
		t.Fatalf("expected 0600 to be valid, got: %v", err)
	}
	if err := checkPOSIXPermissions(0o400); err != nil {
		t.Fatalf("expected 0400 to be valid, got: %v", err)
	}
	if err := checkPOSIXPermissions(0o644); err == nil {
		t.Fatal("expected 0644 to fail checkPOSIXPermissions")
	}
	if err := checkPOSIXPermissions(0o777); err == nil {
		t.Fatal("expected 0777 to fail checkPOSIXPermissions")
	}
}

func TestLoadAndWriteSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")

	// Missing file returns empty map
	secrets, err := LoadSecrets(path)
	if err != nil || len(secrets) != 0 {
		t.Fatalf("LoadSecrets on missing file = %v, %v", secrets, err)
	}

	// Write secrets
	data := map[string]string{
		"prov1": "sk-secret-1",
		"prov2": "sk-secret-2",
	}
	if err := WriteSecrets(path, data); err != nil {
		t.Fatalf("WriteSecrets failed: %v", err)
	}

	// Reload
	reloaded, err := LoadSecrets(path)
	if err != nil {
		t.Fatalf("LoadSecrets failed: %v", err)
	}
	if reloaded["prov1"] != "sk-secret-1" || reloaded["prov2"] != "sk-secret-2" {
		t.Fatalf("reloaded secrets mismatch: %#v", reloaded)
	}

	// Nested format compatibility
	nestedPath := filepath.Join(dir, "nested_secrets.json")
	nestedRaw := []byte(`{"secrets": {"p1": "key-p1"}, "providers": {"p2": {"key": "key-p2"}}}`)
	if err := os.WriteFile(nestedPath, nestedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	nestedMap, err := LoadSecrets(nestedPath)
	if err != nil {
		t.Fatalf("LoadSecrets nested failed: %v", err)
	}
	if nestedMap["p1"] != "key-p1" || nestedMap["p2"] != "key-p2" {
		t.Fatalf("nestedMap mismatch: %#v", nestedMap)
	}

	// Nested format with direct string provider values: {"providers": {"p1": "secret1"}}
	nestedDirectPath := filepath.Join(dir, "nested_direct.json")
	if err := os.WriteFile(nestedDirectPath, []byte(`{"providers": {"p1": "secret1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nestedDirectMap, err := LoadSecrets(nestedDirectPath)
	if err != nil {
		t.Fatalf("LoadSecrets nested direct failed: %v", err)
	}
	if len(nestedDirectMap) == 0 || nestedDirectMap["p1"] != "secret1" {
		t.Fatalf("expected nestedDirectMap to contain p1: secret1 without returning empty map, got: %#v", nestedDirectMap)
	}
}

func TestLegacyInlineKeysMigratedOnFirstSave(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	secretsPath := filepath.Join(dir, "secrets.json")

	// Create legacy models document with inline key
	inlineDoc := Document{
		Providers: []Provider{
			{
				ID:       "legacy-prov",
				Name:     "Legacy Provider",
				BaseURL:  "https://api.legacy.example/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     Auth{Scheme: AuthBearer, Key: "sk-legacy-secret-12345"},
				Models:   []ModelMapping{{Alias: "m", UpstreamModel: "m"}},
			},
		},
	}

	rawModels, err := json.Marshal(inlineDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, rawModels, 0o600); err != nil {
		t.Fatal(err)
	}

	// Load document (secrets.json does not exist yet)
	loaded, err := LoadDocument(modelsPath)
	if err != nil {
		t.Fatalf("LoadDocument failed: %v", err)
	}
	if loaded.Providers[0].Auth.Key != "sk-legacy-secret-12345" {
		t.Fatalf("loaded key mismatch: %q", loaded.Providers[0].Auth.Key)
	}

	// First save: WriteDocument
	if err := WriteDocument(modelsPath, loaded); err != nil {
		t.Fatalf("WriteDocument failed: %v", err)
	}

	// Check models.json on disk: must NOT contain the secret!
	diskModels, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(diskModels, []byte("sk-legacy-secret-12345")) {
		t.Fatalf("catalog models.json leaked secret key on disk: %s", string(diskModels))
	}
	if !bytes.Contains(diskModels, []byte("key_ref")) {
		t.Fatalf("catalog models.json should contain key_ref: %s", string(diskModels))
	}

	// Check secrets.json on disk: must contain the secret!
	diskSecrets, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("secrets.json missing on disk: %v", err)
	}
	if !bytes.Contains(diskSecrets, []byte("sk-legacy-secret-12345")) {
		t.Fatalf("secrets.json does not contain secret: %s", string(diskSecrets))
	}

	// Re-load models.json: key is resolved from secrets.json!
	reloaded, err := LoadDocument(modelsPath)
	if err != nil {
		t.Fatalf("reloaded LoadDocument failed: %v", err)
	}
	if reloaded.Providers[0].Auth.Key != "sk-legacy-secret-12345" {
		t.Fatalf("resolved key mismatch: %q", reloaded.Providers[0].Auth.Key)
	}
}

func TestStoreUpsertPreservesRedactedSentinels(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	store, err := OpenStore(modelsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	prov := Provider{
		ID:       "prov-sentinel",
		Name:     "Sentinel Prov",
		BaseURL:  "https://api.sentinel.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     Auth{Scheme: AuthBearer, Key: "sk-original-secret-9999"},
		Models:   []ModelMapping{{Alias: "m", UpstreamModel: "m"}},
	}
	if err := store.Upsert(&prov); err != nil {
		t.Fatalf("store.Upsert initial failed: %v", err)
	}

	// Check that secrets were written
	secrets, err := LoadSecrets(DefaultSecretsPath(modelsPath))
	if err != nil || secrets["prov-sentinel"] != "sk-original-secret-9999" {
		t.Fatalf("initial secret mismatch: %#v, err: %v", secrets, err)
	}

	// Update with [REDACTED]
	updated := prov
	updated.Auth.Key = "[REDACTED]"
	if err := store.Upsert(&updated); err != nil {
		t.Fatalf("store.Upsert [REDACTED] failed: %v", err)
	}

	got, found := store.Get("prov-sentinel")
	if !found || got.Auth.Key != "sk-original-secret-9999" {
		t.Fatalf("secret overwritten with [REDACTED]: got %q", got.Auth.Key)
	}

	// Update with "unchanged"
	updated.Auth.Key = "unchanged"
	if err := store.Upsert(&updated); err != nil {
		t.Fatalf("store.Upsert unchanged failed: %v", err)
	}
	got, found = store.Get("prov-sentinel")
	if !found || got.Auth.Key != "sk-original-secret-9999" {
		t.Fatalf("secret overwritten with unchanged: got %q", got.Auth.Key)
	}

	// Update with masked hint "sk-...9999"
	updated.Auth.Key = "sk-...9999"
	if err := store.Upsert(&updated); err != nil {
		t.Fatalf("store.Upsert hint failed: %v", err)
	}
	got, found = store.Get("prov-sentinel")
	if !found || got.Auth.Key != "sk-original-secret-9999" {
		t.Fatalf("secret overwritten with hint: got %q", got.Auth.Key)
	}

	// Update with new real key
	updated.Auth.Key = "sk-brand-new-key-1111"
	if err := store.Upsert(&updated); err != nil {
		t.Fatalf("store.Upsert new key failed: %v", err)
	}
	got, found = store.Get("prov-sentinel")
	if !found || got.Auth.Key != "sk-brand-new-key-1111" {
		t.Fatalf("secret not updated with new key: got %q", got.Auth.Key)
	}
}

func TestAuthUnmarshalJSONSupportsAPIKeyEnv(t *testing.T) {
	raw := []byte(`{"scheme": "bearer", "api_key_env": "MY_API_KEY_ENV"}`)
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if a.Scheme != AuthBearer || a.KeyEnv != "MY_API_KEY_ENV" {
		t.Fatalf("Auth unmarshal api_key_env failed: %#v", a)
	}

	// Legacy provider unmarshal
	rawLegacy := []byte(`{"name": "legacy", "base_url": "https://api.example/v1", "api_key_env": "LEGACY_KEY_ENV", "model": "m"}`)
	var lp LegacyProvider
	if err := json.Unmarshal(rawLegacy, &lp); err != nil {
		t.Fatal(err)
	}
	converted, err := FromLegacy([]LegacyProvider{lp})
	if err != nil {
		t.Fatal(err)
	}
	if len(converted) != 1 || converted[0].Auth.KeyEnv != "LEGACY_KEY_ENV" {
		t.Fatalf("FromLegacy with api_key_env failed: %#v", converted)
	}
}

func TestCheckSecretsFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Missing file is ok
	if err := CheckSecretsFilePermissions(filepath.Join(dir, "nonexistent.json")); err != nil {
		t.Fatalf("expected nil for nonexistent file: %v", err)
	}
	// Existing file
	if err := CheckSecretsFilePermissions(path); err != nil {
		t.Fatalf("CheckSecretsFilePermissions failed: %v", err)
	}
	// Permissions checker helper
	if err := checkPOSIXPermissions(fs.FileMode(0o644)); err == nil {
		t.Fatal("expected error for 0644")
	}
}

func TestStoreUpsertSwitchToAuthNoneOrKeyEnv(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	store, err := OpenStore(modelsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rawSecret := "sk-upsert-switch-test-secret"
	prov := Provider{
		ID:       "prov-switch-auth",
		Name:     "Switch Auth Prov",
		BaseURL:  "https://api.switchauth.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     Auth{Scheme: AuthBearer, Key: rawSecret},
		Models:   []ModelMapping{{Alias: "m", UpstreamModel: "m"}},
	}
	if err := store.Upsert(&prov); err != nil {
		t.Fatalf("initial upsert failed: %v", err)
	}

	got, found := store.Get("prov-switch-auth")
	if !found || got.Auth.Key != rawSecret {
		t.Fatalf("initial secret mismatch: got %q, want %q", got.Auth.Key, rawSecret)
	}

	// Switch to AuthNone
	noneProv := prov
	noneProv.Auth = Auth{Scheme: AuthNone}
	if err := store.Upsert(&noneProv); err != nil {
		t.Fatalf("upsert AuthNone failed: %v", err)
	}
	got, found = store.Get("prov-switch-auth")
	if !found || got.Auth.Scheme != AuthNone || got.Auth.Key != "" || got.Auth.KeyRef != "" {
		t.Fatalf("AuthNone not preserved: %#v", got.Auth)
	}

	// Restore to Bearer
	bearerProv := prov
	bearerProv.Auth = Auth{Scheme: AuthBearer, Key: "sk-fresh-secret-5555"}
	if err := store.Upsert(&bearerProv); err != nil {
		t.Fatalf("upsert bearer failed: %v", err)
	}

	// Switch to KeyEnv
	keyEnvProv := prov
	keyEnvProv.Auth = Auth{Scheme: AuthBearer, KeyEnv: "MY_STORE_KEY_ENV"}
	if err := store.Upsert(&keyEnvProv); err != nil {
		t.Fatalf("upsert KeyEnv failed: %v", err)
	}
	got, found = store.Get("prov-switch-auth")
	if !found || got.Auth.KeyEnv != "MY_STORE_KEY_ENV" || got.Auth.Key != "" || got.Auth.KeyRef != "" {
		t.Fatalf("KeyEnv not preserved: %#v", got.Auth)
	}
}

func TestStoreUpdatePreservesResolvedSecretInMemory(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")

	store, err := OpenStore(modelsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rawSecret := "sk-persistent-in-memory-secret-123"
	prov := Provider{
		ID:       "prov-in-memory-test",
		Name:     "In Memory Prov",
		BaseURL:  "https://api.inmem.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     Auth{Scheme: AuthBearer, Key: rawSecret},
		Models:   []ModelMapping{{Alias: "m", UpstreamModel: "m"}},
	}
	if err := store.Upsert(&prov); err != nil {
		t.Fatal(err)
	}

	got, found := store.Get("prov-in-memory-test")
	if !found || got.Auth.Key != rawSecret {
		t.Fatalf("initial secret mismatch: got %q, want %q", got.Auth.Key, rawSecret)
	}

	// Update metadata only
	err = store.Update(func(doc *Document) error {
		for i := range doc.Providers {
			if doc.Providers[i].ID == "prov-in-memory-test" {
				doc.Providers[i].Name = "Renamed In Memory Prov"
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("metadata store.Update failed: %v", err)
	}

	got, found = store.Get("prov-in-memory-test")
	if !found || got.Auth.Key != rawSecret {
		t.Fatalf("secret lost after metadata update: got %q, want %q", got.Auth.Key, rawSecret)
	}
	snap := store.Snapshot()
	if len(snap.Providers) != 1 || snap.Providers[0].Auth.Key != rawSecret {
		t.Fatalf("snapshot secret lost: got %q, want %q", snap.Providers[0].Auth.Key, rawSecret)
	}

	// Update where candidate has redacted sentinel key
	err = store.Update(func(doc *Document) error {
		for i := range doc.Providers {
			if doc.Providers[i].ID == "prov-in-memory-test" {
				doc.Providers[i].Auth.Key = "[REDACTED]"
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("redacted store.Update failed: %v", err)
	}

	got, found = store.Get("prov-in-memory-test")
	if !found || got.Auth.Key != rawSecret {
		t.Fatalf("secret not repopulated in in-memory doc: got %q, want %q", got.Auth.Key, rawSecret)
	}
}

func TestMergeLegacySwitchToKeyEnvOrAuthNone(t *testing.T) {
	current := []Provider{{
		ID:       "prov-1",
		Name:     "ProvOne",
		BaseURL:  "https://api.one.example/v1",
		Protocol: ir.ProtocolOpenAIChat,
		Auth:     Auth{Scheme: AuthBearer, Key: "sk-original-secret", KeyRef: "prov-1"},
		Models:   []ModelMapping{{Alias: "one/m", UpstreamModel: "m", LegacyPosition: 1}},
		Enabled:  true,
	}}

	// 1. Switch to AuthNone by clearing key
	legacyNone := []LegacyProvider{{
		Name:    "ProvOne",
		BaseURL: "https://api.one.example/v1",
		APIKey:  "",
		Model:   "m",
		Alias:   "one/m",
	}}
	merged, err := MergeLegacy(current, legacyNone)
	if err != nil {
		t.Fatalf("MergeLegacy to AuthNone failed: %v", err)
	}
	if len(merged) != 1 || merged[0].Auth.Scheme != AuthNone || merged[0].Auth.Key != "" || merged[0].Auth.KeyRef != "" {
		t.Fatalf("expected AuthNone without key/keyref, got: %#v", merged[0].Auth)
	}

	// 2. Switch to KeyEnv
	legacyKeyEnv := []LegacyProvider{{
		Name:    "ProvOne",
		BaseURL: "https://api.one.example/v1",
		KeyEnv:  "NEW_PROVIDER_KEY_ENV",
		Model:   "m",
		Alias:   "one/m",
	}}
	merged, err = MergeLegacy(current, legacyKeyEnv)
	if err != nil {
		t.Fatalf("MergeLegacy to KeyEnv failed: %v", err)
	}
	if len(merged) != 1 || merged[0].Auth.KeyEnv != "NEW_PROVIDER_KEY_ENV" || merged[0].Auth.Key != "" || merged[0].Auth.KeyRef != "" {
		t.Fatalf("expected KeyEnv without key/keyref, got: %#v", merged[0].Auth)
	}

	// 3. Retain key when using redacted hint
	legacyRedacted := []LegacyProvider{{
		Name:    "ProvOne",
		BaseURL: "https://api.one.example/v1",
		APIKey:  "sk-...secret",
		Model:   "m",
		Alias:   "one/m",
	}}
	merged, err = MergeLegacy(current, legacyRedacted)
	if err != nil {
		t.Fatalf("MergeLegacy with redacted key failed: %v", err)
	}
	if len(merged) != 1 || merged[0].Auth.Key != "sk-original-secret" {
		t.Fatalf("expected retained key, got: %#v", merged[0].Auth)
	}
}

func TestWriteDocumentWithSecretsPreservesUnreferencedSecrets(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	secretsPath := filepath.Join(dir, "secrets.json")

	// Pre-seed secrets.json with provider B's secret
	initialSecrets := map[string]string{
		"prov-b": "sk-secret-b",
	}
	if err := WriteSecrets(secretsPath, initialSecrets); err != nil {
		t.Fatalf("WriteSecrets failed: %v", err)
	}

	// Create document containing only provider A
	docA := Document{
		Providers: []Provider{
			{
				ID:       "prov-a",
				Name:     "Provider A",
				BaseURL:  "https://api.a.example/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     Auth{Scheme: AuthBearer, Key: "sk-secret-a", KeyRef: "prov-a"},
				Models:   []ModelMapping{{Alias: "a/m", UpstreamModel: "m"}},
			},
		},
	}

	// Save document containing only provider A
	if err := WriteDocumentWithSecrets(modelsPath, secretsPath, docA); err != nil {
		t.Fatalf("WriteDocumentWithSecrets failed: %v", err)
	}

	// Verify secrets file still contains prov-b's secret and prov-a's secret
	secrets, err := LoadSecrets(secretsPath)
	if err != nil {
		t.Fatalf("LoadSecrets failed: %v", err)
	}
	if secrets["prov-b"] != "sk-secret-b" {
		t.Fatalf("provider B secret was purged! secrets: %#v", secrets)
	}
	if secrets["prov-a"] != "sk-secret-a" {
		t.Fatalf("provider A secret was not saved! secrets: %#v", secrets)
	}

	// Verify docA in-memory was NOT mutated with plaintext or corrupted
	callerDoc := Document{
		Providers: []Provider{
			{
				ID:       "prov-a",
				Name:     "Provider A",
				BaseURL:  "https://api.a.example/v1",
				Protocol: ir.ProtocolOpenAIChat,
				Auth:     Auth{Scheme: AuthBearer, Key: "[REDACTED]", KeyRef: "prov-a"},
				Models:   []ModelMapping{{Alias: "a/m", UpstreamModel: "m"}},
			},
		},
	}
	if err := WriteDocumentWithSecrets(modelsPath, secretsPath, callerDoc); err != nil {
		t.Fatalf("WriteDocumentWithSecrets failed: %v", err)
	}
	if callerDoc.Providers[0].Auth.Key != "[REDACTED]" {
		t.Fatalf("caller's in-memory doc was mutated: got %q, want [REDACTED]", callerDoc.Providers[0].Auth.Key)
	}
}
