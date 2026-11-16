package db

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/migrations"
)

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
		want   string
	}{
		{
			name:   "url password",
			in:     "postgres://gnvipi:hunter2@db.example.com:5432/gnvipi?sslmode=verify-full",
			secret: "hunter2",
			want:   "postgres://gnvipi:xxxxx@db.example.com:5432/gnvipi?sslmode=verify-full",
		},
		{
			name:   "url percent-encoded password",
			in:     "postgresql://u:p%40ss@127.0.0.1:5432/x",
			secret: "p%40ss",
			want:   "postgresql://u:xxxxx@127.0.0.1:5432/x",
		},
		{
			name:   "password query parameter",
			in:     "postgres://u@db.example.com/x?password=s3cret&sslmode=verify-full",
			secret: "s3cret",
			want:   "postgres://u@db.example.com/x?password=xxxxx&sslmode=verify-full",
		},
		{
			name:   "keyword dsn",
			in:     "host=db user=u password=s3cret dbname=x",
			secret: "s3cret",
			want:   "host=db user=u password=xxxxx dbname=x",
		},
		{
			name:   "no password is unchanged",
			in:     "postgres://u@localhost:5432/x?sslmode=disable",
			secret: "",
			want:   "postgres://u@localhost:5432/x?sslmode=disable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactDSN(tc.in)
			if tc.secret != "" && strings.Contains(got, tc.secret) {
				t.Fatalf("redacted DSN still contains the secret: %q", got)
			}
			if got != tc.want {
				t.Fatalf("RedactDSN() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	dsn := "postgres://u:hunter2@localhost:5432/x"
	err := redactError(errors.New("dial "+dsn+": password hunter2 rejected"), dsn)
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), dsn) {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestLoadConfigFromRequiresDatabaseURL(t *testing.T) {
	if _, err := LoadConfigFrom(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("expected an error when GNVIPI_DATABASE_URL is absent")
	}
}

func TestLoadConfigFromHasNoDefaultPassword(t *testing.T) {
	env := map[string]string{
		"GNVIPI_DATABASE_URL": "postgres://gnvipi@localhost:5432/gnvipi?sslmode=disable",
	}
	cfg, err := LoadConfigFrom(mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	// The DSN must be the exact environment value: no password is invented.
	if cfg.DatabaseURL != env["GNVIPI_DATABASE_URL"] {
		t.Fatalf("DatabaseURL = %q, want the environment value verbatim", cfg.DatabaseURL)
	}
	if cfg.MaxConns != DefaultMaxConns || cfg.MinConns != DefaultMinConns {
		t.Fatalf("pool defaults = %d/%d, want %d/%d",
			cfg.MaxConns, cfg.MinConns, DefaultMaxConns, DefaultMinConns)
	}
	if cfg.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, DefaultConnectTimeout)
	}
}

func TestLoadConfigFromProductionDoesNotRequireMigrationURL(t *testing.T) {
	env := map[string]string{
		"GNVIPI_DATABASE_URL": "postgres://runtime@localhost:5432/x",
		"GNVIPI_ENV":          "production",
	}
	if _, err := LoadConfigFrom(mapLookup(env)); err != nil {
		t.Fatalf("production runtime config required the migration credential: %v", err)
	}
}

func TestLoadMigrationURLFromPolicy(t *testing.T) {
	const (
		runtimeDSN   = "postgres://runtime@localhost:5432/runtime?sslmode=disable"
		migrationDSN = "postgres://migrator@localhost:5432/migrations?sslmode=disable"
	)
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{
			name: "production migration only",
			env: map[string]string{
				"GNVIPI_MIGRATION_DATABASE_URL": migrationDSN,
				"GNVIPI_ENV":                    "production",
			},
			want: migrationDSN,
		},
		{
			name: "development runtime fallback",
			env: map[string]string{
				"GNVIPI_DATABASE_URL": runtimeDSN,
				"GNVIPI_ENV":          "development",
			},
			want: runtimeDSN,
		},
		{
			name: "production runtime fallback refused",
			env: map[string]string{
				"GNVIPI_DATABASE_URL": runtimeDSN,
				"GNVIPI_ENV":          "production",
			},
			wantErr: "GNVIPI_MIGRATION_DATABASE_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadMigrationURLFrom(mapLookup(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadMigrationURLFrom() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("LoadMigrationURLFrom() = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestLoadMaintenanceURLFromPolicy(t *testing.T) {
	const (
		runtimeDSN     = "postgres://runtime@localhost:5432/runtime?sslmode=disable"
		maintenanceDSN = "postgres://retention@localhost:5432/runtime?sslmode=disable"
	)
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{
			name: "unset disables audit pruning",
			env:  map[string]string{"GNVIPI_DATABASE_URL": runtimeDSN},
			want: "",
		},
		{
			name: "explicit maintenance role",
			env: map[string]string{
				"GNVIPI_DATABASE_URL":             runtimeDSN,
				"GNVIPI_MAINTENANCE_DATABASE_URL": maintenanceDSN,
			},
			want: maintenanceDSN,
		},
		{
			// The application role is denied DELETE on audit_log, so silently
			// reusing its DSN would turn a misconfiguration into a nightly
			// permission error.
			name: "never falls back to the runtime role",
			env: map[string]string{
				"GNVIPI_DATABASE_URL":             runtimeDSN,
				"GNVIPI_MAINTENANCE_DATABASE_URL": " ",
			},
			want: "",
		},
		{
			name: "remote host without verify-full refused",
			env: map[string]string{
				"GNVIPI_MAINTENANCE_DATABASE_URL": "postgres://retention@db.example.com:5432/gnvipi?sslmode=require",
			},
			wantErr: "GNVIPI_MAINTENANCE_DATABASE_URL",
		},
		{
			name: "malformed DSN refused",
			env: map[string]string{
				"GNVIPI_MAINTENANCE_DATABASE_URL": "mysql://retention@localhost/x",
			},
			wantErr: "GNVIPI_MAINTENANCE_DATABASE_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadMaintenanceURLFrom(mapLookup(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadMaintenanceURLFrom() error = %v, want %q", err, tc.wantErr)
				}
				if got != "" {
					t.Fatalf("LoadMaintenanceURLFrom() returned %q with an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("LoadMaintenanceURLFrom() = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestLoadConfigFromOverrides(t *testing.T) {
	env := map[string]string{
		"GNVIPI_DATABASE_URL":       "postgres://u@localhost:5432/x",
		"GNVIPI_DB_MAX_CONNS":       "25",
		"GNVIPI_DB_MIN_CONNS":       "2",
		"GNVIPI_DB_CONNECT_TIMEOUT": "1500ms",
	}
	cfg, err := LoadConfigFrom(mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 25 || cfg.MinConns != 2 || cfg.ConnectTimeout != 1500*time.Millisecond {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigFromRejectsBadNumbers(t *testing.T) {
	for _, key := range []string{"GNVIPI_DB_MAX_CONNS", "GNVIPI_DB_MIN_CONNS", "GNVIPI_DB_CONNECT_TIMEOUT"} {
		env := map[string]string{
			"GNVIPI_DATABASE_URL": "postgres://u@localhost:5432/x",
			key:                   "not-a-number",
		}
		if _, err := LoadConfigFrom(mapLookup(env)); err == nil {
			t.Fatalf("expected %s to be validated", key)
		}
	}
}

func TestValidateDSNTLSRule(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"local disable", "postgres://u@localhost:5432/x?sslmode=disable", false},
		{"loopback disable", "postgres://u@127.0.0.1:5432/x?sslmode=disable", false},
		{"ipv6 loopback disable", "postgres://u@[::1]:5432/x?sslmode=disable", false},
		{"unix socket", "postgres://u@/x?host=/var/run/postgresql", false},
		{"unix socket disable", "postgres://u@/x?host=/var/run/postgresql&sslmode=disable", false},
		{"unix socket multi", "postgres://u@/x?host=/var/run/postgresql,/tmp", false},
		{"empty host no socket", "postgres://u@/x", true},
		{"empty host remote query", "postgres://u@/x?host=db.example.com&sslmode=disable", true},
		{"unix socket mixed remote", "postgres://u@/x?host=/var/run/postgresql,db.example.com&sslmode=disable", true},
		{"repeated host remote override", "postgres://u@/x?host=/var/run/postgresql&host=db.example.test&sslmode=disable", true},
		{"repeated host unix sockets", "postgres://u@/x?host=/var/run/postgresql&host=/tmp&sslmode=disable", true},
		{"local host with remote query", "postgres://u@localhost:5432/x?host=db.example.com&sslmode=disable", true},
		{"remote host with socket query", "postgres://u@db.example.com:5432/x?host=/var/run/postgresql&sslmode=disable", true},
		{"remote verify-full", "postgres://u@db.example.com:5432/x?sslmode=verify-full", false},
		{"remote disable", "postgres://u@db.example.com:5432/x?sslmode=disable", true},
		{"remote require", "postgres://u@db.example.com:5432/x?sslmode=require", true},
		{"remote no sslmode", "postgres://u@db.example.com:5432/x", true},
		{"repeated sslmode verify-full then disable", "postgres://u@db.example.com:5432/x?sslmode=verify-full&sslmode=disable", true},
		{"repeated sslmode disable then verify-full", "postgres://u@db.example.com:5432/x?sslmode=disable&sslmode=verify-full", true},
		{"unix socket repeated sslmode", "postgres://u@/x?host=/var/run/postgresql&sslmode=verify-full&sslmode=disable", true},
		{"not a url", "host=localhost user=u", true},
		{"wrong scheme", "mysql://u@localhost/x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDSN(tc.dsn)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateDSN(%q) = nil, want error", tc.dsn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateDSN(%q) = %v", tc.dsn, err)
			}
		})
	}
}

func TestValidateDSNDoesNotLeakPassword(t *testing.T) {
	err := ValidateDSN("postgres://u:hunter2@db.example.com/x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("validation error leaked the password: %v", err)
	}
}

func TestCheckVersion(t *testing.T) {
	if err := CheckVersion(2, false, 2); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
	cases := []struct {
		name     string
		applied  uint
		dirty    bool
		expected uint
		want     error
	}{
		{"dirty", 2, true, 2, ErrSchemaDirty},
		{"too new", 3, false, 2, ErrSchemaTooNew},
		{"too old", 1, false, 2, ErrSchemaTooOld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckVersion(tc.applied, tc.dirty, tc.expected)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckVersion() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSchemaGuardErrorsTellOperatorHowToRecover(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "unmigrated", err: ErrSchemaNotMigrated, want: "serve migrate"},
		{name: "dirty", err: CheckVersion(2, true, 2), want: "repair"},
		{name: "older", err: CheckVersion(1, false, 2), want: "serve migrate"},
		{name: "newer", err: CheckVersion(3, false, 2), want: "upgrade the binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.err.Error(), tc.want) {
				t.Fatalf("schema guard error = %q, want recovery instruction containing %q", tc.err, tc.want)
			}
		})
	}
}

func TestLatestVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.up.sql":   {Data: []byte("select 1;")},
		"0001_init.down.sql": {Data: []byte("select 1;")},
		"0002_more.up.sql":   {Data: []byte("select 1;")},
		"README.md":          {Data: []byte("not a migration")},
	}
	got, err := LatestVersion(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("LatestVersion() = %d, want 2", got)
	}
	if _, err := LatestVersion(fstest.MapFS{"README.md": {Data: []byte("x")}}); err == nil {
		t.Fatal("expected an error when no up migrations are embedded")
	}
}

func TestAutoMigrateAllowed(t *testing.T) {
	if !AutoMigrateAllowed("") || !AutoMigrateAllowed("development") {
		t.Fatal("auto-migrate must be allowed outside production")
	}
	if AutoMigrateAllowed("production") || AutoMigrateAllowed(" PRODUCTION ") {
		t.Fatal("auto-migrate must be refused in production")
	}
}

func TestComposeDefaultDSNPassesValidation(t *testing.T) {
	composePath := filepath.Join("..", "..", "docker-compose.yml")
	raw, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	re := regexp.MustCompile(`(?m)^\s*GNVIPI_DATABASE_URL:\s*\$\{GNVIPI_DATABASE_URL:-([^}\r\n]+)\}`)
	m := re.FindSubmatch(raw)
	if len(m) != 2 {
		t.Fatalf("default GNVIPI_DATABASE_URL not found in %s", composePath)
	}
	dsn := strings.TrimSpace(string(m[1]))

	if err := ValidateDSN(dsn); err != nil {
		t.Fatalf("Compose default DSN rejected by ValidateDSN: %v", err)
	}
	env := mapLookup(map[string]string{"GNVIPI_DATABASE_URL": dsn})
	cfg, err := LoadConfigFrom(env)
	if err != nil {
		t.Fatalf("Compose default DSN rejected by LoadConfigFrom: %v", err)
	}
	if _, err := LoadMigrationURLFrom(env); err != nil {
		t.Fatalf("Compose default DSN rejected by LoadMigrationURLFrom: %v", err)
	}
	if _, err := pgxpool.ParseConfig(cfg.DatabaseURL); err != nil {
		t.Fatalf("Compose default DSN rejected by pgxpool.ParseConfig: %v", err)
	}
}

func TestGuardRootPromotionMigrationEmbedded(t *testing.T) {
	latest, err := LatestVersion(migrations.FS)
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if latest < 8 {
		t.Fatalf("LatestVersion = %d, want >= 8", latest)
	}
	up, err := migrations.FS.ReadFile("0008_guard_root_promotion.up.sql")
	if err != nil {
		t.Fatalf("read 0008 up migration: %v", err)
	}
	if !strings.Contains(string(up), "NEW.role NOT IN ('superuser', 'root')") {
		t.Fatalf("0008 up migration missing root-preserving NEW.role guard")
	}
	if _, err := migrations.FS.ReadFile("0008_guard_root_promotion.down.sql"); err != nil {
		t.Fatalf("read 0008 down migration: %v", err)
	}
}

func mapLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}
