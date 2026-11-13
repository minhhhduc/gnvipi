package providerstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// SecretNonceSize is the fixed AES-GCM nonce length in bytes.
	SecretNonceSize = 12
	// MasterKeySize is the required raw master key length in bytes.
	MasterKeySize = 32
	// ZeroScopeOwner is the generated scope_owner sentinel for shared keys.
	ZeroScopeOwner = "00000000-0000-0000-0000-000000000000"
)

var (
	// ErrMissingMasterKey is returned when encrypted secrets exist but no
	// master key was configured. It is never replaced by an environment or
	// file fallback.
	ErrMissingMasterKey = errors.New("providerstore: master key is not configured")
	// ErrInvalidMasterKey reports malformed master-key material.
	ErrInvalidMasterKey = errors.New("providerstore: invalid master key")
	// ErrUnknownKeyVersion reports a secret encrypted with a key version the
	// keyring does not hold. It is a hard failure, never a fallback.
	ErrUnknownKeyVersion = errors.New("providerstore: unknown master key version")
	// ErrDecrypt reports an authentication or plaintext failure.
	ErrDecrypt = errors.New("providerstore: secret decryption failed")
	// ErrInvalidNonce reports a nonce that is not exactly 12 bytes.
	ErrInvalidNonce = errors.New("providerstore: invalid secret nonce")
	// ErrInvalidCiphertext reports empty ciphertext.
	ErrInvalidCiphertext = errors.New("providerstore: invalid secret ciphertext")
)

type masterKey struct {
	version int
	key     [MasterKeySize]byte
}

// Keyring holds the current encryption key and, during rotation, previous
// decryption keys. Encryption always uses the current version.
type Keyring struct {
	current  masterKey
	previous map[int]masterKey
}

// NewKeyring validates and builds a keyring. previous may be nil.
func NewKeyring(currentVersion int, current []byte, previous map[int][]byte) (*Keyring, error) {
	if currentVersion <= 0 {
		return nil, fmt.Errorf("%w: version must be positive", ErrInvalidMasterKey)
	}
	cur, err := copyMasterKey(current)
	if err != nil {
		return nil, err
	}
	k := &Keyring{current: masterKey{version: currentVersion, key: cur}, previous: make(map[int]masterKey, len(previous))}
	for version, raw := range previous {
		if version <= 0 {
			return nil, fmt.Errorf("%w: previous version must be positive", ErrInvalidMasterKey)
		}
		if version == currentVersion {
			return nil, fmt.Errorf("%w: previous version %d duplicates the current version", ErrInvalidMasterKey, version)
		}
		key, err := copyMasterKey(raw)
		if err != nil {
			return nil, fmt.Errorf("previous key version %d: %w", version, err)
		}
		k.previous[version] = masterKey{version: version, key: key}
	}
	return k, nil
}

func copyMasterKey(raw []byte) ([MasterKeySize]byte, error) {
	var out [MasterKeySize]byte
	if len(raw) != MasterKeySize {
		return out, fmt.Errorf("%w: expected %d bytes, got %d", ErrInvalidMasterKey, MasterKeySize, len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

// DecodeMasterKey decodes a standard-base64 32-byte key. Unpadded standard and
// URL encodings are accepted for operator convenience.
func DecodeMasterKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrMissingMasterKey
	}
	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, decoder := range decoders {
		decoded, err := decoder.DecodeString(raw)
		if err == nil {
			if len(decoded) != MasterKeySize {
				return nil, fmt.Errorf("%w: expected %d decoded bytes, got %d", ErrInvalidMasterKey, MasterKeySize, len(decoded))
			}
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%w: must be base64", ErrInvalidMasterKey)
}

// LoadKeyringFromEnv reads GNVIPI_MASTER_KEY, GNVIPI_MASTER_KEY_VERSION and
// GNVIPI_MASTER_KEY_PREVIOUS. The previous value is a comma-separated list of
// "version:base64" pairs.
func LoadKeyringFromEnv() (*Keyring, error) {
	return KeyringFromEnv(os.Getenv)
}

// KeyringFromEnv is LoadKeyringFromEnv with an injected environment lookup, so
// tests never mutate process state.
func KeyringFromEnv(getenv func(string) string) (*Keyring, error) {
	master := strings.TrimSpace(getenv("GNVIPI_MASTER_KEY"))
	if master == "" {
		return nil, ErrMissingMasterKey
	}
	rawKey, err := DecodeMasterKey(master)
	if err != nil {
		return nil, err
	}
	version := 1
	if raw := strings.TrimSpace(getenv("GNVIPI_MASTER_KEY_VERSION")); raw != "" {
		version, err = strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: GNVIPI_MASTER_KEY_VERSION must be an integer", ErrInvalidMasterKey)
		}
	}
	previous, err := decodePreviousKeys(getenv("GNVIPI_MASTER_KEY_PREVIOUS"))
	if err != nil {
		return nil, err
	}
	return NewKeyring(version, rawKey, previous)
}

func decodePreviousKeys(raw string) (map[int][]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[int][]byte)
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}) {
		versionText, keyText, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%w: previous keys must be version:base64 pairs", ErrInvalidMasterKey)
		}
		version, err := strconv.Atoi(strings.TrimSpace(versionText))
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("%w: previous key version must be a positive integer", ErrInvalidMasterKey)
		}
		key, err := DecodeMasterKey(keyText)
		if err != nil {
			return nil, fmt.Errorf("previous key version %d: %w", version, err)
		}
		out[version] = key
	}
	return out, nil
}

