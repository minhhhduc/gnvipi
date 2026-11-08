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
