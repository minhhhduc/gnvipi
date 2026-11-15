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
	// ErrModelNotAvailable means an alias this account used to reach no longer
	// maps for it. Typed so callers need not parse a message; it matches
	// ErrModelNotFound so the 404 and delegation paths are unchanged, and its
	// message never says whether the alias exists for anyone else.
	ErrModelNotAvailable error = modelNotAvailableError{}
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

// modelNotAvailableError is the typed 404 for an alias whose mapping stopped
// resolving for one account.
type modelNotAvailableError struct{}

func (modelNotAvailableError) Error() string {
	return "providerstore: this model is no longer available for this account"
}

// Is matches ErrModelNotFound so StatusForError and key-aware delegation keep
// their existing behavior, and matches ErrModelNotAvailable itself.
func (modelNotAvailableError) Is(target error) bool {
	return errors.Is(target, ErrModelNotFound) || errors.Is(target, ErrModelNotAvailable)
}
