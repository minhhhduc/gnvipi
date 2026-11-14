package providerstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgx used by the key CRUD helpers. It is satisfied by
// *pgxpool.Pool and pgx.Tx, so callers choose the transaction boundary.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Key CRUD errors. Every one maps to a fixed HTTP response in the admin API.
var (
	// ErrKeyNotFound means no key matches the provider and key id.
	ErrKeyNotFound = errors.New("providerstore: provider key not found")
	// ErrProviderNotFound means the provider id names no configured provider.
	ErrProviderNotFound = errors.New("providerstore: provider not found")
	// ErrDuplicateKeyName means the key name is already used in its owner scope.
	ErrDuplicateKeyName = errors.New("providerstore: provider key name already exists")
	// ErrDuplicateKeyAlias means a model alias is already used in its owner scope.
	ErrDuplicateKeyAlias = errors.New("providerstore: provider key model alias already exists")
)

// KeyRecord is a provider_keys row plus its server-assigned timestamps.
type KeyRecord struct {
	KeyRow
	CreatedAt string
	UpdatedAt string
}

// keyRecordColumns mirrors KeyRow for the CRUD queries. The timestamps are
// rendered by PostgreSQL so callers never reimplement its time formatting.
const keyRecordColumns = `id::text, provider_id, COALESCE(owner_id::text, ''), name, enabled,
	auth_scheme, COALESCE(auth_header_name, ''), COALESCE(key_env, ''),
	COALESCE(key_ref, ''), secret_enc, secret_nonce, COALESCE(secret_kver, 0),
	COALESCE(secret_last4, ''),
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`

func scanKeyRecord(row pgx.Row) (KeyRecord, error) {
	var record KeyRecord
	err := row.Scan(&record.ID, &record.ProviderID, &record.OwnerID, &record.Name, &record.Enabled,
		&record.AuthScheme, &record.AuthHeaderName, &record.KeyEnv, &record.KeyRef,
		&record.SecretEnc, &record.SecretNonce, &record.SecretKVer, &record.SecretLast4,
		&record.CreatedAt, &record.UpdatedAt)
	return record, err
}

// keyConstraintError maps the two name indexes onto ErrDuplicateKeyName and
// leaves every other failure intact.
func keyConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "provider_keys_owner_name_uq", "provider_keys_shared_name_uq":
			return fmt.Errorf("%w: %s", ErrDuplicateKeyName, pgErr.Detail)
		}
	}
	return err
}

func keyModelConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "key_models_owned_alias_uq", "key_models_shared_alias_uq", "key_models_pkey":
			return fmt.Errorf("%w: %s", ErrDuplicateKeyAlias, pgErr.Detail)
		}
	}
	return err
}

// NewKeyID asks PostgreSQL for the id so secrets are encrypted with an AAD that
// names the final row id.
func NewKeyID(ctx context.Context, q Querier) (string, error) {
	var id string
	if err := q.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return "", fmt.Errorf("providerstore: generate key id: %w", err)
	}
	return id, nil
}

// ProviderExists reports whether a provider id is configured without reading
// the whole row.
func ProviderExists(ctx context.Context, q Querier, providerID string) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM providers WHERE id = $1)`, providerID).Scan(&exists); err != nil {
		return false, fmt.Errorf("providerstore: check provider %s: %w", providerID, err)
	}
	return exists, nil
}

// GetProviderRow reads one providers row for credential probing.
func GetProviderRow(ctx context.Context, q Querier, providerID string) (ProviderRow, error) {
	return getProviderRow(ctx, q, providerID, false)
}

func getProviderRow(ctx context.Context, q Querier, providerID string, forUpdate bool) (ProviderRow, error) {
	query := `SELECT id, name, protocol, base_url, proxy_url, enabled, tls,
		endpoint_paths, forward_headers, headers, extra_body, extensions,
		max_concurrency, queue_timeout, timeouts
		FROM providers WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	row, err := scanProviderRow(q.QueryRow(ctx, query, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderRow{}, ErrProviderNotFound
	}
	if err != nil {
		return ProviderRow{}, fmt.Errorf("providerstore: read provider %s: %w", providerID, err)
	}
	return row, nil
}

