package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	defer lockConn.Close(context.Background()) //nolint:errcheck // best-effort cleanup/best-effort parse
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('gnvipi:migrations'))`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", redactError(err, databaseURL))
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), //nolint:errcheck // best-effort cleanup/best-effort parse
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

	driver, err := pgx5.WithInstance(sqlDB, &pgx5.Config{})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", redactError(err, databaseURL))
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", redactError(err, databaseURL))
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", redactError(err, databaseURL))
	}
	return nil
}
