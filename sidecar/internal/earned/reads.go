package earned

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Perf (2026-10-04): the Trust view used to send its ledger reads one
// after another, 17 round trips per view with the effective levels. Each
// read is cheap; on a loaded host each round trip is not (10-350 ms
// measured), so the view's reads now go through reads: the store runs a
// read at once, a readBatch queues it and sends every queued read in one
// pipelined round trip.

// reads runs a set read now or queues it.
type reads interface {
	each(ctx context.Context, what, sql string, args []any, scan func(pgx.Rows) error) error
}

// each runs the read now.
func (s *PostgresStore) each(ctx context.Context, what, sql string, args []any,
	scan func(pgx.Rows) error) error {
	return s.queryEach(ctx, what, sql, args, scan)
}

// readBatch queues reads; run sends them in one round trip and runs each
// read's scan in order. Results are filled only by run.
type readBatch struct{ batch pgx.Batch }

// each queues the read; the error is run's.
func (b *readBatch) each(_ context.Context, what, sql string, args []any,
	scan func(pgx.Rows) error) error {
	b.batch.Queue(sql, args...).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			if err := scan(rows); err != nil {
				return storeErr(what, err)
			}
		}
		return storeErr(what, rows.Err())
	})
	return nil
}

// runBatch sends b's reads in one round trip.
func (s *PostgresStore) runBatch(ctx context.Context, b *readBatch) error {
	if b.batch.Len() == 0 {
		return nil
	}
	return storeErr("read the trust ledger", s.pool.SendBatch(ctx, &b.batch).Close())
}
