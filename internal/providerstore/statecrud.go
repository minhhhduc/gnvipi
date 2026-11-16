package providerstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Model state and settings writes. Both tables are routing inputs, so a
// migration trigger bumps config_state.version on every committed change and
// the watcher republishes the snapshot; readers never see a partial write
// because every call runs inside the caller's transaction.

// ReplaceModelState persists the whole mask and deletion sets. Rows that are
// neither hidden nor deleted are removed, so model_state holds intent only.
// actorID is the account credited with the change; an empty id stores NULL.
func ReplaceModelState(ctx context.Context, q Querier, hidden, deleted []string, actorID string) error {
	hidden = nonEmptySorted(hidden)
	deleted = nonEmptySorted(deleted)
	if err := markModelState(ctx, q, hidden, hideModelsSQL, actorID); err != nil {
		return err
	}
	if err := markModelState(ctx, q, deleted, deleteModelsSQL, actorID); err != nil {
		return err
	}
	// Clearing is one statement per flag: rows the caller dropped from the set
	// lose the flag, and pruning removes the rows that carry nothing.
	if _, err := q.Exec(ctx, `UPDATE model_state
		SET hidden = false, updated_at = now(), updated_by = NULLIF($2, '')::uuid
		WHERE hidden AND NOT (model_id = ANY($1::text[]))`, hidden, actorID); err != nil {
		return fmt.Errorf("providerstore: clear model mask: %w", err)
	}
	if _, err := q.Exec(ctx, `UPDATE model_state
		SET deleted_at = NULL, updated_at = now(), updated_by = NULLIF($2, '')::uuid
		WHERE deleted_at IS NOT NULL AND NOT (model_id = ANY($1::text[]))`, deleted, actorID); err != nil {
		return fmt.Errorf("providerstore: clear deleted models: %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM model_state WHERE NOT hidden AND deleted_at IS NULL`); err != nil {
		return fmt.Errorf("providerstore: prune model state: %w", err)
	}
	return nil
}

// The insert branch must carry the flag itself: a new row defaults to visible
// and undeleted, so the ON CONFLICT clause alone would store nothing.
const (
	hideModelsSQL = `INSERT INTO model_state (model_id, hidden, updated_by)
		SELECT id, true, NULLIF($2, '')::uuid FROM unnest($1::text[]) AS id
		ON CONFLICT (model_id) DO UPDATE
		SET hidden = true, updated_at = now(), updated_by = EXCLUDED.updated_by`
	deleteModelsSQL = `INSERT INTO model_state (model_id, deleted_at, updated_by)
		SELECT id, now(), NULLIF($2, '')::uuid FROM unnest($1::text[]) AS id
		ON CONFLICT (model_id) DO UPDATE
		SET deleted_at = now(), updated_at = now(), updated_by = EXCLUDED.updated_by`
)

// markModelState raises one flag for every id. statement is one of the fixed
// constants above, never caller text.
func markModelState(ctx context.Context, q Querier, ids []string, statement, actorID string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, statement, ids, actorID); err != nil {
		return fmt.Errorf("providerstore: save model state: %w", err)
	}
	return nil
}

// LoadModelState reads the committed mask and deletion sets.
func LoadModelState(ctx context.Context, q Querier) (hidden, deleted []string, err error) {
	rows, err := q.Query(ctx, `SELECT model_id, hidden, deleted_at IS NOT NULL FROM model_state`)
	if err != nil {
		return nil, nil, fmt.Errorf("providerstore: load model state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var isHidden, isDeleted bool
		if err := rows.Scan(&id, &isHidden, &isDeleted); err != nil {
			return nil, nil, fmt.Errorf("providerstore: scan model state: %w", err)
		}
		if isHidden {
			hidden = append(hidden, id)
		}
		if isDeleted {
			deleted = append(deleted, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("providerstore: load model state: %w", err)
	}
	slices.Sort(hidden)
	slices.Sort(deleted)
	return hidden, deleted, nil
}

// PutSetting upserts one settings row. key must be one the table allows.
func PutSetting(ctx context.Context, q Querier, key string, value any, actorID string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("providerstore: setting %q: %w", key, err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO settings (key, value, updated_by)
		VALUES ($1, $2::jsonb, NULLIF($3, '')::uuid)
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, string(raw), actorID); err != nil {
		return fmt.Errorf("providerstore: save setting %q: %w", key, err)
	}
	return nil
}

// LoadSettingObject decodes one settings row into target. A missing key leaves
// target untouched and reports false.
func LoadSettingObject(ctx context.Context, q Querier, key string, target any) (bool, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("providerstore: load setting %q: %w", key, err)
	}
	if err := decodeSettingObject(raw, target); err != nil {
		return false, fmt.Errorf("%w: settings.%s: %w", ErrInvalidConfig, key, err)
	}
	return true, nil
}

// System state is the free-form document store for runtime state no table
// column owns, such as the captcha champion selection.

// SaveSystemState upserts one system_state document.
func SaveSystemState(ctx context.Context, q Querier, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("providerstore: system state %q: %w", key, err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO system_state (key, value)
		VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, string(raw)); err != nil {
		return fmt.Errorf("providerstore: save system state %q: %w", key, err)
	}
	return nil
}

// LoadSystemState decodes the system_state document under key into target. A
// missing key leaves target untouched and reports false.
func LoadSystemState(ctx context.Context, q Querier, key string, target any) (bool, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT value FROM system_state WHERE key = $1`, key).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("providerstore: load system state %q: %w", key, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false, fmt.Errorf("providerstore: decode system state %q: %w", key, err)
	}
	return true, nil
}

func nonEmptySorted(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
