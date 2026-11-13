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

type v2ImportMarker struct {
	SourceSHA256 string `json:"source_sha256"`
	ImportedAt   string `json:"imported_at"`
	Owner        string `json:"owner"`
}

// ImportV2 imports one v2 provider document in a single transaction. It returns
// false when source has already been imported byte-for-byte.
func ImportV2(ctx context.Context, pool *pgxpool.Pool, source []byte, owner string, force bool, keyring *Keyring) (bool, error) {
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
		providerRows[i], err = importProviderRow(doc.Providers[i])
		if err != nil {
			return false, err
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
	if force {
		if _, err := tx.Exec(ctx, `DELETE FROM providers`); err != nil {
			return false, fmt.Errorf("providerstore: replace prior v2 providers: %w", err)
		}
	}

	for i, p := range doc.Providers {
		if err := insertImportedProvider(ctx, tx, providerRows[i]); err != nil {
			return false, err
		}
		key, err := importedKey(ctx, tx, p, ownerID, keyring)
		if err != nil {
			return false, err
		}
		if err := insertImportedKey(ctx, tx, key); err != nil {
			return false, err
		}
		for _, model := range p.Models {
			if err := insertImportedModel(ctx, tx, key, model); err != nil {
				return false, err
			}
		}
	}

	marker = v2ImportMarker{
		SourceSHA256: sourceHash,
		ImportedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Owner:        owner,
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

func importProviderRow(p provider.Provider) (ProviderRow, error) {
	endpointPaths, err := marshalImportJSON(p.EndpointPaths, `{}`)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q endpoint_paths: %w", p.ID, err)
	}
	forwardHeaders, err := marshalImportJSON(p.ForwardHeaders, `[]`)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q forward_headers: %w", p.ID, err)
	}
	headers, err := marshalImportJSON(p.Headers, `{}`)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q headers: %w", p.ID, err)
	}
	extensions, err := marshalImportJSON(p.Extensions, `{}`)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q extensions: %w", p.ID, err)
	}
	timeouts, err := json.Marshal(p.Timeouts)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q timeouts: %w", p.ID, err)
	}
	extraBody := append([]byte(nil), p.ExtraBody...)
	if len(bytes.TrimSpace(extraBody)) == 0 {
		extraBody = []byte(`{}`)
	}
	tls := string(p.TLS)
	if tls == "" {
		tls = string(provider.TLSDefault)
	}
	row := ProviderRow{
		ID:             p.ID,
		Name:           p.Name,
		Protocol:       p.Protocol.String(),
		BaseURL:        p.BaseURL,
		ProxyURL:       p.ProxyURL,
		Enabled:        p.Enabled,
		TLS:            tls,
		EndpointPaths:  endpointPaths,
		ForwardHeaders: forwardHeaders,
		Headers:        headers,
		ExtraBody:      extraBody,
		Extensions:     extensions,
		Timeouts:       timeouts,
		MaxConcurrency: p.MaxConcurrency,
		QueueTimeout:   int64(p.QueueTimeout),
	}
	definition, err := BuildProviderDefinition(row)
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: imported provider %q: %w", p.ID, err)
	}
	if problems := ValidateSensitiveConfig(row, definition); len(problems) > 0 {
		fieldErrors := make([]*provider.FieldError, len(problems))
		for i, problem := range problems {
			fieldErrors[i] = &provider.FieldError{Provider: p.ID, Field: problem.Field, Problem: problem.Problem}
		}
		return ProviderRow{}, &provider.ValidationError{Errors: fieldErrors}
	}
	return row, nil
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

func insertImportedProvider(ctx context.Context, tx pgx.Tx, row ProviderRow) error {
	_, err := tx.Exec(ctx, `INSERT INTO providers
		(id, name, protocol, base_url, proxy_url, enabled, tls, endpoint_paths,
		 forward_headers, headers, extra_body, extensions, max_concurrency,
		 queue_timeout, timeouts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb,
		        $11::jsonb, $12::jsonb, $13, $14, $15::jsonb)`,
		row.ID, row.Name, row.Protocol, row.BaseURL, row.ProxyURL, row.Enabled, row.TLS,
		string(row.EndpointPaths), string(row.ForwardHeaders), string(row.Headers),
		string(row.ExtraBody), string(row.Extensions), row.MaxConcurrency, row.QueueTimeout,
		string(row.Timeouts))
	if err != nil {
		return fmt.Errorf("providerstore: import provider %q: %w", row.ID, err)
	}
	return nil
}

func importedKey(ctx context.Context, tx pgx.Tx, p provider.Provider, ownerID string, keyring *Keyring) (KeyRow, error) {
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
	if p.Auth.Key != "" {
		sources++
	}
	if p.Auth.KeyEnv != "" {
		sources++
		key.KeyEnv = p.Auth.KeyEnv
	}
	if p.Auth.KeyRef != "" {
		sources++
		key.KeyRef = p.Auth.KeyRef
	}
	if key.AuthScheme == SchemeNone && sources != 0 {
		return KeyRow{}, fmt.Errorf("providerstore: provider %q none auth carries a credential", p.ID)
	}
	if key.AuthScheme != SchemeNone && sources != 1 {
		return KeyRow{}, fmt.Errorf("providerstore: provider %q must have exactly one credential source", p.ID)
	}
	if p.Auth.Key != "" {
		if keyring == nil {
			return KeyRow{}, fmt.Errorf("providerstore: provider %q literal credential requires GNVIPI_MASTER_KEY", p.ID)
		}
		var err error
		key.SecretEnc, key.SecretNonce, key.SecretKVer, err = keyring.Encrypt([]byte(p.Auth.Key), key.ID, key.ScopeOwner(), key.AuthScheme)
		if err != nil {
			return KeyRow{}, fmt.Errorf("providerstore: encrypt provider %q credential: %w", p.ID, err)
		}
		key.SecretLast4 = SecretLast4([]byte(p.Auth.Key), key.AuthScheme)
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
