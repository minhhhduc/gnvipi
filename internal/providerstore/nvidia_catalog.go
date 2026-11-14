package providerstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/minhhhduc/gnvipi/internal/models"
)

const (
	nvidiaCatalogQuery = `SELECT model, slug, namespace, function_id::text
		FROM nvidia_models ORDER BY model`
	nvidiaCatalogInsert = `INSERT INTO nvidia_models (model, slug, namespace, function_id)
		VALUES ($1, $2, $3, $4)`
)

// LoadNvidiaCatalog reads the catalog the last rescan committed. An empty table
// is an empty catalog, not an error.
func LoadNvidiaCatalog(ctx context.Context, q Querier) ([]models.PlaygroundRow, error) {
	rows, err := q.Query(ctx, nvidiaCatalogQuery)
	if err != nil {
		return nil, fmt.Errorf("providerstore: load nvidia catalog: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	out := make([]models.PlaygroundRow, 0)
	for rows.Next() {
		var row models.PlaygroundRow
		if err := rows.Scan(&row.Model, &row.Slug, &row.Namespace, &row.FunctionID); err != nil {
			return nil, fmt.Errorf("providerstore: scan nvidia catalog: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providerstore: load nvidia catalog: %w", err)
	}
	return out, nil
}

// ReplaceNvidiaCatalog replaces the whole catalog inside the caller's
// transaction. Rows are validated before the DELETE, and the INSERTs are sent
// as one batch. The caller commits; a failed insert leaves the table unchanged
// when the transaction is rolled back.
func ReplaceNvidiaCatalog(ctx context.Context, tx pgx.Tx, rows []models.PlaygroundRow) error {
	if tx == nil {
		return errors.New("providerstore: replace nvidia catalog: nil transaction")
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Model == "" || row.Slug == "" || row.Namespace == "" || row.FunctionID == "" {
			return fmt.Errorf("providerstore: nvidia catalog entry %q is incomplete", row.Model)
		}
		if _, duplicate := seen[row.Model]; duplicate {
			return fmt.Errorf("providerstore: duplicate nvidia catalog entry %q", row.Model)
		}
		seen[row.Model] = struct{}{}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM nvidia_models`); err != nil {
		return fmt.Errorf("providerstore: clear nvidia catalog: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(nvidiaCatalogInsert, row.Model, row.Slug, row.Namespace, row.FunctionID)
	}
	results := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			_ = results.Close() //nolint:errcheck // best-effort cleanup after a failed insert
			return fmt.Errorf("providerstore: insert nvidia catalog: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("providerstore: insert nvidia catalog: %w", err)
	}
	return nil
}
