// Package querystore maintains a per-queryid metrics time-series in
// sage.query_store (F2). Because pg_stat_statements only exposes lifetime
// averages, pg_sage samples per-queryid totals each cycle so it can
// compute *windowed* latency — the substrate for per-queryid
// verify-and-revert (F1) and plan-regression detection (A5).
package querystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sample is one per-queryid metrics observation for a cycle.
type Sample struct {
	QueryID     int64
	Calls       int64
	TotalExecMs float64 // pg_stat_statements.total_exec_time (already ms)
	MeanExecMs  float64
	Rows        int64
}

// Record writes a batch of samples to sage.query_store. A nil/empty
// batch is a no-op.
func Record(ctx context.Context, pool *pgxpool.Pool, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, s := range samples {
		batch.Queue(
			`/* pg_sage */ INSERT INTO sage.query_store
			   (queryid, calls, total_exec_time, mean_exec_time, rows)
			 VALUES ($1, $2, $3, $4, $5)`,
			s.QueryID, s.Calls, s.TotalExecMs, s.MeanExecMs, s.Rows)
	}
	br := pool.SendBatch(ctx, batch)
	defer br.Close()
	for range samples {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

type sampleRow struct {
	calls int64
	total float64
}

// WindowedLatencyMs returns the average per-call latency (ms) for a
// queryid over the window starting at `since`, computed from the delta
// between the earliest sample at/after `since` and the latest sample.
// ok is false when there is insufficient data (no samples, only one
// sample, no new calls in the window, or a pg_stat_statements reset).
func WindowedLatencyMs(
	ctx context.Context,
	pool *pgxpool.Pool,
	queryid int64,
	since time.Time,
) (float64, bool, error) {
	var earliest, latest sampleRow
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT calls, total_exec_time
		   FROM sage.query_store
		  WHERE queryid = $1 AND captured_at >= $2
		  ORDER BY captured_at ASC LIMIT 1`,
		queryid, since,
	).Scan(&earliest.calls, &earliest.total)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	err = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT calls, total_exec_time
		   FROM sage.query_store
		  WHERE queryid = $1
		  ORDER BY captured_at DESC LIMIT 1`,
		queryid,
	).Scan(&latest.calls, &latest.total)
	if err != nil {
		return 0, false, err
	}
	ms, ok := windowedLatencyMs(earliest, latest)
	return ms, ok, nil
}

// WindowedLatencyMsBetween returns the average per-call latency (ms) for
// a queryid between the earliest sample at/after `from` and the latest
// sample at/before `to`. Used to compare a query's latency before an
// action (baseline window) against after it (verify window) for F1.
func WindowedLatencyMsBetween(
	ctx context.Context,
	pool *pgxpool.Pool,
	queryid int64,
	from, to time.Time,
) (float64, bool, error) {
	var earliest, latest sampleRow
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT calls, total_exec_time
		   FROM sage.query_store
		  WHERE queryid = $1 AND captured_at >= $2 AND captured_at <= $3
		  ORDER BY captured_at ASC LIMIT 1`,
		queryid, from, to,
	).Scan(&earliest.calls, &earliest.total)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	err = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT calls, total_exec_time
		   FROM sage.query_store
		  WHERE queryid = $1 AND captured_at >= $2 AND captured_at <= $3
		  ORDER BY captured_at DESC LIMIT 1`,
		queryid, from, to,
	).Scan(&latest.calls, &latest.total)
	if err != nil {
		return 0, false, err
	}
	ms, ok := windowedLatencyMs(earliest, latest)
	return ms, ok, nil
}

// windowedLatencyMs is the pure delta computation. A pg_stat_statements
// reset (counters decreasing) yields ok=false rather than a bogus value.
func windowedLatencyMs(earliest, latest sampleRow) (float64, bool) {
	dCalls := latest.calls - earliest.calls
	dTotal := latest.total - earliest.total
	if dCalls <= 0 || dTotal < 0 {
		return 0, false
	}
	return dTotal / float64(dCalls), true
}

// Prune deletes query_store rows older than the cutoff. Returns rows
// deleted. Keeps the table bounded alongside the retention sweeper.
func Prune(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int64, error) {
	tag, err := pool.Exec(ctx,
		`/* pg_sage */ DELETE FROM sage.query_store WHERE captured_at < $1`,
		cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
