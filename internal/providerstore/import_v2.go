package providerstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// ErrV2SourceChanged requires an explicit force flag before replacing an earlier import.
var ErrV2SourceChanged = errors.New("v2 import source differs from the previous import; pass --force to replace it")

// ErrV2ProviderConflict reports an imported provider ID that already exists in
// the database outside this import. Replacement must never delete it.
var ErrV2ProviderConflict = errors.New("v2 import provider already exists outside the import")

type v2ImportMarker struct {
	SourceSHA256 string   `json:"source_sha256"`
	ImportedAt   string   `json:"imported_at"`
	Owner        string   `json:"owner"`
	ProviderIDs  []string `json:"provider_ids,omitempty"`
}

// ImportV2 imports one v2 provider document in a single transaction. It returns
// false when source has already been imported byte-for-byte.
func ImportV2(ctx context.Context, pool *pgxpool.Pool, source []byte, owner string, force bool, keyring *Keyring, refs RefResolver) (bool, error) {
	if pool == nil {
		return false, errors.New("providerstore: nil database pool")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, errors.New("providerstore: import owner is required")
	}
	doc, err := provider.DecodeDocument(source)
	if err != nil {
		return false, fmt.Errorf("providerstore: decode v2 source: %w", err)
	}
	providerRows := make([]ProviderRow, len(doc.Providers))
	for i := range doc.Providers {
		providerRows[i], err = ProviderRowFromV2(doc.Providers[i])
		if err != nil {
			return false, err
		}
	}
	nextLegacyPosition := 1
	for _, p := range doc.Providers {
		for _, model := range p.Models {
			if model.LegacyPosition >= nextLegacyPosition {
				nextLegacyPosition = model.LegacyPosition + 1
			}
		}
	}
	sum := sha256.Sum256(source)
	sourceHash := hex.EncodeToString(sum[:])

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("providerstore: begin v2 import: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // rollback after error or committed transaction

	for _, lock := range []string{"gnvipi:migrations", "gnvipi:bootstrap"} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lock); err != nil {
			return false, fmt.Errorf("providerstore: acquire v2 import lock: %w", err)
		}
	}

	var markerRaw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM system_state WHERE key = 'v2_import' FOR UPDATE`).Scan(&markerRaw); err != nil {
		return false, fmt.Errorf("providerstore: read v2 import marker: %w", err)
	}
	var marker v2ImportMarker
	if err := json.Unmarshal(markerRaw, &marker); err != nil {
		return false, fmt.Errorf("providerstore: decode v2 import marker: %w", err)
	}
	if marker.SourceSHA256 == sourceHash {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("providerstore: commit v2 import no-op: %w", err)
		}
		return false, nil
	}
	if marker.SourceSHA256 != "" && !force {
		return false, ErrV2SourceChanged
	}

	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE username = $1 AND role = 'owner' AND status = 'active'`, owner).Scan(&ownerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("providerstore: active owner %q not found", owner)
		}
		return false, fmt.Errorf("providerstore: find import owner: %w", err)
	}
	providerIDs := make([]string, len(doc.Providers))
	for i := range doc.Providers {
		providerIDs[i] = doc.Providers[i].ID
	}
	if force {
		// Replacement is limited to the providers the previous import created;
		// DB-managed providers, keys and models are never removed here.
		if _, err := tx.Exec(ctx, `DELETE FROM providers WHERE id = ANY($1::text[])`, marker.ProviderIDs); err != nil {
			return false, fmt.Errorf("providerstore: remove prior v2 import providers: %w", err)
		}
	}
	var conflicts []string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(id ORDER BY id), '{}')
		FROM providers WHERE id = ANY($1::text[])`, providerIDs).Scan(&conflicts); err != nil {
		return false, fmt.Errorf("providerstore: check existing providers: %w", err)
	}
	if len(conflicts) > 0 {
		return false, fmt.Errorf("%w: %s", ErrV2ProviderConflict, strings.Join(conflicts, ", "))
	}

	for i, p := range doc.Providers {
		if err := InsertProvider(ctx, tx, providerRows[i]); err != nil {
			return false, err
		}
		key, err := importedKey(ctx, tx, p, ownerID, keyring, refs)
		if err != nil {
			return false, err
		}
		if err := insertImportedKey(ctx, tx, key); err != nil {
			return false, err
		}
		for _, model := range p.Models {
			if model.LegacyPosition == 0 {
				model.LegacyPosition = nextLegacyPosition
				nextLegacyPosition++
			}
			if err := insertImportedModel(ctx, tx, key, model); err != nil {
				return false, err
			}
		}
	}

	marker = v2ImportMarker{
		SourceSHA256: sourceHash,
		ImportedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Owner:        owner,
		ProviderIDs:  providerIDs,
	}
	markerRaw, err = json.Marshal(marker)
	if err != nil {
		return false, fmt.Errorf("providerstore: encode v2 import marker: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE system_state SET value = $1::jsonb WHERE key = 'v2_import'`, string(markerRaw)); err != nil {
		return false, fmt.Errorf("providerstore: write v2 import marker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("providerstore: commit v2 import: %w", err)
	}
	return true, nil
}

func marshalImportJSON(value any, empty string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(raw, []byte("null")) {
		return []byte(empty), nil
	}
	return raw, nil
}

