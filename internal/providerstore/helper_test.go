package providerstore

import "testing"

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	key := make([]byte, MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ring, err := NewKeyring(1, key, nil)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	return ring
}

func testKeyringAt(t *testing.T, version int, seed byte) *Keyring {
	t.Helper()
	key := make([]byte, MasterKeySize)
	for i := range key {
		key[i] = seed + byte(i)
	}
	ring, err := NewKeyring(version, key, nil)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	return ring
}

func sealKey(t *testing.T, ring *Keyring, key KeyRow, plaintext []byte) KeyRow {
	t.Helper()
	ciphertext, nonce, version, err := ring.Encrypt(plaintext, key.ID, key.ScopeOwner(), key.AuthScheme)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	key.SecretEnc = ciphertext
	key.SecretNonce = nonce
	key.SecretKVer = version
	key.SecretLast4 = SecretLast4(plaintext, key.AuthScheme)
	return key
}

//nolint:unparam // test helper keeps explicit call-site shape
func validProviderRow(id string, enabled bool) ProviderRow {
	return ProviderRow{
		ID:       id,
		Name:     id,
		Protocol: "openai-chat",
		BaseURL:  "https://" + id + ".example.com/v1",
		Enabled:  enabled,
		TLS:      "default",
	}
}

func baseRows() Rows {
	return Rows{
		Providers: []ProviderRow{validProviderRow("p1", true)},
		Keys: []KeyRow{{
			ID: "k1", ProviderID: "p1", OwnerID: "u1", Name: "key",
			Enabled: true, AuthScheme: SchemeNone,
		}},
		KeyModels: []KeyModelRow{{
			KeyID: "k1", ProviderID: "p1", OwnerID: "u1",
			Alias: "model-1", UpstreamModel: "upstream-1",
		}},
	}
}
