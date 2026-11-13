// Package db owns the PostgreSQL connection, embedded migrations and the
// startup schema-version guard for GNVIPI (#185).
//
// The DSN is environment-only. There is no default credential and no DSN is
// ever written verbatim to an error: use RedactDSN when a URL must be shown.
package db

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pool and timeout defaults from spec §5.
const (
	DefaultMaxConns int32 = 10
	DefaultMinConns int32 = 1

	DefaultConnectTimeout    = 5 * time.Second
	AdminStatementTimeout    = 10 * time.Second
	AdminLockTimeout         = 2 * time.Second
	IdleInTransactionTimeout = 30 * time.Second

	MigrationTimeout   = 5 * time.Minute
	SchemaCheckTimeout = 10 * time.Second

	EnvProduction = "production"
)

// Config is the resolved runtime database configuration. DatabaseURL is a
// secret; never format a Config with %v.
type Config struct {
	DatabaseURL string
	Env         string

	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// LoadConfig reads configuration from the process environment.
func LoadConfig() (Config, error) {
	return LoadConfigFrom(os.LookupEnv)
}

// LoadConfigFrom reads runtime configuration from lookup, allowing tests and
// callers to inject an environment. GNVIPI_DATABASE_URL is required; no
// default password is ever synthesized.
func LoadConfigFrom(lookup func(string) (string, bool)) (Config, error) {
	appURL := strings.TrimSpace(envValue(lookup, "GNVIPI_DATABASE_URL"))
	if appURL == "" {
		return Config{}, errors.New("GNVIPI_DATABASE_URL is required")
	}
	if err := ValidateDSN(appURL); err != nil {
		return Config{}, fmt.Errorf("GNVIPI_DATABASE_URL: %w", err)
	}

	env := strings.TrimSpace(envValue(lookup, "GNVIPI_ENV"))

	maxConns, err := envInt32(lookup, "GNVIPI_DB_MAX_CONNS", DefaultMaxConns)
	if err != nil {
		return Config{}, err
	}
	minConns, err := envInt32(lookup, "GNVIPI_DB_MIN_CONNS", DefaultMinConns)
	if err != nil {
		return Config{}, err
	}
	connectTimeout, err := envDuration(lookup, "GNVIPI_DB_CONNECT_TIMEOUT", DefaultConnectTimeout)
	if err != nil {
		return Config{}, err
	}
	if maxConns < 1 {
		return Config{}, errors.New("GNVIPI_DB_MAX_CONNS must be at least 1")
	}
	if minConns < 0 || minConns > maxConns {
		return Config{}, errors.New("GNVIPI_DB_MIN_CONNS must be between 0 and GNVIPI_DB_MAX_CONNS")
	}
	if connectTimeout <= 0 {
		return Config{}, errors.New("GNVIPI_DB_CONNECT_TIMEOUT must be positive")
	}

	return Config{
		DatabaseURL:    appURL,
		Env:            env,
		MaxConns:       maxConns,
		MinConns:       minConns,
		ConnectTimeout: connectTimeout,
	}, nil
}

// LoadMigrationURL reads the DDL credential used by the explicit migration
// command and development auto-migration.
func LoadMigrationURL() (string, error) {
	return LoadMigrationURLFrom(os.LookupEnv)
}

// LoadMigrationURLFrom keeps the DDL credential independent from runtime
// configuration. Development may fall back to the runtime DSN; production may
// not.
func LoadMigrationURLFrom(lookup func(string) (string, bool)) (string, error) {
	migrationURL := strings.TrimSpace(envValue(lookup, "GNVIPI_MIGRATION_DATABASE_URL"))
	if migrationURL != "" {
		if err := ValidateDSN(migrationURL); err != nil {
			return "", fmt.Errorf("GNVIPI_MIGRATION_DATABASE_URL: %w", err)
		}
		return migrationURL, nil
	}
	if IsProduction(envValue(lookup, "GNVIPI_ENV")) {
		return "", errors.New("GNVIPI_MIGRATION_DATABASE_URL is required in production")
	}
	appURL := strings.TrimSpace(envValue(lookup, "GNVIPI_DATABASE_URL"))
	if appURL == "" {
		return "", errors.New("GNVIPI_DATABASE_URL is required when GNVIPI_MIGRATION_DATABASE_URL is unset")
	}
	if err := ValidateDSN(appURL); err != nil {
		return "", fmt.Errorf("GNVIPI_DATABASE_URL: %w", err)
	}
	return appURL, nil
}

// IsProduction reports whether env selects production mode.
func IsProduction(env string) bool {
	return strings.EqualFold(strings.TrimSpace(env), EnvProduction)
}

// AutoMigrateAllowed reports whether --auto-migrate may run. It is
// development-only: production must use the explicit migration command. The
// flag itself is wired by cmd/serve.
func AutoMigrateAllowed(env string) bool {
	return !IsProduction(env)
}

// ValidateDSN checks that raw is a postgres URL and applies the spec's TLS
// rule: non-local hosts require sslmode=verify-full.
func ValidateDSN(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("invalid database URL %s", RedactDSN(raw))
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("invalid database URL %s: missing host", RedactDSN(raw))
	}
	if isLocalHost(host) {
		return nil
	}
	if mode := strings.ToLower(u.Query().Get("sslmode")); mode != "verify-full" {
		return fmt.Errorf("database host %q is not local; sslmode=verify-full is required", host)
	}
	return nil
}

func isLocalHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

var (
	urlPasswordRE     = regexp.MustCompile(`(?i)(://[^:/?#@]*:)[^@]*@`)
	keywordPasswordRE = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|\S+)`)
)

// RedactDSN returns raw with the password replaced. It never returns the
// original secret, including for malformed DSNs.
func RedactDSN(raw string) string {
	if u, err := url.Parse(raw); err == nil &&
		(u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if u.User != nil {
			user := u.User.Username()
			if _, ok := u.User.Password(); ok {
				u.User = url.UserPassword(user, "xxxxx")
			}
		}
		if q := u.Query(); q.Has("password") {
			q.Set("password", "xxxxx")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}
	out := urlPasswordRE.ReplaceAllString(raw, "${1}xxxxx@")
	return keywordPasswordRE.ReplaceAllString(out, "${1}xxxxx")
}

// redactError strips a DSN and its password from a driver error before it is
// returned to a caller that may log it.
func redactError(err error, raw string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if raw != "" {
		msg = strings.ReplaceAll(msg, raw, RedactDSN(raw))
		if u, e := url.Parse(raw); e == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				msg = strings.ReplaceAll(msg, pw, "xxxxx")
			}
		}
	}
	return errors.New(msg)
}

func envValue(lookup func(string) (string, bool), key string) string {
	v, _ := lookup(key)
	return v
}

func envInt32(lookup func(string) (string, bool), key string, def int32) (int32, error) {
	raw := strings.TrimSpace(envValue(lookup, key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return int32(n), nil
}

func envDuration(lookup func(string) (string, bool), key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(envValue(lookup, key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration (for example 5s)", key)
	}
	return d, nil
}