func importedKey(ctx context.Context, tx pgx.Tx, p provider.Provider, ownerID string, keyring *Keyring, refs RefResolver) (KeyRow, error) {
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return KeyRow{}, fmt.Errorf("providerstore: allocate key for provider %q: %w", p.ID, err)
	}
	key := KeyRow{ID: id, ProviderID: p.ID, Name: "v2 import", Enabled: true}
	switch p.Auth.Scheme {
	case "", provider.AuthNone:
		key.AuthScheme = SchemeNone
	case provider.AuthBearer:
		key.OwnerID = ownerID
		key.AuthScheme = SchemeBearer
	case provider.AuthAPIKey:
		key.OwnerID = ownerID
		key.AuthScheme = SchemeHeader
		key.AuthHeaderName = p.Auth.HeaderName
		if key.AuthHeaderName == "" {
			key.AuthHeaderName = "x-api-key"
		}
	default:
		return KeyRow{}, fmt.Errorf("providerstore: provider %q has unsupported auth scheme %q", p.ID, p.Auth.Scheme)
	}

	sources := 0
	var plaintext []byte
	if p.Auth.Key != "" {
		sources++
		plaintext = []byte(p.Auth.Key)
	}
	if p.Auth.KeyEnv != "" {
		sources++
		key.KeyEnv = p.Auth.KeyEnv
	}
	if p.Auth.KeyRef != "" {
		sources++
		if refs == nil {
			return KeyRow{}, &provider.FieldError{Provider: p.ID, Field: "auth.key_ref", Problem: "reference cannot be resolved without the imported secrets map"}
		}
		value, ok := refs.ResolveRef(ctx, p.Auth.KeyRef)
		if !ok {
			return KeyRow{}, &provider.FieldError{Provider: p.ID, Field: "auth.key_ref", Problem: fmt.Sprintf("reference %q is not present in the imported secrets map", p.Auth.KeyRef)}
		}
		// Database keys hold secrets encrypted; a v2 key_ref cannot be resolved
		// at request time, so the imported value becomes the stored secret.
		plaintext = []byte(value)
	}
	if key.AuthScheme == SchemeNone && sources != 0 {
		return KeyRow{}, fmt.Errorf("providerstore: provider %q none auth carries a credential", p.ID)
	}
	if key.AuthScheme != SchemeNone && sources != 1 {
		return KeyRow{}, fmt.Errorf("providerstore: provider %q must have exactly one credential source", p.ID)
	}
	if plaintext != nil {
		if keyring == nil {
			return KeyRow{}, fmt.Errorf("providerstore: provider %q literal credential requires GNVIPI_MASTER_KEY", p.ID)
		}
		var err error
		key.SecretEnc, key.SecretNonce, key.SecretKVer, err = keyring.Encrypt(plaintext, key.ID, key.ScopeOwner(), key.AuthScheme)
		if err != nil {
			return KeyRow{}, fmt.Errorf("providerstore: encrypt provider %q credential: %w", p.ID, err)
		}
		key.SecretLast4 = SecretLast4(plaintext, key.AuthScheme)
	}
	if err := ValidateKeyRow(key); err != nil {
		return KeyRow{}, fmt.Errorf("providerstore: provider %q key: %w", p.ID, err)
	}
	return key, nil
}

func insertImportedKey(ctx context.Context, tx pgx.Tx, key KeyRow) error {
	_, err := tx.Exec(ctx, `INSERT INTO provider_keys
		(id, provider_id, owner_id, name, enabled, auth_scheme, auth_header_name,
		 key_env, key_ref, secret_enc, secret_nonce, secret_kver, secret_last4)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		key.ID, key.ProviderID, nullableString(key.OwnerID), key.Name, key.Enabled,
		key.AuthScheme, nullableString(key.AuthHeaderName), nullableString(key.KeyEnv),
		nullableString(key.KeyRef), nullableBytes(key.SecretEnc), nullableBytes(key.SecretNonce),
		nullableInt(key.SecretKVer), nullableString(key.SecretLast4))
	if err != nil {
		return fmt.Errorf("providerstore: import key for provider %q: %w", key.ProviderID, err)
	}
	return nil
}

func insertImportedModel(ctx context.Context, tx pgx.Tx, key KeyRow, model provider.ModelMapping) error {
	fallbacks, err := marshalImportJSON(model.Fallbacks, `[]`)
	if err != nil {
		return fmt.Errorf("providerstore: provider %q model %q fallbacks: %w", key.ProviderID, model.Alias, err)
	}
	statuses, err := marshalImportJSON(model.FallbackStatuses, `[]`)
	if err != nil {
		return fmt.Errorf("providerstore: provider %q model %q fallback statuses: %w", key.ProviderID, model.Alias, err)
	}
	row := KeyModelRow{
		KeyID:            key.ID,
		ProviderID:       key.ProviderID,
		OwnerID:          key.OwnerID,
		Alias:            model.Alias,
		UpstreamModel:    model.UpstreamModel,
		Hidden:           model.Hidden,
		Fallbacks:        fallbacks,
		FallbackStatuses: statuses,
		AllowShadow:      model.AllowShadow,
		LegacyPosition:   model.LegacyPosition,
	}
	if err := ValidateKeyModelRow(row); err != nil {
		return fmt.Errorf("providerstore: provider %q model %q: %w", key.ProviderID, model.Alias, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO key_models
		(key_id, alias, upstream_model, hidden, fallbacks, fallback_statuses,
		 allow_shadow, legacy_position)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8)`,
		row.KeyID, row.Alias, row.UpstreamModel, row.Hidden, string(row.Fallbacks),
		string(row.FallbackStatuses), row.AllowShadow, row.LegacyPosition)
	if err != nil {
		return fmt.Errorf("providerstore: import model %q for provider %q: %w", row.Alias, key.ProviderID, err)
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
