package providerstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

// ProviderRowFromV2 converts the provider-definition fields of a v2 provider
// into a providers row and validates them. Auth and models are owned by
// provider_keys/key_models, so they are not part of the row.
func ProviderRowFromV2(p provider.Provider) (ProviderRow, error) {
	if p.Auth.Scheme == provider.AuthBearer && p.Auth.HeaderName != "" {
		return ProviderRow{}, &provider.FieldError{
			Provider: p.ID,
			Field:    "auth.header_name",
			Problem:  "bearer header overrides cannot be represented by database keys",
		}
	}
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
		return ProviderRow{}, fmt.Errorf("providerstore: provider %q: %w", p.ID, err)
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

// ListProviders reads every provider definition in id order.
func ListProviders(ctx context.Context, q Querier) ([]ProviderRow, error) {
	rows, err := q.Query(ctx, providersQuery)
	if err != nil {
		return nil, fmt.Errorf("providerstore: list providers: %w", err)
	}
	defer rows.Close()
	var providers []ProviderRow
	for rows.Next() {
		row, err := scanProviderRow(rows)
		if err != nil {
			return nil, fmt.Errorf("providerstore: scan provider: %w", err)
		}
		providers = append(providers, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providerstore: list providers: %w", err)
	}
	return providers, nil
}

// GetProviderRowForUpdate locks the provider row so concurrent provider-level
// writes and models replacements serialize on it.
func GetProviderRowForUpdate(ctx context.Context, q Querier, providerID string) (ProviderRow, error) {
	return getProviderRow(ctx, q, providerID, true)
}

// InsertProvider writes one validated provider definition.
func InsertProvider(ctx context.Context, q Querier, row ProviderRow) error {
	_, err := q.Exec(ctx, `INSERT INTO providers
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
		return fmt.Errorf("providerstore: insert provider %q: %w", row.ID, err)
	}
	return nil
}

// UpdateProvider rewrites every mutable provider-definition column.
func UpdateProvider(ctx context.Context, q Querier, row ProviderRow) error {
	tag, err := q.Exec(ctx, `UPDATE providers
		SET name = $2, protocol = $3, base_url = $4, proxy_url = $5, enabled = $6,
		    tls = $7, endpoint_paths = $8::jsonb, forward_headers = $9::jsonb,
		    headers = $10::jsonb, extra_body = $11::jsonb, extensions = $12::jsonb,
		    max_concurrency = $13, queue_timeout = $14, timeouts = $15::jsonb
		WHERE id = $1`,
		row.ID, row.Name, row.Protocol, row.BaseURL, row.ProxyURL, row.Enabled, row.TLS,
		string(row.EndpointPaths), string(row.ForwardHeaders), string(row.Headers),
		string(row.ExtraBody), string(row.Extensions), row.MaxConcurrency, row.QueueTimeout,
		string(row.Timeouts))
	if err != nil {
		return fmt.Errorf("providerstore: update provider %q: %w", row.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderNotFound
	}
	return nil
}

// DeleteProvider removes one provider; its keys and models cascade.
func DeleteProvider(ctx context.Context, q Querier, providerID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM providers WHERE id = $1`, providerID)
	if err != nil {
		return fmt.Errorf("providerstore: delete provider %q: %w", providerID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderNotFound
	}
	return nil
}

// ListProviderKeysForUpdate locks the provider's keys for the duration of the
// transaction, so a models replacement and a concurrent key write serialize.
func ListProviderKeysForUpdate(ctx context.Context, q Querier, providerID string) ([]KeyRecord, error) {
	return listProviderKeys(ctx, q, providerID, "", true)
}

// ListProviderKeyModels reads every alias mapping of a provider with its JSONB
// columns raw, so LegacyView can decode them.
func ListProviderKeyModels(ctx context.Context, q Querier, providerID string) ([]KeyModelRow, error) {
	rows, err := q.Query(ctx, `SELECT key_id::text, provider_id, COALESCE(owner_id::text, ''),
		alias, upstream_model, hidden, fallbacks, fallback_statuses, allow_shadow,
		legacy_position
		FROM key_models WHERE provider_id = $1 ORDER BY key_id, alias`, providerID)
	if err != nil {
		return nil, fmt.Errorf("providerstore: list provider key models: %w", err)
	}
	defer rows.Close()
	var models []KeyModelRow
	for rows.Next() {
		var row KeyModelRow
		if err := rows.Scan(&row.KeyID, &row.ProviderID, &row.OwnerID, &row.Alias,
			&row.UpstreamModel, &row.Hidden, &row.Fallbacks, &row.FallbackStatuses,
			&row.AllowShadow, &row.LegacyPosition); err != nil {
			return nil, fmt.Errorf("providerstore: scan provider key model: %w", err)
		}
		models = append(models, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providerstore: list provider key models: %w", err)
	}
	return models, nil
}

func scanProviderRow(row pgx.Row) (ProviderRow, error) {
	var providerRow ProviderRow
	var endpointPaths, forwardHeaders, headers, extraBody, extensions, timeouts []byte
	err := row.Scan(&providerRow.ID, &providerRow.Name, &providerRow.Protocol,
		&providerRow.BaseURL, &providerRow.ProxyURL, &providerRow.Enabled, &providerRow.TLS,
		&endpointPaths, &forwardHeaders, &headers, &extraBody, &extensions,
		&providerRow.MaxConcurrency, &providerRow.QueueTimeout, &timeouts)
	if err != nil {
		return ProviderRow{}, err
	}
	providerRow.EndpointPaths, providerRow.ForwardHeaders, providerRow.Headers = endpointPaths, forwardHeaders, headers
	providerRow.ExtraBody, providerRow.Extensions, providerRow.Timeouts = extraBody, extensions, timeouts
	return providerRow, nil
}