// CurrentVersion returns the version used for new encryptions.
func (k *Keyring) CurrentVersion() int {
	if k == nil {
		return 0
	}
	return k.current.version
}

// SecretAAD builds the AES-GCM additional authenticated data. It binds the
// ciphertext to the key id, owner scope, auth scheme and key version.
func SecretAAD(keyID, scopeOwner, scheme string, keyVersion int) []byte {
	if scopeOwner == "" {
		scopeOwner = ZeroScopeOwner
	}
	return []byte("gnvipi:provider-key:v1:" + keyID + ":" + scopeOwner + ":" + scheme + ":" + strconv.Itoa(keyVersion))
}

// Encrypt seals plaintext for one provider key and returns ciphertext,
// nonce and the master-key version used.
func (k *Keyring) Encrypt(plaintext []byte, keyID, scopeOwner, scheme string) (ciphertext, nonce []byte, keyVersion int, err error) {
	if k == nil {
		return nil, nil, 0, ErrMissingMasterKey
	}
	gcm, err := k.gcm(k.current.version)
	if err != nil {
		return nil, nil, 0, err
	}
	nonce = make([]byte, SecretNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("providerstore: read nonce: %w", err)
	}
	aad := SecretAAD(keyID, scopeOwner, scheme, k.current.version)
	ciphertext = gcm.Seal(nil, nonce, plaintext, aad)
	return ciphertext, nonce, k.current.version, nil
}

// Decrypt opens ciphertext for one provider key. Unknown key versions,
// malformed nonces and authentication failures are hard errors; the caller
// must not fall back to another credential.
func (k *Keyring) Decrypt(ciphertext, nonce []byte, keyID, scopeOwner, scheme string, keyVersion int) ([]byte, error) {
	if k == nil {
		return nil, ErrMissingMasterKey
	}
	if keyVersion <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, keyVersion)
	}
	if len(nonce) != SecretNonceSize {
		return nil, ErrInvalidNonce
	}
	if len(ciphertext) == 0 {
		return nil, ErrInvalidCiphertext
	}
	gcm, err := k.gcm(keyVersion)
	if err != nil {
		return nil, err
	}
	aad := SecretAAD(keyID, scopeOwner, scheme, keyVersion)
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w for key version %d", ErrDecrypt, keyVersion)
	}
	return plaintext, nil
}

func (k *Keyring) gcm(version int) (cipher.AEAD, error) {
	var key masterKey
	switch version {
	case k.current.version:
		key = k.current
	default:
		previous, ok := k.previous[version]
		if !ok {
			return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, version)
		}
		key = previous
	}
	block, err := aes.NewCipher(key.key[:])
	if err != nil {
		return nil, fmt.Errorf("providerstore: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("providerstore: create GCM: %w", err)
	}
	return gcm, nil
}

// SecretLast4 derives the display-only suffix. For basic auth it is the
// password's last four characters; for other schemes the raw credential's.
func SecretLast4(plaintext []byte, scheme string) string {
	if scheme == SchemeBasic {
		var basic BasicSecret
		if err := json.Unmarshal(plaintext, &basic); err == nil {
			return tail4(basic.Password)
		}
	}
	return tail4(string(plaintext))
}

func tail4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// BasicSecret is the plaintext form of a basic-auth provider key.
type BasicSecret struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