// ListProviderKeys reads the keys of one provider, shared keys first. A
// non-empty ownerID limits the read to that user's keys, so a caller never
// loads another owner's ciphertext.
func ListProviderKeys(ctx context.Context, q Querier, providerID, ownerID string) ([]KeyRecord, error) {
	return listProviderKeys(ctx, q, providerID, ownerID, false)
}

func listProviderKeys(ctx context.Context, q Querier, providerID, ownerID string, forUpdate bool) ([]KeyRecord, error) {
	query := `SELECT ` + keyRecordColumns + `
		FROM provider_keys WHERE provider_id = $1`
	args := []any{providerID}
	if ownerID != "" {
		query += ` AND owner_id = $2::uuid`
		args = append(args, ownerID)
	}
	query += ` ORDER BY owner_id NULLS FIRST, name, id`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("providerstore: list provider keys: %w", err)
	}
	defer rows.Close()
	var keys []KeyRecord
	for rows.Next() {
		record, err := scanKeyRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("providerstore: scan provider key: %w", err)
		}
		keys = append(keys, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providerstore: list provider keys: %w", err)
	}
	return keys, nil
}

// GetProviderKey reads one key. forUpdate locks the row so a read-modify-write
// update cannot race another writer.
func GetProviderKey(ctx context.Context, q Querier, providerID, keyID string, forUpdate bool) (KeyRecord, error) {
	query := `SELECT ` + keyRecordColumns + `
		FROM provider_keys WHERE provider_id = $1 AND id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	record, err := scanKeyRecord(q.QueryRow(ctx, query, providerID, keyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyRecord{}, ErrKeyNotFound
	}
	if err != nil {
		return KeyRecord{}, fmt.Errorf("providerstore: read provider key %s: %w", keyID, err)
	}
	return record, nil
}

// InsertProviderKey writes one fully validated key row.
func InsertProviderKey(ctx context.Context, q Querier, key KeyRow) (KeyRecord, error) {
	record, err := scanKeyRecord(q.QueryRow(ctx, `INSERT INTO provider_keys
		(id, provider_id, owner_id, name, enabled, auth_scheme, auth_header_name,
		 key_env, key_ref, secret_enc, secret_nonce, secret_kver, secret_last4)
		VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, $4, $5, $6, NULLIF($7, ''),
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, 0), NULLIF($13, ''))
		RETURNING `+keyRecordColumns,
		key.ID, key.ProviderID, key.OwnerID, key.Name, key.Enabled, key.AuthScheme,
		key.AuthHeaderName, key.KeyEnv, key.KeyRef, key.SecretEnc, key.SecretNonce,
		key.SecretKVer, key.SecretLast4))
	if err != nil {
		return KeyRecord{}, keyConstraintError(err)
	}
	return record, nil
}

// UpdateProviderKey rewrites every mutable column of an existing key.
func UpdateProviderKey(ctx context.Context, q Querier, key KeyRow) (KeyRecord, error) {
	record, err := scanKeyRecord(q.QueryRow(ctx, `UPDATE provider_keys
		SET name = $3, enabled = $4, auth_scheme = $5, auth_header_name = NULLIF($6, ''),
		    key_env = NULLIF($7, ''), key_ref = NULLIF($8, ''),
		    secret_enc = $9, secret_nonce = $10, secret_kver = NULLIF($11, 0),
		    secret_last4 = NULLIF($12, '')
		WHERE provider_id = $1 AND id = $2::uuid
		RETURNING `+keyRecordColumns,
		key.ProviderID, key.ID, key.Name, key.Enabled, key.AuthScheme, key.AuthHeaderName,
		key.KeyEnv, key.KeyRef, key.SecretEnc, key.SecretNonce, key.SecretKVer, key.SecretLast4))
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyRecord{}, ErrKeyNotFound
	}
	if err != nil {
		return KeyRecord{}, keyConstraintError(err)
	}
	return record, nil
}

// DeleteProviderKey removes one key; key_models cascades.
func DeleteProviderKey(ctx context.Context, q Querier, providerID, keyID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM provider_keys WHERE provider_id = $1 AND id = $2::uuid`, providerID, keyID)
	if err != nil {
		return fmt.Errorf("providerstore: delete provider key %s: %w", keyID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// KeyModel is one decoded key_models row.
type KeyModel struct {
	Alias            string
	UpstreamModel    string
	Hidden           bool
	Fallbacks        []string
	FallbackStatuses []int
	AllowShadow      bool
	LegacyPosition   int
}

// ListKeyModels reads the alias mappings of one key.
func ListKeyModels(ctx context.Context, q Querier, keyID string) ([]KeyModel, error) {
	rows, err := q.Query(ctx, `SELECT alias, upstream_model, hidden, fallbacks,
		fallback_statuses, allow_shadow, legacy_position
		FROM key_models WHERE key_id = $1::uuid ORDER BY alias`, keyID)
	if err != nil {
		return nil, fmt.Errorf("providerstore: list key models: %w", err)
	}
	defer rows.Close()
	models := make([]KeyModel, 0)
	for rows.Next() {
		var model KeyModel
		var fallbacks, statuses []byte
		if err := rows.Scan(&model.Alias, &model.UpstreamModel, &model.Hidden, &fallbacks,
			&statuses, &model.AllowShadow, &model.LegacyPosition); err != nil {
			return nil, fmt.Errorf("providerstore: scan key model: %w", err)
		}
		if len(fallbacks) > 0 {
			if err := json.Unmarshal(fallbacks, &model.Fallbacks); err != nil {
				return nil, fmt.Errorf("%w: key %s alias %s fallbacks: %w", ErrInvalidConfig, keyID, model.Alias, err)
			}
		}
		if len(statuses) > 0 {
			if err := json.Unmarshal(statuses, &model.FallbackStatuses); err != nil {
				return nil, fmt.Errorf("%w: key %s alias %s fallback_statuses: %w", ErrInvalidConfig, keyID, model.Alias, err)
			}
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providerstore: list key models: %w", err)
	}
	return models, nil
}

// keyModelsMaxPerRequest bounds one replace so a small body cannot open an
// unbounded insert.
const keyModelsMaxPerRequest = 500

// ReplaceKeyModels rewrites the whole alias set of one key. Every row goes
// through the snapshot's validator before the write, and the rows are inserted
// in one multi-row statement, so a rejected row never reaches the database.
func ReplaceKeyModels(ctx context.Context, q Querier, key KeyRow, models []KeyModel) error {
	if len(models) > keyModelsMaxPerRequest {
		return fmt.Errorf("%w: at most %d models per request", ErrInvalidConfig, keyModelsMaxPerRequest)
	}
	rows := make([]KeyModelRow, 0, len(models))
	for _, model := range models {
		fallbacks, err := json.Marshal(model.Fallbacks)
		if err != nil {
			return fmt.Errorf("providerstore: encode fallbacks: %w", err)
		}
		if model.Fallbacks == nil {
			fallbacks = []byte(`[]`)
		}
		statuses, err := json.Marshal(model.FallbackStatuses)
		if err != nil {
			return fmt.Errorf("providerstore: encode fallback statuses: %w", err)
		}
		if model.FallbackStatuses == nil {
			statuses = []byte(`[]`)
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
			return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		rows = append(rows, row)
	}
	if _, err := q.Exec(ctx, `DELETE FROM key_models WHERE key_id = $1::uuid`, key.ID); err != nil {
		return fmt.Errorf("providerstore: clear key models: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	values := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*10)
	for i, row := range rows {
		base := i * 10
		values = append(values, fmt.Sprintf(
			"($%d::uuid, $%d, NULLIF($%d, '')::uuid, $%d, $%d, $%d, $%d::jsonb, $%d::jsonb, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10))
		args = append(args, row.KeyID, row.ProviderID, row.OwnerID, row.Alias,
			row.UpstreamModel, row.Hidden, string(row.Fallbacks), string(row.FallbackStatuses),
			row.AllowShadow, row.LegacyPosition)
	}
	_, err := q.Exec(ctx, `INSERT INTO key_models
		(key_id, provider_id, owner_id, alias, upstream_model, hidden,
		 fallbacks, fallback_statuses, allow_shadow, legacy_position)
		VALUES `+strings.Join(values, ", "), args...)
	if err != nil {
		return keyModelConstraintError(err)
	}
	return nil
}
