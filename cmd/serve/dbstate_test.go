package main

import (
	"context"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/db"
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

func TestOpenAppDBRejectsInvalidArgon2ConfigurationBeforeConnecting(t *testing.T) {
	t.Setenv(databaseEnv, "postgres://postgres@127.0.0.1:1/gnvipi?sslmode=disable")
	t.Setenv("GNVIPI_PASSWORD_ARGON2_MEMORY_KIB", "not-a-number")

	state, err := openAppDB(t.Context(), false)
	if state != nil {
		state.Close()
		t.Fatal("invalid password configuration returned database state")
	}
	if err == nil || !strings.Contains(err.Error(), "GNVIPI_PASSWORD_ARGON2_MEMORY_KIB") {
		t.Fatalf("invalid password configuration error = %v, want the invalid variable named", err)
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

func TestDatabaseSchemaErrorDoesNotRecommendAutoMigrate(t *testing.T) {
	for _, cause := range []error{db.ErrSchemaNotMigrated, db.ErrSchemaDirty, db.ErrSchemaTooOld, db.ErrSchemaTooNew} {
		err := databaseSchemaError(cause)
		if !strings.Contains(err.Error(), "database schema") {
			t.Fatalf("schema error = %v, want database schema context", err)
		}
		if strings.Contains(err.Error(), "--auto-migrate") {
			t.Fatalf("schema error recommends auto-migrate for %v: %v", cause, err)
		}
	}
}

func TestRunCLICommandMigrationDSNPolicy(t *testing.T) {
	const (
		runtimeDSN   = "postgres://runtime@localhost:5432/runtime?sslmode=disable"
		migrationDSN = "postgres://migrator@localhost:5432/migrations?sslmode=disable"
	)
	cases := []struct {
		name    string
		env     map[string]string
		wantDSN string
		wantErr string
	}{
		{
			name: "explicit migration DSN in production",
			env: map[string]string{
				"GNVIPI_MIGRATION_DATABASE_URL": migrationDSN,
				"GNVIPI_ENV":                    "production",
			},
			wantDSN: migrationDSN,
		},
		{
			name: "development fallback to runtime DSN",
			env: map[string]string{
				databaseEnv:  runtimeDSN,
				"GNVIPI_ENV": "development",
			},
			wantDSN: runtimeDSN,
		},
		{
			name: "production refuses runtime fallback",
			env: map[string]string{
				databaseEnv:  runtimeDSN,
				"GNVIPI_ENV": "production",
			},
			wantErr: "GNVIPI_MIGRATION_DATABASE_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				value, ok := tc.env[key]
				return value, ok
			}
			calls := 0
			usedDSN := ""
			runner := func(_ context.Context, dsn string) error {
				calls++
				usedDSN = dsn
				return nil
			}

			handled, err := runCLICommand([]string{"migrate"}, lookup, runner, nil)
			if !handled {
				t.Fatal("migrate command was not handled")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("run migrate error = %v, want %q", err, tc.wantErr)
				}
				if calls != 0 {
					t.Fatalf("migration runner called %d times after rejected configuration", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("run migrate: %v", err)
			}
			if calls != 1 || usedDSN != tc.wantDSN {
				t.Fatalf("migration runner = %d calls with %q, want one call with %q", calls, usedDSN, tc.wantDSN)
			}
		})
	}
}

func TestRunCLICommandDispatchesMigrationArguments(t *testing.T) {
	const secret = "postgres://migrator:hunter2@localhost:5432/gnvipi?sslmode=disable"
	calls := 0
	handled, err := runCLICommand([]string{"migrate", secret}, nil, func(context.Context, string) error {
		calls++
		return nil
	}, nil)
	if !handled || err == nil {
		t.Fatalf("migrate with an argument = handled %v err %v, want handled error", handled, err)
	}
	if calls != 0 {
		t.Fatalf("migration runner called %d times after unexpected argument", calls)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("migration dispatch error leaked the argument: %v", err)
	}

	handled, err = runCLICommand([]string{"--addr", ":8080"}, nil, nil, nil)
	if handled || err != nil {
		t.Fatalf("server flags = handled %v err %v, want unhandled nil", handled, err)
	}
}

func TestRunMigrateCommandDoesNotEchoUnexpectedArguments(t *testing.T) {
	const secret = "postgres://migrator:hunter2@localhost:5432/gnvipi?sslmode=disable"
	err := runMigrateCommand(t.Context(), []string{secret}, nil, nil)
	if err == nil {
		t.Fatal("unexpected migration argument was accepted")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("migration argument error leaked the argument: %v", err)
	}
}
