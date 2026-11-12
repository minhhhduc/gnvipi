package providerstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// RefResolver resolves provider_keys.key_ref values from the imported secrets
// map. Returning ok=false is a hard key error; a nil resolver is a typed
// unsupported error, never a silent skip.
type RefResolver interface {
	ResolveRef(ctx context.Context, ref string) (string, bool)
}

// MapRefResolver is an in-memory RefResolver, suitable for the v2 import
// secrets map and tests.
type MapRefResolver map[string]string

// ResolveRef implements RefResolver.
func (m MapRefResolver) ResolveRef(_ context.Context, ref string) (string, bool) {
	value, ok := m[ref]
	return value, ok && value != ""
}

// ResolvedCredential is one usable credential. Raw is the header value for
// bearer/header; Basic is populated for basic auth. Neither is ever returned
// by an API or written to a log.
type ResolvedCredential struct {
	Raw   string
	Basic BasicSecret
}

// ResolveKeyCredential deterministically resolves the single configured
// source. Decrypt failures, missing environment variables and unresolved
// references are hard errors; there is no cross-source fallback.
func ResolveKeyCredential(ctx context.Context, key KeyRow, keyring *Keyring, refs RefResolver) (ResolvedCredential, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedCredential{}, err
	}
	sources := 0
	if key.HasSecret() {
		sources++
	}
	if key.KeyEnv != "" {
		sources++
	}
	if key.KeyRef != "" {
		sources++
	}
	if sources > 1 {
		return ResolvedCredential{}, fmt.Errorf("%w: key %s", ErrInvalidCredentialSource, key.ID)
	}
	if key.AuthScheme == SchemeNone {
		if sources != 0 {
			return ResolvedCredential{}, fmt.Errorf("%w: none key %s carries a credential", ErrInvalidCredentialSource, key.ID)
		}
		return ResolvedCredential{}, nil
	}
	switch {
	case key.HasSecret():
		if keyring == nil {
			return ResolvedCredential{}, ErrMissingMasterKey
		}
		plaintext, err := keyring.Decrypt(key.SecretEnc, key.SecretNonce, key.ID, key.ScopeOwner(), key.AuthScheme, key.SecretKVer)
		if err != nil {
			return ResolvedCredential{}, err
		}
		return credentialFromPlaintext(key.AuthScheme, plaintext)
	case key.KeyEnv != "":
		value, ok := os.LookupEnv(key.KeyEnv)
		if !ok || value == "" {
			return ResolvedCredential{}, fmt.Errorf("%w: key %s environment credential is empty", ErrCredentialUnavailable, key.ID)
		}
		return credentialFromPlaintext(key.AuthScheme, []byte(value))
	case key.KeyRef != "":
		if refs == nil {
			return ResolvedCredential{}, fmt.Errorf("%w: key %s", ErrUnsupportedReference, key.ID)
		}
		value, ok := refs.ResolveRef(ctx, key.KeyRef)
		if !ok || value == "" {
			return ResolvedCredential{}, fmt.Errorf("%w: key %s reference is unresolved", ErrCredentialUnavailable, key.ID)
		}
		return credentialFromPlaintext(key.AuthScheme, []byte(value))
	default:
		return ResolvedCredential{}, fmt.Errorf("%w: enabled key %s has no credential", ErrInvalidCredentialSource, key.ID)
	}
}

func credentialFromPlaintext(scheme string, plaintext []byte) (ResolvedCredential, error) {
	if scheme != SchemeBasic {
		return ResolvedCredential{Raw: string(plaintext)}, nil
	}
	basic, err := parseBasicSecret(plaintext)
	if err != nil {
		return ResolvedCredential{}, err
	}
	return ResolvedCredential{Basic: basic}, nil
}

func parseBasicSecret(plaintext []byte) (BasicSecret, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(plaintext), &fields); err != nil || fields == nil {
		return BasicSecret{}, fmt.Errorf("%w: basic secret must be a JSON object", ErrInvalidCredentialSource)
	}
	var basic BasicSecret
	for name, target := range map[string]*string{"username": &basic.Username, "password": &basic.Password} {
		raw, ok := fields[name]
		if !ok {
			return BasicSecret{}, fmt.Errorf("%w: basic secret is missing %s", ErrInvalidCredentialSource, name)
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return BasicSecret{}, fmt.Errorf("%w: basic secret %s must be a string", ErrInvalidCredentialSource, name)
		}
	}
	if basic.Username == "" {
		return BasicSecret{}, fmt.Errorf("%w: basic secret username is empty", ErrInvalidCredentialSource)
	}
	return basic, nil
}

