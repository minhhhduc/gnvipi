package providerstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RowSource supplies a consistent configuration read and the current
// config_state.version. A production implementation is DBRowSource; tests may
// supply deterministic rows without simulating SQL semantics.
type RowSource interface {
	LoadConfig(ctx context.Context) (Rows, int64, error)
	Version(ctx context.Context) (int64, error)
}

// DBRowSource reads configuration through a pgx pool under
// BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY.
type DBRowSource struct {
	Pool *pgxpool.Pool
}

// NewDBRowSource wraps a pool.
func NewDBRowSource(pool *pgxpool.Pool) *DBRowSource { return &DBRowSource{Pool: pool} }

const (
	providersQuery = `SELECT id, name, protocol, base_url, proxy_url, enabled, tls,
		endpoint_paths, forward_headers, headers, extra_body, extensions,
		max_concurrency, queue_timeout, timeouts
		FROM providers ORDER BY id`
	keysQuery = `SELECT id::text, provider_id, COALESCE(owner_id::text, ''), name, enabled,
		auth_scheme, COALESCE(auth_header_name, ''), COALESCE(key_env, ''),
		COALESCE(key_ref, ''), secret_enc, secret_nonce, COALESCE(secret_kver, 0),
		COALESCE(secret_last4, '')
		FROM provider_keys
		ORDER BY provider_id, owner_id NULLS FIRST, id`
	keyModelsQuery = `SELECT key_id::text, provider_id, COALESCE(owner_id::text, ''),
		alias, upstream_model, hidden, fallbacks, fallback_statuses, allow_shadow,
		legacy_position
		FROM key_models
		ORDER BY provider_id, owner_id NULLS FIRST, key_id, alias`
	modelStateQuery = `SELECT model_id, hidden, deleted_at IS NOT NULL
		FROM model_state ORDER BY model_id`
	settingsQuery = `SELECT key, value FROM settings ORDER BY key`
	versionQuery  = `SELECT version FROM config_state WHERE id`
)

// LoadConfig reads every configuration table in one repeatable-read, read-only
// transaction. Any read error rolls back and returns no partial rows.
func (s *DBRowSource) LoadConfig(ctx context.Context) (Rows, int64, error) {
	if s == nil || s.Pool == nil {
		return Rows{}, 0, errors.New("providerstore: nil database pool")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: begin snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort cleanup/best-effort parse

	var version int64
	if err := tx.QueryRow(ctx, versionQuery).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Rows{}, 0, errors.New("providerstore: config_state row is missing")
		}
		return Rows{}, 0, fmt.Errorf("providerstore: read config version: %w", err)
	}
	var rows Rows

	providerRows, err := tx.Query(ctx, providersQuery)
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: read providers: %w", err)
	}
	for providerRows.Next() {
		var row ProviderRow
		var endpointPaths, forwardHeaders, headers, extraBody, extensions, timeouts []byte
		if err := providerRows.Scan(&row.ID, &row.Name, &row.Protocol, &row.BaseURL, &row.ProxyURL,
			&row.Enabled, &row.TLS, &endpointPaths, &forwardHeaders, &headers, &extraBody,
			&extensions, &row.MaxConcurrency, &row.QueueTimeout, &timeouts); err != nil {
			providerRows.Close()
			return Rows{}, 0, fmt.Errorf("providerstore: scan provider: %w", err)
		}
		row.EndpointPaths, row.ForwardHeaders, row.Headers = endpointPaths, forwardHeaders, headers
		row.ExtraBody, row.Extensions, row.Timeouts = extraBody, extensions, timeouts
		rows.Providers = append(rows.Providers, row)
	}
	if err := providerRows.Err(); err != nil {
		providerRows.Close()
		return Rows{}, 0, fmt.Errorf("providerstore: read providers: %w", err)
	}
	providerRows.Close()

	keyRows, err := tx.Query(ctx, keysQuery)
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: read provider keys: %w", err)
	}
	for keyRows.Next() {
		var row KeyRow
		if err := keyRows.Scan(&row.ID, &row.ProviderID, &row.OwnerID, &row.Name, &row.Enabled,
			&row.AuthScheme, &row.AuthHeaderName, &row.KeyEnv, &row.KeyRef,
			&row.SecretEnc, &row.SecretNonce, &row.SecretKVer, &row.SecretLast4); err != nil {
			keyRows.Close()
			return Rows{}, 0, fmt.Errorf("providerstore: scan provider key: %w", err)
		}
		rows.Keys = append(rows.Keys, row)
	}
	if err := keyRows.Err(); err != nil {
		keyRows.Close()
		return Rows{}, 0, fmt.Errorf("providerstore: read provider keys: %w", err)
	}
	keyRows.Close()

	modelRows, err := tx.Query(ctx, keyModelsQuery)
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: read key models: %w", err)
	}
	for modelRows.Next() {
		var row KeyModelRow
		if err := modelRows.Scan(&row.KeyID, &row.ProviderID, &row.OwnerID, &row.Alias,
			&row.UpstreamModel, &row.Hidden, &row.Fallbacks, &row.FallbackStatuses,
			&row.AllowShadow, &row.LegacyPosition); err != nil {
			modelRows.Close()
			return Rows{}, 0, fmt.Errorf("providerstore: scan key model: %w", err)
		}
		rows.KeyModels = append(rows.KeyModels, row)
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return Rows{}, 0, fmt.Errorf("providerstore: read key models: %w", err)
	}
	modelRows.Close()

	stateRows, err := tx.Query(ctx, modelStateQuery)
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: read model state: %w", err)
	}
	for stateRows.Next() {
		var row ModelStateRow
		if err := stateRows.Scan(&row.ModelID, &row.Hidden, &row.Deleted); err != nil {
			stateRows.Close()
			return Rows{}, 0, fmt.Errorf("providerstore: scan model state: %w", err)
		}
		rows.ModelState = append(rows.ModelState, row)
	}
	if err := stateRows.Err(); err != nil {
		stateRows.Close()
		return Rows{}, 0, fmt.Errorf("providerstore: read model state: %w", err)
	}
	stateRows.Close()

	settingRows, err := tx.Query(ctx, settingsQuery)
	if err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: read settings: %w", err)
	}
	for settingRows.Next() {
		var row SettingRow
		if err := settingRows.Scan(&row.Key, &row.Value); err != nil {
			settingRows.Close()
			return Rows{}, 0, fmt.Errorf("providerstore: scan setting: %w", err)
		}
		rows.Settings = append(rows.Settings, row)
	}
	if err := settingRows.Err(); err != nil {
		settingRows.Close()
		return Rows{}, 0, fmt.Errorf("providerstore: read settings: %w", err)
	}
	settingRows.Close()

	if err := tx.Commit(ctx); err != nil {
		return Rows{}, 0, fmt.Errorf("providerstore: commit snapshot transaction: %w", err)
	}
	return rows, version, nil
}

