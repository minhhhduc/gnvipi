package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/db"
	"github.com/minhhhduc/gnvipi/migrations"
)

// databaseEnv names the DSN that switches the gateway into database mode. When
// it is absent or empty the gateway keeps the file-backed admin surface and the
// database-backed admin API is never registered.
const databaseEnv = "GNVIPI_DATABASE_URL"

const (
	authCleanupInterval   = 6 * time.Hour
	loginAttemptRetention = 30 * 24 * time.Hour
)

func runMigrateCommand(
	ctx context.Context,
	args []string,
	lookup func(string) (string, bool),
	run func(context.Context, string) error,
) error {
	if len(args) != 0 {
		return fmt.Errorf("serve migrate accepts no arguments (got %d)", len(args))
	}
	migrationURL, err := db.LoadMigrationURLFrom(lookup)
	if err != nil {
		return fmt.Errorf("migration configuration: %w", err)
	}
	if err := run(ctx, migrationURL); err != nil {
		return fmt.Errorf("database migration: %w", err)
	}
	log.Print("database: migrations applied")
	return nil
}

func databaseSchemaError(err error) error {
	return fmt.Errorf("database schema: %w", err)
}

// appDB is the process-wide PostgreSQL handle. A nil *appDB means file-backed
// mode; callers must treat it as "no database-backed admin surface".
type appDB struct {
	pool          *pgxpool.Pool
	auth          *authState
	cleanupCancel context.CancelFunc
	cleanupDone   <-chan struct{}
}

// Close releases the pool. It is safe on the nil handle used by file-backed
// mode so callers never need a nil check.
func (d *appDB) Close() {
	if d == nil {
		return
	}
	if d.cleanupCancel != nil {
		d.cleanupCancel()
	}
	if d.cleanupDone != nil {
		<-d.cleanupDone
	}
	if d.pool != nil {
		d.pool.Close()
	}
}

// openAppDB connects when the operator configured a database. A configured but
// unreachable, unmigrated or half-migrated database is a startup error: serving
// credential checks from an unexpected schema must fail closed, never degrade.
func openAppDB(ctx context.Context, autoMigrate bool) (*appDB, error) {
	if strings.TrimSpace(os.Getenv(databaseEnv)) == "" {
		if db.IsProduction(strings.TrimSpace(os.Getenv("GNVIPI_ENV"))) {
			return nil, fmt.Errorf("%s is required when GNVIPI_ENV=production", databaseEnv)
		}
		return nil, nil
	}
	cfg, err := db.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}
	password, err := loadPasswordHashConfig(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("password hashing configuration: %w", err)
	}
	if autoMigrate {
		if !db.AutoMigrateAllowed(cfg.Env) {
			return nil, fmt.Errorf("--auto-migrate is not allowed when GNVIPI_ENV=%s; run 'serve migrate' instead", cfg.Env)
		}
		log.Print("warning: --auto-migrate is for development only; production must use 'serve migrate'")
		migrationURL, err := db.LoadMigrationURL()
		if err != nil {
			return nil, fmt.Errorf("migration configuration: %w", err)
		}
		if err := db.Migrate(ctx, migrationURL); err != nil {
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
		return nil, databaseSchemaError(err)
	}
	state := newAuthState(pool, &password)
	if err := cleanupAuthData(ctx, state); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database auth cleanup: %w", err)
	}
	cleanupCtx, cancelCleanup := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})
	go runAuthCleanup(cleanupCtx, state, cleanupDone)
	log.Printf("database: ready (schema v%d)", latest)
	return &appDB{pool: pool, auth: state, cleanupCancel: cancelCleanup, cleanupDone: cleanupDone}, nil
}

func cleanupAuthData(ctx context.Context, state *authState) error {
	now := time.Now()
	if _, err := auth.CleanupSessions(ctx, state.pool, now, state.sessions); err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	if _, err := auth.CleanupLoginAttempts(ctx, state.pool, now.Add(-loginAttemptRetention)); err != nil {
		return fmt.Errorf("login attempts: %w", err)
	}
	return nil
}

func runAuthCleanup(ctx context.Context, state *authState, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(authCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cleanupAuthData(ctx, state); err != nil && ctx.Err() == nil {
				log.Printf("database: auth cleanup failed: %v", err)
			}
		}
	}
}
