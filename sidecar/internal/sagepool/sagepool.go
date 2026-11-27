// Package sagepool opens the pools pg_sage's one-shot commands use on a
// database it monitors or keeps its state in.
//
// A command's statements are pg_sage's own, like the runtime's: they carry
// application_name pg_sage and the statement tag, and run on sessions
// pinned to READ COMMITTED and search_path = pg_catalog, pg_temp
// (selfmonitor.ConfigurePool). On PostgreSQL 18 pg_stat_statements drops a
// comment that comes before the first token, so a pool without the tagger
// leaves the command's statements reading as workload to pg_sage's own
// readers. The Lakebase OAuth credential is applied after the pins
// (databricksauth.ConfigurePool).
package sagepool

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// New parses dsn with ParseConfig and opens a pool configured by
// Configure. The pool connects lazily, so an unreachable server surfaces
// at the first query.
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if err := Configure(ctx, cfg); err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// ErrUnparseableDSN is ParseConfig's only error, with the wording the
// ledger command prints. pgx's own error prints the connection string
// after a redaction that misses a password holding '@' or ':', and the url
// error it wraps quotes a password holding '/', '?' or '#' as the port, so
// ParseConfig neither returns nor wraps it.
var ErrUnparseableDSN = errors.New(
	"the DSN does not parse (check its form; it is not printed)")

// ParseConfig is pgxpool.ParseConfig with every error replaced by
// ErrUnparseableDSN. It only parses: Configure makes the config pg_sage's.
func ParseConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, ErrUnparseableDSN
	}
	return cfg, nil
}

// ErrNoConfig refuses a pool config that is missing or has no connection
// config, which ParseConfig never returns.
var ErrNoConfig = errors.New("sagepool: no pool config to configure")

// Configure makes cfg's sessions pg_sage's own and adds the Lakebase
// credential when cfg names a Lakebase endpoint. It is for a command that
// sets more on the config (a pool size) before opening it.
func Configure(ctx context.Context, cfg *pgxpool.Config) error {
	if cfg == nil || cfg.ConnConfig == nil {
		return ErrNoConfig
	}
	selfmonitor.ConfigurePool(cfg)
	return databricksauth.ConfigurePool(ctx, cfg)
}
