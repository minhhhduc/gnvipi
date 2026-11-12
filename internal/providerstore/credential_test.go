package providerstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sealedKey(t *testing.T, scheme, plaintext string) (KeyRow, *Keyring) {
	t.Helper()
	ring := testKeyring(t)
	key := KeyRow{
		ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key",
		Enabled: true, AuthScheme: scheme,
	}
	if scheme == SchemeHeader {
		key.AuthHeaderName = "X-Custom-Key"
	}
	key = sealKey(t, ring, key, []byte(plaintext))
	return key, ring
}

func TestResolveKeyCredentialSourceMatrix(t *testing.T) {
	ctx := context.Background()
	ring := testKeyring(t)

	t.Run("none has no credential", func(t *testing.T) {
		got, err := ResolveKeyCredential(ctx, KeyRow{ID: "k", AuthScheme: SchemeNone, Enabled: true}, ring, nil)
		if err != nil || got.Raw != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("bearer encrypted secret", func(t *testing.T) {
		key, ring := sealedKey(t, SchemeBearer, "sk-secret")
		got, err := ResolveKeyCredential(ctx, key, ring, nil)
		if err != nil || got.Raw != "sk-secret" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("header environment credential", func(t *testing.T) {
		t.Setenv("GNVIPI_KEY_TEST_ENV", "env-secret")
		key := KeyRow{ID: "k", ProviderID: "p", OwnerID: "u", Enabled: true, AuthScheme: SchemeHeader, AuthHeaderName: "X-Key", KeyEnv: "GNVIPI_KEY_TEST_ENV"}
		got, err := ResolveKeyCredential(ctx, key, nil, nil)
		if err != nil || got.Raw != "env-secret" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("missing environment variable is unavailable", func(t *testing.T) {
		key := KeyRow{ID: "k", ProviderID: "p", OwnerID: "u", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "GNVIPI_KEY_DEFINITELY_UNSET"}
		if _, err := ResolveKeyCredential(ctx, key, nil, nil); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("reference without resolver is unsupported", func(t *testing.T) {
		key := KeyRow{ID: "k", ProviderID: "p", OwnerID: "u", Enabled: true, AuthScheme: SchemeBearer, KeyRef: "import:foo"}
		if _, err := ResolveKeyCredential(ctx, key, nil, nil); !errors.Is(err, ErrUnsupportedReference) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unresolved reference is unavailable", func(t *testing.T) {
		key := KeyRow{ID: "k", ProviderID: "p", OwnerID: "u", Enabled: true, AuthScheme: SchemeBearer, KeyRef: "import:missing"}
		if _, err := ResolveKeyCredential(ctx, key, nil, MapRefResolver{}); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("resolved reference", func(t *testing.T) {
		key := KeyRow{ID: "k", ProviderID: "p", OwnerID: "u", Enabled: true, AuthScheme: SchemeBearer, KeyRef: "import:foo"}
		got, err := ResolveKeyCredential(ctx, key, nil, MapRefResolver{"import:foo": "ref-secret"})
		if err != nil || got.Raw != "ref-secret" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("multiple sources rejected", func(t *testing.T) {
		key, ring := sealedKey(t, SchemeBearer, "secret")
		key.KeyEnv = "GNVIPI_KEY_TEST_ENV"
		t.Setenv("GNVIPI_KEY_TEST_ENV", "other")
		if _, err := ResolveKeyCredential(ctx, key, ring, nil); !errors.Is(err, ErrInvalidCredentialSource) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("none with credential rejected", func(t *testing.T) {
		key := KeyRow{ID: "k", AuthScheme: SchemeNone, Enabled: true, KeyEnv: "GNVIPI_KEY_TEST_ENV"}
		if _, err := ResolveKeyCredential(ctx, key, nil, nil); !errors.Is(err, ErrInvalidCredentialSource) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("non-none without source rejected", func(t *testing.T) {
		key := KeyRow{ID: "k", AuthScheme: SchemeBearer, Enabled: true}
		if _, err := ResolveKeyCredential(ctx, key, nil, nil); !errors.Is(err, ErrInvalidCredentialSource) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("decrypt failure does not fall back to env", func(t *testing.T) {
		key, ring := sealedKey(t, SchemeBearer, "original")
		t.Setenv("GNVIPI_KEY_TEST_ENV", "fallback")
		key.KeyEnv = ""
		key.SecretEnc = append([]byte(nil), key.SecretEnc...)
		key.SecretEnc[0] ^= 0xff
		if _, err := ResolveKeyCredential(ctx, key, ring, MapRefResolver{}); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("secret and nil keyring", func(t *testing.T) {
		key, _ := sealedKey(t, SchemeBearer, "original")
		if _, err := ResolveKeyCredential(ctx, key, nil, nil); !errors.Is(err, ErrMissingMasterKey) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("basic secret parses username and password", func(t *testing.T) {
		key, ring := sealedKey(t, SchemeBasic, `{"username":"alice","password":"p@ss"}`)
		got, err := ResolveKeyCredential(ctx, key, ring, nil)
		if err != nil || got.Basic.Username != "alice" || got.Basic.Password != "p@ss" || got.Raw != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("malformed basic secret rejected", func(t *testing.T) {
		key, ring := sealedKey(t, SchemeBasic, "not-json")
		if _, err := ResolveKeyCredential(ctx, key, ring, nil); !errors.Is(err, ErrInvalidCredentialSource) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResolveKeyCredentialErrorsNeverLeakSecret(t *testing.T) {
	key, _ := sealedKey(t, SchemeBearer, "super-secret-value")
	_, err := ResolveKeyCredential(context.Background(), key, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error leaks secret: %v", err)
	}
}

func TestApplyKeyPatchSecretUpdate(t *testing.T) {
	ring := testKeyring(t)
	cur := KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key", Enabled: true, AuthScheme: SchemeBearer}

	next, err := ApplyKeyPatch(cur, KeyPatch{Secret: []byte("new-secret")}, ring)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !next.HasSecret() || next.SecretKVer != ring.CurrentVersion() || next.SecretLast4 != "cret" {
		t.Fatalf("unexpected key: %+v", next)
	}
	got, err := ResolveKeyCredential(context.Background(), next, ring, nil)
	if err != nil || got.Raw != "new-secret" {
		t.Fatalf("resolve = %+v, %v", got, err)
	}

	if _, err := ApplyKeyPatch(cur, KeyPatch{Secret: []byte("x"), KeyEnv: ptr("ENV_NAME")}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("conflicting sources error = %v", err)
	}
	if _, err := ApplyKeyPatch(cur, KeyPatch{Secret: []byte("x")}, nil); !errors.Is(err, ErrMissingMasterKey) {
		t.Fatalf("nil keyring error = %v", err)
	}
}

func TestApplyKeyPatchClearSecret(t *testing.T) {
	ring := testKeyring(t)
	sealed, _ := sealedKey(t, SchemeBearer, "secret")
	sealed.Enabled = true

	if _, err := ApplyKeyPatch(sealed, KeyPatch{ClearSecret: true}, ring); !errors.Is(err, ErrClearSecretNotAllowed) {
		t.Fatalf("enabled clear error = %v", err)
	}
	disabled := sealed
	disabled.Enabled = false
	next, err := ApplyKeyPatch(disabled, KeyPatch{ClearSecret: true}, ring)
	if err != nil {
		t.Fatalf("disabled clear: %v", err)
	}
	if next.HasSecret() || next.SecretNonce != nil || next.SecretKVer != 0 || next.SecretLast4 != "" {
		t.Fatalf("secret not cleared: %+v", next)
	}
	replaced, err := ApplyKeyPatch(sealed, KeyPatch{ClearSecret: true, KeyEnv: ptr("GNVIPI_KEY_TEST_ENV")}, ring)
	if err != nil {
		t.Fatalf("clear with replacement: %v", err)
	}
	if replaced.HasSecret() || replaced.KeyEnv != "GNVIPI_KEY_TEST_ENV" {
		t.Fatalf("unexpected replacement: %+v", replaced)
	}
}

func TestApplyKeyPatchSchemeTransitions(t *testing.T) {
	ring := testKeyring(t)
	cur, _ := sealedKey(t, SchemeBearer, "secret")

	// Switching to none clears every credential and header field.
	next, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeNone)}, ring)
	if err != nil {
		t.Fatalf("to none: %v", err)
	}
	if next.AuthScheme != SchemeNone || next.HasSecret() || next.KeyEnv != "" || next.KeyRef != "" || next.AuthHeaderName != "" {
		t.Fatalf("unexpected none transition: %+v", next)
	}

	// Header requires a name.
	if _, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeHeader)}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("header without name error = %v", err)
	}
	// An existing ciphertext is bound to the bearer AAD, so changing the
	// scheme without re-entering the secret must be rejected.
	if _, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeHeader), AuthHeaderName: ptr("X-Custom")}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("scheme change without secret error = %v", err)
	}

	// Supplying both a new scheme and a new secret seals for the final scheme.
	next, err = ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeHeader), AuthHeaderName: ptr("X-Custom"), Secret: []byte("hdr-secret")}, ring)
	if err != nil {
		t.Fatalf("to header with secret: %v", err)
	}
	if next.AuthHeaderName != "X-Custom" {
		t.Fatalf("header name = %q", next.AuthHeaderName)
	}
	got, err := ResolveKeyCredential(context.Background(), next, ring, nil)
	if err != nil || got.Raw != "hdr-secret" {
		t.Fatalf("resolve header = %+v, %v", got, err)
	}
}

func TestApplyKeyPatchValidatesBasicPlaintext(t *testing.T) {
	ring := testKeyring(t)
	cur, _ := sealedKey(t, SchemeBearer, "secret")

	if _, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeBasic), Secret: []byte("not-json")}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("invalid basic plaintext error = %v", err)
	}
	if _, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeBasic), Secret: []byte(`{"username":"","password":"x"}`)}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("empty basic username error = %v", err)
	}
	next, err := ApplyKeyPatch(cur, KeyPatch{AuthScheme: ptr(SchemeBasic), Secret: []byte(`{"username":"alice","password":"p@ss"}`)}, ring)
	if err != nil {
		t.Fatalf("valid basic patch: %v", err)
	}
	got, err := ResolveKeyCredential(context.Background(), next, ring, nil)
	if err != nil || got.Basic.Username != "alice" || got.Basic.Password != "p@ss" {
		t.Fatalf("resolve basic = %+v, %v", got, err)
	}
}

func TestApplyKeyPatchEnvAndRefExclusive(t *testing.T) {
	ring := testKeyring(t)
	cur, _ := sealedKey(t, SchemeBearer, "secret")

	next, err := ApplyKeyPatch(cur, KeyPatch{KeyEnv: ptr("GNVIPI_KEY_TEST_ENV")}, ring)
	if err != nil {
		t.Fatalf("env patch: %v", err)
	}
	if next.HasSecret() || next.KeyEnv != "GNVIPI_KEY_TEST_ENV" || next.KeyRef != "" {
		t.Fatalf("unexpected env patch: %+v", next)
	}
	if _, err := ApplyKeyPatch(cur, KeyPatch{KeyRef: ptr("import:foo")}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("unprivileged ref patch error = %v, want ErrInvalidCredentialSource", err)
	}
	next, err = ApplyKeyPatch(cur, KeyPatch{KeyRef: ptr("import:foo"), AllowKeyRef: true}, ring)
	if err != nil {
		t.Fatalf("privileged ref patch: %v", err)
	}
	if next.HasSecret() || next.KeyRef != "import:foo" || next.KeyEnv != "" {
		t.Fatalf("unexpected privileged ref patch: %+v", next)
	}
	if _, err := ApplyKeyPatch(cur, KeyPatch{KeyEnv: ptr("A"), KeyRef: ptr("B")}, ring); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("env+ref error = %v", err)
	}
}

func ptr[T any](value T) *T { return &value }

func TestKeyEnvAllowlist(t *testing.T) {
	for _, name := range []string{
		"GNVIPI_MASTER_KEY",
		"GNVIPI_MASTER_KEY_VERSION",
		"GNVIPI_DATABASE_URL",
		"GNVIPI_MIGRATION_DATABASE_URL",
		"GNVIPI_MIGRATION_CUSTOM",
		"GNVIPI_PASSWORD_PEPPER",
		"GNVIPI_ADMIN_TOKEN",
		"OTHER_KEY",
		"GNVIPI_KEY_",
		"GNVIPI_KEY_BAD-DASH",
	} {
		if err := validateKeyEnvName(name); err == nil {
			t.Errorf("validateKeyEnvName(%q) accepted reserved/invalid name", name)
		}
	}
	if err := validateKeyEnvName("GNVIPI_KEY_OPENROUTER"); err != nil {
		t.Fatalf("valid key env rejected: %v", err)
	}
	t.Setenv("GNVIPI_KEY_OPENROUTER", "sk-test")
	key := KeyRow{ID: "k1", ProviderID: "p1", OwnerID: "u1", Enabled: true, AuthScheme: SchemeBearer, KeyEnv: "GNVIPI_KEY_OPENROUTER"}
	got, err := ResolveKeyCredential(context.Background(), key, nil, nil)
	if err != nil || got.Raw != "sk-test" {
		t.Fatalf("resolve allowlisted env = %+v, %v", got, err)
	}
	key.KeyEnv = "GNVIPI_MASTER_KEY"
	if _, err := ResolveKeyCredential(context.Background(), key, nil, nil); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("reserved env error = %v, want ErrInvalidCredentialSource", err)
	}
	if _, err := ApplyKeyPatch(key, KeyPatch{KeyEnv: &key.KeyEnv}, nil); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("reserved env patch error = %v", err)
	}
}
