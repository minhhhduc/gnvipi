package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgx5 "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minhhhduc/gnvipi/migrations"
)

// Migrate applies every embedded migration under
// pg_advisory_lock(hashtext('gnvipi:migrations')) on the migration DSN. A
// concurrent migration waits for the lock (spec §5). Each migration file runs
// as a single implicit PostgreSQL transaction.
func Migrate(ctx context.Context, databaseURL string) error {
	if err := ValidateDSN(databaseURL); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, MigrationTimeout)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect for migration: %w", redactError(err, databaseURL))
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), SchemaCheckTimeout)
		defer closeCancel()
		_ = lockConn.Close(closeCtx) //nolint:errcheck // bounded best-effort cleanup
	}()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('gnvipi:migrations'))`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", redactError(err, databaseURL))
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), SchemaCheckTimeout)
		defer unlockCancel()
		_, _ = lockConn.Exec(unlockCtx, //nolint:errcheck // bounded best-effort cleanup
			`SELECT pg_advisory_unlock(hashtext('gnvipi:migrations'))`)
	}()

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration database: %w", redactError(err, databaseURL))
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort cleanup/best-effort parse
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	var migrationPID int32
	if err := sqlDB.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&migrationPID); err != nil {
		return fmt.Errorf("identify migration connection: %w", redactError(err, databaseURL))
	}
	statementTimeout := MigrationTimeout
	if deadline, ok := ctx.Deadline(); ok {
		statementTimeout = time.Until(deadline)
		if statementTimeout <= 0 {
			return ctx.Err()
		}
	}
	cancelDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		terminateCtx, terminateCancel := context.WithTimeout(context.Background(), SchemaCheckTimeout)
		defer terminateCancel()
		_, _ = lockConn.Exec(terminateCtx, `SELECT pg_terminate_backend($1)`, migrationPID) //nolint:errcheck // best-effort cancellation
	})
	defer func() {
		if !stopCancellation() {
			<-cancelDone
		}
	}()

	driver, err := pgx5.WithInstance(sqlDB, &pgx5.Config{StatementTimeout: statementTimeout})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("create migration driver: %w", ctx.Err())
		}
		return fmt.Errorf("create migration driver: %w", redactError(err, databaseURL))
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", redactError(err, databaseURL))
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		if ctx.Err() != nil {
			return fmt.Errorf("apply migrations: %w", ctx.Err())
		}
		return fmt.Errorf("apply migrations: %w", redactError(err, databaseURL))
	}
	return nil
}
