package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// publishStatementTimeout bounds each read of the publish command.
const publishStatementTimeout = "60s"

// pgPublishSource reads one database in a single read-only, repeatable
// read transaction, so the ledger and the back-test come from one
// snapshot and nothing can be written through it.
type pgPublishSource struct {
	pool *pgxpool.Pool
	tx   pgx.Tx
	name string
}

func openPublishSource(ctx context.Context, dsn string) (publishSource, error) {
	pool, err := databricksauth.NewPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("begin a read-only snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+
		publishStatementTimeout+"'"); err != nil {
		_ = tx.Rollback(ctx)
		pool.Close()
		return nil, fmt.Errorf("bound the snapshot's statements: %w", err)
	}
	return &pgPublishSource{pool: pool, tx: tx, name: dsnDatabase(dsn)}, nil
}

func (s *pgPublishSource) ReadOutcomes(ctx context.Context, q outcomes.Query) (
	outcomes.Result, error) {
	return outcomes.Read(ctx, s.tx, s.name, q)
}

// LatestBacktest is the newest stored back-test's v2 counts.
func (s *pgPublishSource) LatestBacktest(ctx context.Context) (backtest.Counts, bool,
	error) {
	r, found, err := backtest.Latest(ctx, s.tx)
	return r.V2, found, err
}

// Close ends the snapshot (it wrote nothing) and the pool.
func (s *pgPublishSource) Close() {
	_ = s.tx.Rollback(context.Background())
	s.pool.Close()
}
