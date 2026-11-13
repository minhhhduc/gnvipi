package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/db"
	"github.com/minhhhduc/gnvipi/migrations"
)

// databaseEnv names the DSN that switches the gateway into database mode. When
// it is absent or empty the gateway keeps the file-backed admin surface and the
// database-backed admin API is never registered.
const databaseEnv = "GNVIPI_DATABASE_URL"

// appDB is the process-wide PostgreSQL handle. A nil *appDB means file-backed
// mode; callers must treat it as "no database-backed admin surface".
type appDB struct {
	pool *pgxpool.Pool
	auth *authState
}

// Close releases the pool. It is safe on the nil handle used by file-backed
// mode so callers never need a nil check.
func (d *appDB) Close() {
	if d == nil || d.pool == nil {
		return
	}
	d.pool.Close()
}

// openAppDB connects when the operator configured a database. A configured but
// unreachable, unmigrated or half-migrated database is a startup error: serving
// credential checks from an unexpected schema must fail closed, never degrade.
func openAppDB(ctx context.Context, autoMigrate bool) (*appDB, error) {
	if strings.TrimSpace(os.Getenv(databaseEnv)) == "" {
		return nil, nil
	}
	cfg, err := db.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}
	if autoMigrate {
		if !db.AutoMigrateAllowed(cfg.Env) {
			return nil, fmt.Errorf("-auto-migrate is not allowed when GNVIPI_ENV=%s; run the migration command instead", cfg.Env)
		}
		if err := db.Migrate(ctx, cfg.MigrationURL); err != nil {
			return nil, fmt.Errorf("database migration: %w", err)
		}
		log.Print("database: migrations applied")
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database connect: %w", err)
	}
	latest, err := db.LatestVersion(migrations.FS)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := db.CheckSchema(ctx, pool, latest); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database schema: %w (development: start with -auto-migrate)", err)
	}
	log.Printf("database: ready (schema v%d)", latest)
	return &appDB{pool: pool, auth: newAuthState(pool)}, nil
}