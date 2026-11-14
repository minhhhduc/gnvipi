package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
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
	// authRetentionInterval is the daily phase-2 retention cadence.
	authRetentionInterval = 24 * time.Hour
	// auditRetentionDaysEnv overrides the audit retention window in days.
	auditRetentionDaysEnv = "GNVIPI_AUDIT_RETENTION_DAYS"
	// maxAuditRetentionDays bounds the configured window so the duration cannot
	// overflow and an obvious typo fails at startup instead of later.
	maxAuditRetentionDays = 36500
)

// loadRetentionConfig resolves the retention windows. Only the audit window is
// operator-configurable; the session and lockout windows are design constants.
func loadRetentionConfig(sessions auth.SessionConfig) (auth.RetentionConfig, error) {
	return loadRetentionConfigFrom(os.LookupEnv, sessions)
}

func loadRetentionConfigFrom(lookup func(string) (string, bool), sessions auth.SessionConfig) (auth.RetentionConfig, error) {
	cfg := auth.DefaultRetentionConfig()
	cfg.Sessions = sessions
	value, _ := lookup(auditRetentionDaysEnv)
	raw := strings.TrimSpace(value)
	if raw == "" {
		return cfg, nil
	}
	days, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || days == 0 || days > maxAuditRetentionDays {
		return cfg, fmt.Errorf("%s must be a whole number of days between 1 and %d", auditRetentionDaysEnv, maxAuditRetentionDays)
	}
	cfg.Audit = time.Duration(days) * 24 * time.Hour
	return cfg, nil
}

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
	pool        *pgxpool.Pool
	maintenance *pgxpool.Pool
	retention   auth.RetentionConfig
	auth        *authState

	retentionCancel context.CancelFunc
	retentionDone   <-chan struct{}
}

// Close releases the pools and stops the retention loop. It is safe on the nil
// handle used by file-backed mode so callers never need a nil check.
func (d *appDB) Close() {
	if d == nil {
		return
	}
	if d.retentionCancel != nil {
		d.retentionCancel()
	}
	if d.retentionDone != nil {
		<-d.retentionDone
	}
	if d.pool != nil {
		d.pool.Close()
	}
	if d.maintenance != nil {
		d.maintenance.Close()
	}
}

// runRetention runs one retention pass. A pass owned by another gateway process
// is not an error, and the caller only logs failures: retention is maintenance,
// never a prerequisite for serving.
func (d *appDB) runRetention(ctx context.Context) error {
	var maintenance auth.DB
	if d.maintenance != nil {
		maintenance = d.maintenance
	}
	_, err := auth.RunRetention(ctx, d.pool, maintenance, time.Now(), d.retention)
	if errors.Is(err, auth.ErrRetentionBusy) {
		return nil
	}
	return err
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
	retention, err := loadRetentionConfig(state.sessions)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("retention configuration: %w", err)
	}
	maintenance := openMaintenancePool(ctx, cfg)
	d := &appDB{pool: pool, maintenance: maintenance, retention: retention, auth: state}

	if err := d.runRetention(ctx); err != nil && ctx.Err() == nil {
		log.Printf("database: auth retention failed: %v", err)
	}
	retentionCtx, cancelRetention := context.WithCancel(ctx)
	retentionDone := make(chan struct{})
	go runRetentionLoop(retentionCtx, d, retentionDone)
	d.retentionCancel, d.retentionDone = cancelRetention, retentionDone
	log.Printf("database: ready (schema v%d)", latest)
	return d, nil
}

// openMaintenancePool connects the optional phase-2 retention role. Audit rows
// are append-only for the application role, so without this pool audit pruning
// is skipped. A configured but unusable pool is logged and disabled rather than
// failing startup: retention never gates serving admin requests.
func openMaintenancePool(ctx context.Context, cfg db.Config) *pgxpool.Pool {
	url, err := db.LoadMaintenanceURL()
	if err != nil {
		log.Printf("database: maintenance role disabled: %v", err)
		return nil
	}
	if url == "" {
		log.Print("database: no maintenance role configured; audit retention is disabled")
		return nil
	}
	maintenanceCfg := cfg
	maintenanceCfg.DatabaseURL = url
	maintenanceCfg.MaxConns = 1
	maintenanceCfg.MinConns = 0
	pool, err := db.Open(ctx, maintenanceCfg)
	if err != nil {
		log.Printf("database: maintenance role disabled: %v", err)
		return nil
	}
	return pool
}

func runRetentionLoop(ctx context.Context, d *appDB, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(authRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.runRetention(ctx); err != nil && ctx.Err() == nil {
				log.Printf("database: auth retention failed: %v", err)
			}
		}
	}
}
