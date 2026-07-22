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

type catalogRows struct {
	pgx.Rows
	tx     pgx.Tx
	closed bool
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
	_ = r.tx.Rollback(context.Background())
}

type catalogRow struct {
	row pgx.Row
	tx  pgx.Tx
	err error
}

func (r catalogRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer func() { _ = r.tx.Rollback(context.Background()) }()
	return r.row.Scan(dest...)
}

func (c *Collector) beginCatalogQuery(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin collector catalog query: %w", err)
	}
	statementMs := strconv.Itoa(c.cfg.Safety.QueryTimeoutMs) + "ms"
	lockMs := strconv.Itoa(c.cfg.Safety.LockTimeout()) + "ms"
	_, err = tx.Exec(ctx, `SELECT
		set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, statementMs, lockMs)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("set collector catalog timeouts: %w", err)
	}
	return tx, nil
}

func (c *Collector) catalogQuery(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	tx, err := c.beginCatalogQuery(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return &catalogRows{Rows: rows, tx: tx}, nil
}

func (c *Collector) catalogQueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	tx, err := c.beginCatalogQuery(ctx)
	if err != nil {
		return catalogRow{err: err}
	}
	return catalogRow{row: tx.QueryRow(ctx, sql, args...), tx: tx}
}
