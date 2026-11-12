package providerstore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestEnvelopeRoundTripAndVersion(t *testing.T) {
	ring := testKeyring(t)
	plaintext := []byte("sk-test-0123456789")
	ciphertext, nonce, version, err := ring.Encrypt(plaintext, "key-1", "user-1", SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	if len(nonce) != SecretNonceSize {
		t.Fatalf("nonce length = %d, want %d", len(nonce), SecretNonceSize)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := ring.Decrypt(ciphertext, nonce, "key-1", "user-1", SchemeBearer, version)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext = %q, want %q", got, plaintext)
	}
}

func TestEnvelopeAADBinding(t *testing.T) {
	ring := testKeyring(t)
	ciphertext, nonce, version, err := ring.Encrypt([]byte("secret"), "key-1", "user-1", SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cases := []struct {
		name   string
		keyID  string
		owner  string
		scheme string
	}{
		{"key id", "key-2", "user-1", SchemeBearer},
		{"scope owner", "key-1", "user-2", SchemeBearer},
		{"auth scheme", "key-1", "user-1", SchemeHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ring.Decrypt(ciphertext, nonce, tc.keyID, tc.owner, tc.scheme, version); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("decrypt error = %v, want ErrDecrypt", err)
			}
		})
	}
}

func TestEnvelopeTamperAndNonceLength(t *testing.T) {
	ring := testKeyring(t)
	ciphertext, nonce, version, err := ring.Encrypt([]byte("secret"), "key-1", "", SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xff
	if _, err := ring.Decrypt(tampered, nonce, "key-1", "", SchemeBearer, version); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext error = %v, want ErrDecrypt", err)
	}
	shortNonce := nonce[:SecretNonceSize-1]
	if _, err := ring.Decrypt(ciphertext, shortNonce, "key-1", "", SchemeBearer, version); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("short nonce error = %v, want ErrInvalidNonce", err)
	}
	if _, err := ring.Decrypt(nil, nonce, "key-1", "", SchemeBearer, version); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("empty ciphertext error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestEnvelopeUnknownVersionIsHardFailure(t *testing.T) {
	oldRing := testKeyringAt(t, 1, 10)
	ciphertext, nonce, version, err := oldRing.Encrypt([]byte("secret"), "key-1", "", SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	current := testKeyringAt(t, 2, 90)
	if _, err := current.Decrypt(ciphertext, nonce, "key-1", "", SchemeBearer, version); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("error = %v, want ErrUnknownKeyVersion", err)
	}
	// Rotation: the previous key is explicitly configured and decrypts.
	previousRing, err := NewKeyring(2, mustKeyBytes(t, 90), map[int][]byte{1: mustKeyBytes(t, 10)})
	if err != nil {
		t.Fatalf("new rotated keyring: %v", err)
	}
	got, err := previousRing.Decrypt(ciphertext, nonce, "key-1", "", SchemeBearer, version)
	if err != nil || string(got) != "secret" {
		t.Fatalf("rotated decrypt = %q, %v", got, err)
	}
}

func mustKeyBytes(t *testing.T, seed byte) []byte {
	t.Helper()
	key := make([]byte, MasterKeySize)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func TestKeyringFromEnv(t *testing.T) {
	master := make([]byte, MasterKeySize)
	for i := range master {
		master[i] = byte(i)
	}
	encoded := base64Std(master)
	env := map[string]string{
		"GNVIPI_MASTER_KEY":          encoded,
		"GNVIPI_MASTER_KEY_VERSION":  "3",
		"GNVIPI_MASTER_KEY_PREVIOUS": "1:" + encoded,
	}
	ring, err := KeyringFromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("keyring from env: %v", err)
	}
	if ring.CurrentVersion() != 3 {
		t.Fatalf("version = %d, want 3", ring.CurrentVersion())
	}
	if _, err := KeyringFromEnv(func(string) string { return "" }); !errors.Is(err, ErrMissingMasterKey) {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := KeyringFromEnv(func(string) string { return "not-base64" }); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("bad key error = %v", err)
	}
	short := base64Std([]byte{1, 2, 3})
	if _, err := KeyringFromEnv(func(string) string { return short }); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("short key error = %v", err)
	}
}

func TestSecretLast4BasicUsesPassword(t *testing.T) {
	plaintext := []byte(`{"username":"alice","password":"p@ssw0rd"}`)
	if got := SecretLast4(plaintext, SchemeBasic); got != "w0rd" {
		t.Fatalf("last4 = %q, want w0rd", got)
	}
	if got := SecretLast4([]byte("abcdef"), SchemeBearer); got != "cdef" {
		t.Fatalf("last4 = %q, want cdef", got)
	}
}

func TestDecryptErrorDoesNotLeakCiphertextOrPlaintext(t *testing.T) {
	ring := testKeyring(t)
	ciphertext, nonce, version, err := ring.Encrypt([]byte("super-secret-value"), "key-1", "", SchemeBearer)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = ring.Decrypt(ciphertext, nonce, "key-2", "", SchemeBearer, version)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error leaks plaintext: %v", err)
	}
}

func base64Std(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}
