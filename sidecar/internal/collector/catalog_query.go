package collector

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
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

// reader is the bounded read every collector catalog statement runs
// through (catalogread: read-only, the configured statement and lock
// timeouts, no parallel workers or JIT; a catalog scan spawning two
// workers per statement made pg_sage use 5 backends on lifeos,
// measured.md section 2). The timeouts are read per statement, so a
// configuration reload applies on the next one.
func (c *Collector) reader(db catalogread.Beginner) catalogread.Reader {
	r := catalogread.New(db, catalogread.FromSafety(c.cfg.Safety))
	r.After = c.onCatalogQuery
	return r
}

func (c *Collector) catalogQuery(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	return c.reader(c.pool).Query(ctx, sql, args...)
}

// catalogQueryVia runs a catalog statement on db (a scratch connection).
func (c *Collector) catalogQueryVia(
	ctx context.Context, db catalogread.Beginner, sql string, args ...any,
) (pgx.Rows, error) {
	return c.reader(db).Query(ctx, sql, args...)
}

func (c *Collector) catalogQueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	return c.reader(c.pool).QueryRow(ctx, sql, args...)
}
