// Package querystore maintains a per-queryid metrics time-series in
// sage.query_store (F2). Because pg_stat_statements only exposes lifetime
// averages, pg_sage samples per-queryid totals each cycle so it can
// compute *windowed* latency — the substrate for per-queryid
// verify-and-revert (F1) and plan-regression detection (A5).
package querystore

import (
	"context"
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
	// StatsEpoch is the pg_stat_statements statistics epoch the counters
	// belong to (zero = unknown, stored as NULL). Samples from different
	// epochs are never differenced (R10).
	StatsEpoch time.Time
}

// recordSampleSQL inserts one sample and stamps it with the fingerprint
// of the latest plan captured for the queryid (plan_hash, M0), so plan
// flips are visible in the per-queryid series. NULL means no plan has
// been fingerprinted for the queryid yet.
const recordSampleSQL = `/* pg_sage */ INSERT INTO sage.query_store
	   (queryid, calls, total_exec_time, mean_exec_time, rows,
	    stats_epoch, plan_hash)
	 VALUES ($1, $2, $3, $4, $5, $6, (
	     SELECT e.plan_hash FROM sage.explain_cache e
	     WHERE e.queryid = $1 AND e.plan_hash IS NOT NULL
	     ORDER BY e.captured_at DESC, e.id DESC LIMIT 1))`

// Record writes a batch of samples to sage.query_store. A nil/empty
// batch is a no-op. Samples sharing a queryid (pg_stat_statements splits
// a statement by userid and toplevel) are summed into one row so each
// cycle has exactly one sample per queryid (G1-B05).
func Record(ctx context.Context, pool *pgxpool.Pool, samples []Sample) error {
	samples = aggregateSamples(samples)
	if len(samples) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, s := range samples {
		batch.Queue(recordSampleSQL,
			s.QueryID, s.Calls, s.TotalExecMs, s.MeanExecMs, s.Rows,
			epochParam(s.StatsEpoch))
	}
	br := pool.SendBatch(ctx, batch)
	for range samples {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}

// epochParam maps an unknown (zero) epoch to NULL.
func epochParam(epoch time.Time) *time.Time {
	if epoch.IsZero() {
		return nil
	}
	return &epoch
}

type sampleRow struct {
	calls int64
	total float64
}

// WindowedLatencyMs returns the average per-call latency (ms) for a
// queryid over every sample captured at or after `since`. ok is false
// unless the window is measurable: at least two samples, new calls, no
// counter decrease and a single known statistics epoch (see
// WindowedLatencyEvidence).
func WindowedLatencyMs(
	ctx context.Context,
	pool *pgxpool.Pool,
	queryid int64,
	since time.Time,
) (float64, bool, error) {
	ev, err := windowEvidence(ctx, pool, queryid, since, nil)
	if err != nil {
		return 0, false, err
	}
	return ev.LatencyMs, ev.Status == EvidenceMeasured, nil
}

// WindowedLatencyMsBetween returns the average per-call latency (ms) for
// a queryid between the earliest and latest samples inside [from, to].
// ok is false for any non-measured evidence; callers that must tell
// "not sampled" apart from "no regression" use WindowedLatencyEvidence.
func WindowedLatencyMsBetween(
	ctx context.Context,
	pool *pgxpool.Pool,
	queryid int64,
	from, to time.Time,
) (float64, bool, error) {
	ev, err := WindowedLatencyEvidence(ctx, pool, queryid, from, to)
	if err != nil {
		return 0, false, err
	}
	return ev.LatencyMs, ev.Status == EvidenceMeasured, nil
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
