package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Schema guard errors. Callers must fail closed on any of them.
var (
	ErrSchemaNotMigrated = errors.New("database schema has no applied migration; run the migration command")
	ErrSchemaDirty       = errors.New("database schema is dirty; repair the failed migration before starting")
	ErrSchemaTooNew      = errors.New("database schema is newer than this binary; upgrade the binary")
	ErrSchemaTooOld      = errors.New("database schema is older than this binary; run the migration command")
)

// Querier is satisfied by *pgxpool.Pool and *pgx.Conn.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var migrationFileRE = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// LatestVersion returns the highest embedded up-migration version.
func LatestVersion(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var latest uint
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse migration version %q: %w", entry.Name(), err)
		}
		if !found || uint(n) > latest {
			latest = uint(n)
			found = true
		}
	}
	if !found {
		return 0, errors.New("no embedded up migrations")
	}
	return latest, nil
}

// CheckVersion compares the applied schema version with the binary's expected
// version. It is pure so the guard can be unit-tested without PostgreSQL.
func CheckVersion(applied uint, dirty bool, expected uint) error {
	switch {
	case dirty:
		return fmt.Errorf("%w: applied version %d", ErrSchemaDirty, applied)
	case applied > expected:
		return fmt.Errorf("%w: database version %d, binary expects %d", ErrSchemaTooNew, applied, expected)
	case applied < expected:
		return fmt.Errorf("%w: database version %d, binary expects %d", ErrSchemaTooOld, applied, expected)
	}
	return nil
}

// CheckSchema reads the golang-migrate version table and fails closed unless
// the database matches expected exactly. It is bounded by SchemaCheckTimeout.
func CheckSchema(ctx context.Context, q Querier, expected uint) error {
	ctx, cancel := context.WithTimeout(ctx, SchemaCheckTimeout)
	defer cancel()

	var applied int64
	var dirty bool
	err := q.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&applied, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSchemaNotMigrated
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return ErrSchemaNotMigrated
		}
		return fmt.Errorf("read schema version: %w", err)
	}
	return CheckVersion(uint(applied), dirty, expected)
}
