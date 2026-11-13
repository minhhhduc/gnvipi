package main

import (
	"context"
	"strings"
	"testing"
)

func TestOpenAppDBRequiresDSNInProduction(t *testing.T) {
	t.Setenv(databaseEnv, "")
	t.Setenv("GNVIPI_ENV", "production")

	state, err := openAppDB(t.Context(), false)
	if state != nil {
		state.Close()
		t.Fatal("production without a DSN returned database state")
	}
	if err == nil || !strings.Contains(err.Error(), databaseEnv) {
		t.Fatalf("production without a DSN error = %v, want actionable %s error", err, databaseEnv)
	}
}

func TestOpenAppDBKeepsDevelopmentFileModeWithoutDSN(t *testing.T) {
	t.Setenv(databaseEnv, "")
	t.Setenv("GNVIPI_ENV", "development")

	state, err := openAppDB(t.Context(), false)
	if err != nil || state != nil {
		t.Fatalf("development without a DSN = state %v err %v, want nil nil", state, err)
	}
}

func TestOpenAppDBRefusesAutoMigrateInProduction(t *testing.T) {
	dsn := "postgres://runtime@127.0.0.1:1/gnvipi?sslmode=disable"
	t.Setenv(databaseEnv, dsn)
	t.Setenv("GNVIPI_MIGRATION_DATABASE_URL", "postgres://migrator@127.0.0.1:1/gnvipi?sslmode=disable")
	t.Setenv("GNVIPI_ENV", "production")

	state, err := openAppDB(t.Context(), true)
	if state != nil {
		state.Close()
		t.Fatal("production --auto-migrate returned database state")
	}
	if err == nil || !strings.Contains(err.Error(), "--auto-migrate") || !strings.Contains(err.Error(), "production") {
		t.Fatalf("production --auto-migrate error = %v, want an actionable production refusal", err)
	}
}

func TestRunMigrateCommandUsesMigrationDatabaseURL(t *testing.T) {
	const (
		runtimeDSN   = "postgres://runtime@localhost:5432/runtime?sslmode=disable"
		migrationDSN = "postgres://migrator@localhost:5432/migrations?sslmode=disable"
	)
	env := map[string]string{
		databaseEnv:                    runtimeDSN,
		"GNVIPI_MIGRATION_DATABASE_URL": migrationDSN,
		"GNVIPI_ENV":                   "production",
	}
	lookup := func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
	var usedDSN string
	runner := func(_ context.Context, dsn string) error {
		usedDSN = dsn
		return nil
	}

	if err := runMigrateCommand(t.Context(), nil, lookup, runner); err != nil {
		t.Fatalf("run migrate: %v", err)
	}
	if usedDSN != migrationDSN {
		t.Fatalf("migration ran with %q, want GNVIPI_MIGRATION_DATABASE_URL %q", usedDSN, migrationDSN)
	}
}
