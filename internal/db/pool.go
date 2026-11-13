package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open builds a bounded pgx pool, applies the connection timeout and pings the
// database once. Errors are redacted.
func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if err := ValidateDSN(cfg.DatabaseURL); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid database URL %s", RedactDSN(cfg.DatabaseURL))
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] =
		IdleInTransactionTimeout.String()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", redactError(err, cfg.DatabaseURL))
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", redactError(err, cfg.DatabaseURL))
	}
	return pool, nil
}

func (c Config) withDefaults() Config {
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.MinConns < 0 {
		c.MinConns = DefaultMinConns
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	return c
}
