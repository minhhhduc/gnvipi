package providerstore

import "errors"

// Auth scheme names as stored in provider_keys.auth_scheme.
const (
	SchemeNone   = "none"
	SchemeBearer = "bearer"
	SchemeHeader = "header"
	SchemeBasic  = "basic"
)

var (
	// ErrModelNotFound means no configured alias matches the request.
	ErrModelNotFound = errors.New("providerstore: model not found")
	// ErrModelHidden means the model or mapping is hidden or soft-deleted.
	ErrModelHidden = errors.New("providerstore: model is hidden")
	// ErrKeyRequired means the alias is known but the user has no usable key.
	ErrKeyRequired = errors.New("providerstore: provider key required")
	// ErrCredentialUnavailable means configured candidates exist but none has
	// a resolvable credential. The caller maps this to 503.
	ErrCredentialUnavailable = errors.New("providerstore: provider credential unavailable")
	// ErrUnsupportedReference means a key_ref was configured but no reference
	// resolver was supplied. It is never silently skipped.
	ErrUnsupportedReference = errors.New("providerstore: key_ref resolution is not configured")
	// ErrInvalidCredentialSource means a key has multiple credential sources,
	// or an enabled non-none key has none.
	ErrInvalidCredentialSource = errors.New("providerstore: key must have exactly one credential source")
	// ErrClearSecretNotAllowed means clear_secret left an enabled non-none key
	// with no credential.
	ErrClearSecretNotAllowed = errors.New("providerstore: clear_secret requires a disabled key or a replacement source")
	// ErrModelsAmbiguous means a provider-level models write is only legal with
	// exactly one key for that provider.
	ErrModelsAmbiguous = errors.New("providerstore: provider-level models require exactly one key")
	// ErrNoSnapshot means the store has not published a snapshot.
	ErrNoSnapshot = errors.New("providerstore: configuration snapshot is not loaded")
	// ErrInvalidConfig reports a row or JSONB contract violation.
	ErrInvalidConfig = errors.New("providerstore: invalid configuration")
)