// Version reads config_state.version outside the snapshot transaction.
func (s *DBRowSource) Version(ctx context.Context) (int64, error) {
	if s == nil || s.Pool == nil {
		return 0, errors.New("providerstore: nil database pool")
	}
	var version int64
	if err := s.Pool.QueryRow(ctx, versionQuery).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("providerstore: config_state row is missing")
		}
		return 0, fmt.Errorf("providerstore: read config version: %w", err)
	}
	return version, nil
}

// Watcher keeps a Store in sync through LISTEN gnvipi_config plus a 30-second
// polling fallback. It reconnects with exponential backoff and reloads on
// reconnect. Reload failures keep the last-known-good snapshot.
type Watcher struct {
	Store          *Store
	Pool           *pgxpool.Pool
	PollInterval   time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	OnError        func(error)
	// OnHealthy runs after a successful connect/reload, clearing the
	// "not refreshing" signal an earlier OnError reported.
	OnHealthy func()
}

// Run blocks until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) error {
	if w == nil || w.Store == nil || w.Pool == nil {
		return errors.New("providerstore: watcher requires a store and pool")
	}
	poll := w.PollInterval
	if poll <= 0 {
		poll = 30 * time.Second
	}
	initial := w.InitialBackoff
	if initial <= 0 {
		initial = time.Second
	}
	maxBackoff := w.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	backoff := initial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := w.connect(ctx)
		if err != nil {
			w.report(err)
			if waitErr := sleepContext(ctx, backoff); waitErr != nil {
				return waitErr
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		backoff = initial
		// Reconnect is a possible lost-notification window: reload immediately.
		w.reload(ctx)
		listenErr := w.listen(ctx, conn, poll)
		// listen only returns after the notification read has returned, so
		// closing the connection here cannot race WaitForNotification's
		// access to pgx's connection state.
		_ = conn.Close(context.Background()) //nolint:errcheck // best-effort cleanup
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if listenErr != nil {
			w.report(listenErr)
		}
		if waitErr := sleepContext(ctx, backoff); waitErr != nil {
			return waitErr
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (w *Watcher) connect(ctx context.Context) (*pgx.Conn, error) {
	config := w.Pool.Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("providerstore: listen connection: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN gnvipi_config"); err != nil {
		_ = conn.Close(context.Background()) //nolint:errcheck // best-effort cleanup/best-effort parse
		return nil, fmt.Errorf("providerstore: LISTEN gnvipi_config: %w", err)
	}
	return conn, nil
}

func (w *Watcher) listen(ctx context.Context, conn *pgx.Conn, poll time.Duration) error {
	notify := make(chan struct{}, 1)
	errorsCh := make(chan error, 1)
	listenCtx, cancel := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			if _, err := conn.WaitForNotification(listenCtx); err != nil {
				errorsCh <- err
				return
			}
			select {
			case notify <- struct{}{}:
			default:
			}
		}
	}()
	// The reader owns the connection while it is inside WaitForNotification:
	// pgx mutates connection state in both WaitForNotification and Close, so
	// closing the connection before the reader has returned is a data race.
	// Cancel the read, wait for it to finish, and only then let the caller
	// close the connection.
	defer func() {
		cancel()
		<-readerDone
	}()

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errorsCh:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("providerstore: notification connection lost: %w", err)
		case <-notify:
			w.reload(ctx)
		case <-ticker.C:
			// Retry after a failed reload even when the version is unchanged,
			// so one transient error does not stick until the next write.
			reload, err := w.Store.pollReload(ctx)
			if err != nil {
				return fmt.Errorf("providerstore: poll config version: %w", err)
			}
			if reload {
				w.reload(ctx)
			}
		}
	}
}

// reload publishes the current configuration. A failure keeps the last good
// snapshot and is reported; success clears the watcher's error signal.
func (w *Watcher) reload(ctx context.Context) {
	if err := w.Store.Reload(ctx); err != nil {
		w.report(err)
		return
	}
	if w.OnHealthy != nil {
		w.OnHealthy()
	}
}

func (w *Watcher) report(err error) {
	if err != nil && w.OnError != nil {
		w.OnError(err)
	}
}

func nextBackoff(current, limit time.Duration) time.Duration {
	next := current * 2
	if next > limit {
		return limit
	}
	return next
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
