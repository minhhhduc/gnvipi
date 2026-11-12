package providerstore

import (
	"context"
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

// TestDBRowSourceLoadConfig exercises the real repeatable-read transaction
// against a migrated database. It is skipped unless GNVIPI_TEST_DATABASE_URL is
// set; isolation and locking are never mocked.
func TestDBRowSourceLoadConfig(t *testing.T) {
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when CI=true")
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