// KeyPatch is the write-only credential patch accepted by key updates. Nil
// pointers mean "unchanged"; a pointer to an empty string clears that field.
type KeyPatch struct {
	Enabled        *bool
	AuthScheme     *string
	AuthHeaderName *string
	Secret         []byte
	KeyEnv         *string
	KeyRef         *string
	ClearSecret    bool
}

// ApplyKeyPatch applies clear_secret and credential-transition semantics, then
// validates the final row. A new secret is encrypted with the current master
// key version and the final scheme.
func ApplyKeyPatch(cur KeyRow, patch KeyPatch, keyring *Keyring) (KeyRow, error) {
	next := cur
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if patch.AuthScheme != nil {
		next.AuthScheme = *patch.AuthScheme
	}
	if next.AuthScheme != SchemeNone && next.AuthScheme != SchemeBearer && next.AuthScheme != SchemeHeader && next.AuthScheme != SchemeBasic {
		return KeyRow{}, fmt.Errorf("%w: unsupported auth_scheme %q", ErrInvalidCredentialSource, next.AuthScheme)
	}
	if patch.ClearSecret && len(patch.Secret) > 0 {
		return KeyRow{}, fmt.Errorf("%w: clear_secret and secret are mutually exclusive", ErrInvalidCredentialSource)
	}
	settingEnv := patch.KeyEnv != nil && *patch.KeyEnv != ""
	settingRef := patch.KeyRef != nil && *patch.KeyRef != ""
	if len(patch.Secret) > 0 && (settingEnv || settingRef) {
		return KeyRow{}, fmt.Errorf("%w: secret, key_env and key_ref cannot coexist", ErrInvalidCredentialSource)
	}
	if settingEnv && settingRef {
		return KeyRow{}, fmt.Errorf("%w: key_env and key_ref cannot coexist", ErrInvalidCredentialSource)
	}

	if patch.ClearSecret {
		clearSecret(&next)
	}
	if patch.KeyEnv != nil {
		next.KeyEnv = *patch.KeyEnv
		if next.KeyEnv != "" {
			clearSecret(&next)
			next.KeyRef = ""
		}
	}
	if patch.KeyRef != nil {
		next.KeyRef = *patch.KeyRef
		if next.KeyRef != "" {
			clearSecret(&next)
			next.KeyEnv = ""
		}
	}
	if len(patch.Secret) > 0 {
		if next.AuthScheme == SchemeBasic {
			if _, err := parseBasicSecret(patch.Secret); err != nil {
				return KeyRow{}, err
			}
		}
		if keyring == nil {
			return KeyRow{}, ErrMissingMasterKey
		}
		ciphertext, nonce, version, err := keyring.Encrypt(patch.Secret, next.ID, next.ScopeOwner(), next.AuthScheme)
		if err != nil {
			return KeyRow{}, err
		}
		next.SecretEnc = ciphertext
		next.SecretNonce = nonce
		next.SecretKVer = version
		next.SecretLast4 = SecretLast4(patch.Secret, next.AuthScheme)
		next.KeyEnv = ""
		next.KeyRef = ""
	}

	if patch.AuthHeaderName != nil {
		next.AuthHeaderName = *patch.AuthHeaderName
	}
	switch next.AuthScheme {
	case SchemeNone:
		clearSecret(&next)
		next.KeyEnv, next.KeyRef, next.AuthHeaderName = "", "", ""
	case SchemeHeader:
		if next.AuthHeaderName == "" {
			return KeyRow{}, fmt.Errorf("%w: header auth requires auth_header_name", ErrInvalidCredentialSource)
		}
	case SchemeBearer, SchemeBasic:
		next.AuthHeaderName = ""
	}

	// The ciphertext AAD binds the auth scheme. An unchanged stored secret with
	// a new scheme would never decrypt, so require re-entry of the secret.
	if next.HasSecret() && len(patch.Secret) == 0 && next.AuthScheme != cur.AuthScheme {
		return KeyRow{}, fmt.Errorf("%w: changing auth_scheme requires re-entering the secret", ErrInvalidCredentialSource)
	}

	if err := ValidateKeyRow(next); err != nil {
		if patch.ClearSecret {
			return KeyRow{}, fmt.Errorf("%w: %w", ErrClearSecretNotAllowed, err)
		}
		return KeyRow{}, err
	}
	return next, nil
}

func clearSecret(key *KeyRow) {
	key.SecretEnc = nil
	key.SecretNonce = nil
	key.SecretKVer = 0
	key.SecretLast4 = ""
}
