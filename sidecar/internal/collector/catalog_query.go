package collector

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type catalogQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type timedCatalogQuerier struct{ collector *Collector }

func (q timedCatalogQuerier) Query(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	return q.collector.catalogQuery(ctx, sql, args...)
}

func (q timedCatalogQuerier) QueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	return q.collector.catalogQueryRow(ctx, sql, args...)
}

// catalogHook runs inside a catalog transaction after its statement and
// before the rollback (tests: lock counts, settings, statement counts).
type catalogHook func(ctx context.Context, tx pgx.Tx, sql string, args []any)

type catalogRows struct {
	pgx.Rows
	tx     pgx.Tx
	closed bool
	after  func(pgx.Tx)
}

func (r *catalogRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

func (r *catalogRows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.Rows.Close()
	if r.after != nil {
		r.after(r.tx)
	}
	_ = r.tx.Rollback(context.Background())
}

type catalogRow struct {
	row   pgx.Row
	tx    pgx.Tx
	err   error
	after func(pgx.Tx)
}

func (r catalogRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer func() { _ = r.tx.Rollback(context.Background()) }()
	err := r.row.Scan(dest...)
	if r.after != nil {
		r.after(r.tx)
	}
	return err
}

// beginCatalogQuery opens the read-only transaction every collector
// catalog statement runs in: the configured statement and lock timeouts,
// and no parallel workers or JIT (a catalog scan spawning two workers per
// statement made pg_sage use 5 backends on lifeos, measured.md section 2).
func (c *Collector) beginCatalogQuery(ctx context.Context) (pgx.Tx, error) {
	return c.beginCatalogTx(ctx, c.pool)
}

// txBeginner is the pool or one dedicated connection.
type txBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func (c *Collector) beginCatalogTx(ctx context.Context, db txBeginner) (pgx.Tx, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin collector catalog query: %w", err)
	}
	statementMs := strconv.Itoa(c.cfg.Safety.QueryTimeoutMs) + "ms"
	lockMs := strconv.Itoa(c.cfg.Safety.LockTimeout()) + "ms"
	_, err = tx.Exec(ctx, `SELECT
		set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true),
		set_config('max_parallel_workers_per_gather', '0', true),
		set_config('jit', 'off', true)`, statementMs, lockMs)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("set collector catalog timeouts: %w", err)
	}
	return tx, nil
}

func (c *Collector) catalogQuery(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	return c.catalogQueryVia(ctx, c.pool, sql, args...)
}

// catalogQueryVia runs a catalog statement on db (see beginCatalogQuery).
func (c *Collector) catalogQueryVia(
	ctx context.Context, db txBeginner, sql string, args ...any,
) (pgx.Rows, error) {
	tx, err := c.beginCatalogTx(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return &catalogRows{Rows: rows, tx: tx, after: c.afterHook(ctx, sql, args)}, nil
}

// afterHook binds the test hook to one statement; nil in production.
func (c *Collector) afterHook(ctx context.Context, sql string, args []any) func(pgx.Tx) {
	hook := c.onCatalogQuery
	if hook == nil {
		return nil
	}
	return func(tx pgx.Tx) { hook(ctx, tx, sql, args) }
}

func (c *Collector) catalogQueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	tx, err := c.beginCatalogQuery(ctx)
	if err != nil {
		return catalogRow{err: err}
	}
	return catalogRow{row: tx.QueryRow(ctx, sql, args...), tx: tx,
		after: c.afterHook(ctx, sql, args)}
}
