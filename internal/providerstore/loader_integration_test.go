package providerstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(time.Second, 30*time.Second); got != 2*time.Second {
		t.Fatalf("backoff = %v", got)
	}
	if got := nextBackoff(20*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("capped backoff = %v", got)
	}
}

// TestReconnectBackoffResetsOnlyAfterStableConnection covers the accept-then-drop
// loop: a connection that is accepted and immediately lost must keep backing
// off to MaxBackoff instead of resetting to the initial delay every second.
func TestReconnectBackoffResetsOnlyAfterStableConnection(t *testing.T) {
	const initial = time.Second
	const maxBackoff = 30 * time.Second

	if got := reconnectBackoff(4*time.Second, initial, maxBackoff, 10*time.Millisecond); got != 4*time.Second {
		t.Fatalf("immediate-drop backoff = %v, want the current 4s", got)
	}

	backoff := initial
	for i := 0; i < 10 && backoff != maxBackoff; i++ {
		backoff = nextBackoff(reconnectBackoff(backoff, initial, maxBackoff, time.Millisecond), maxBackoff)
	}
	if backoff != maxBackoff {
		t.Fatalf("backoff after repeated immediate drops = %v, want %v", backoff, maxBackoff)
	}

	if got := reconnectBackoff(maxBackoff, initial, maxBackoff, maxBackoff); got != initial {
		t.Fatalf("stable-connection backoff = %v, want the initial %v", got, initial)
	}
}

// TestWatcherReloadFailureIsNotAConnectionError keeps reload failures on the
// Store (where /healthz reads LastError) instead of the watcher's connection
// error slot, which would claim the LISTEN connection is unhealthy.
func TestWatcherReloadFailureIsNotAConnectionError(t *testing.T) {
	source := &stubSource{
		load:    func(context.Context) (Rows, int64, error) { return Rows{}, 0, errors.New("reload failed") },
		version: func(context.Context) (int64, error) { return 1, nil },
	}
	store := NewStore(source, StoreOptions{})

	var reported []error
	watcher := &Watcher{Store: store, OnError: func(err error) { reported = append(reported, err) }}
	watcher.reload(context.Background())

	if len(reported) != 0 {
		t.Fatalf("reload failure reported as a watcher connection error: %v", reported)
	}
	if store.LastError() == nil {
		t.Fatal("reload failure was not recorded on the store")
	}
}

// TestDBRowSourceLoadConfig exercises the real repeatable-read transaction
// against a migrated database. It is skipped unless GNVIPI_TEST_DATABASE_URL is
// set; isolation and locking are never mocked.
func TestDBRowSourceLoadConfig(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var migrated bool
	const check = `SELECT to_regclass('public.config_state') IS NOT NULL
		AND to_regclass('public.providers') IS NOT NULL
		AND to_regclass('public.provider_keys') IS NOT NULL
		AND to_regclass('public.key_models') IS NOT NULL
		AND to_regclass('public.model_state') IS NOT NULL
		AND to_regclass('public.settings') IS NOT NULL`
	if err := pool.QueryRow(ctx, check).Scan(&migrated); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if !migrated {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("provider-key tables are not migrated while GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("provider-key tables are not migrated in GNVIPI_TEST_DATABASE_URL")
	}

	source := NewDBRowSource(pool)
	rows, version, err := source.LoadConfig(ctx)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if version < 0 {
		t.Fatalf("version = %d", version)
	}
	latest, err := source.Version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if latest < version {
		t.Fatalf("version regressed between reads: loaded=%d latest=%d", version, latest)
	}

	// The loaded rows must satisfy the same contracts the snapshot builder
	// enforces. A nil keyring only downgrades encrypted keys to diagnostics.
	if _, err := BuildSnapshot(rows, version, BuildOptions{Now: fixedNow}); err != nil {
		t.Fatalf("snapshot from database rows: %v", err)
	}
}
