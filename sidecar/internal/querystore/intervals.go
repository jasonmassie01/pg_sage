package querystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The query store's window readers. Two rules decide what a window of
// samples can say, and both are written once, here:
//
//   - The anchor: the Recorder writes a query's sample only when its
//     counters move (and hourly), so an idle query has no sample at a
//     window's start. The window starts at the query's newest sample
//     before (or, for a capture pair, at) the start, when there is one
//     within AnchorLookback; an older one means the query left the sampled
//     set and its counters at the start are unknown.
//   - The epoch break (R10): consecutive samples belong to different
//     statistics epochs when a counter decreased (pg_stat_statements reset
//     or entry eviction), when the recorded epoch changed (a reset or a
//     restart, even after the counters regrew), or when an unknown epoch
//     sits next to a known one. Deltas across a break are invalid.

// anchorRange is the anchor rule's predicate on alias's samples of query
// qid, relative to the window start: op is "<" (strictly before) or "<="
// (at or before). Every operand is a placeholder or a column, never data.
func anchorRange(alias, qid, start, op, lookback string) string {
	return "{db:" + alias + "} AND " + alias + ".queryid = " + qid +
		" AND " + alias + ".captured_at " + op + " " + start +
		" AND " + alias + ".captured_at >= " + start + " - " + lookback + "::interval"
}

// epochBreak is R10's predicate between a sample (columns cur+name) and the
// sample before it (columns prev+name).
func epochBreak(cur, prev string) string {
	return "(" + cur + "calls < " + prev + "calls" +
		" OR " + cur + "total_exec_time < " + prev + "total_exec_time" +
		" OR " + cur + "stats_epoch IS DISTINCT FROM " + prev + "stats_epoch)"
}

// EpochBreak is R10's predicate for history readers outside this package
// (cost attribution): the same rule, written once.
func EpochBreak(cur, prev string) string { return epochBreak(cur, prev) }

// queryIntervalsSQL returns query $1's call and time deltas between
// consecutive samples in [$2, $3], summed per bucket of $5 seconds, with
// whether the bucket holds an epoch break. The window starts at the last
// sample strictly before $2 within $4 (AnchorLookback). It reads one index
// range of idx_query_store_qid_time.
var queryIntervalsSQL = `/* pg_sage */ WITH anchor AS (
	SELECT max(a.captured_at) AS at FROM sage.query_store a
	WHERE ` + anchorRange("a", "$1", "$2", "<", "$4") + `
), s AS (
	SELECT q.captured_at, q.calls, q.total_exec_time, q.stats_epoch,
		lag(q.calls) OVER w AS prev_calls,
		lag(q.total_exec_time) OVER w AS prev_total_exec_time,
		lag(q.stats_epoch) OVER w AS prev_stats_epoch
	FROM sage.query_store q
	WHERE {db:q} AND q.queryid = $1
		AND q.captured_at BETWEEN COALESCE((SELECT at FROM anchor), $2) AND $3
	WINDOW w AS (ORDER BY q.captured_at, q.id)
)
SELECT COALESCE(sum(s.calls - s.prev_calls), 0)::bigint,
	COALESCE(sum(s.total_exec_time - s.prev_total_exec_time), 0)::float8,
	COALESCE(bool_or(` + epochBreak("s.", "s.prev_") + `), false)
FROM s WHERE s.calls - s.prev_calls IS NOT NULL
GROUP BY floor(extract(epoch FROM s.captured_at) / $5)
ORDER BY floor(extract(epoch FROM s.captured_at) / $5)`

// captureDeltasSQL lists the top $3 queries of the interval ending at
// capture $1 by interval total time. Each query's interval starts at its
// own newest sample at or before the previous capture $2 (within $4,
// AnchorLookback), not at a row of $2 an idle query does not have. The
// last column is whether any query of the capture has an epoch break,
// computed before the limit.
var captureDeltasSQL = `/* pg_sage sre:slo_proxy */
SELECT (c.calls - p.calls)::float8, (c.total_exec_time - p.total_exec_time)::float8,
	bool_or(` + epochBreak("c.", "p.") + `) OVER ()
FROM sage.query_store c
CROSS JOIN LATERAL (
	SELECT q.calls, q.total_exec_time, q.stats_epoch FROM sage.query_store q
	WHERE ` + anchorRange("q", "c.queryid", "$2", "<=", "$4") + `
	ORDER BY q.captured_at DESC, q.id DESC LIMIT 1) p
WHERE {db:c} AND c.captured_at = $1
ORDER BY c.total_exec_time - p.total_exec_time DESC
LIMIT $3`

// recentCapturesSQL lists the newest $2 distinct capture times within $1.
const recentCapturesSQL = `/* pg_sage sre:slo_proxy */
SELECT DISTINCT q.captured_at FROM sage.query_store q
WHERE {db:q} AND q.captured_at > pg_catalog.now() - $1::interval
ORDER BY q.captured_at DESC LIMIT $2`

// Interval is one time bucket of a query's window: the calls and time
// between its consecutive samples, and whether an epoch break is among
// them (the deltas are then invalid, R10).
type Interval struct {
	Calls      int64
	TotalMs    float64
	EpochBreak bool
}

// Delta is one query's calls and time between two captures.
type Delta struct {
	Calls, TotalMs float64
}

// QueryIntervals returns queryid's intervals in [from, to], summed per
// bucket, oldest first, read in db's history store (histstore.Resolve).
// A window without two samples has no intervals.
func QueryIntervals(ctx context.Context, db any, queryid int64, from, to time.Time,
	bucket time.Duration) ([]Interval, error) {
	if bucket <= 0 {
		return nil, fmt.Errorf("query %d intervals: bucket must be positive, got %v",
			queryid, bucket)
	}
	rows, err := histstore.Resolve(db).Query(ctx, queryIntervalsSQL, queryid, from, to,
		AnchorLookback, bucket.Seconds())
	if err != nil {
		return nil, fmt.Errorf("query %d intervals: %w", queryid, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Interval, error) {
		var iv Interval
		err := r.Scan(&iv.Calls, &iv.TotalMs, &iv.EpochBreak)
		return iv, err
	})
	if err != nil {
		return nil, fmt.Errorf("query %d intervals: %w", queryid, err)
	}
	return out, nil
}

// CaptureDeltas returns the top limit queries of the interval ending at
// capture by interval total time, largest first, and whether any query of
// the capture has an epoch break since prev (no delta is then valid). It
// reads db's history store (histstore.Resolve).
func CaptureDeltas(ctx context.Context, db any, capture, prev time.Time, limit int) ([]Delta,
	bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("capture deltas: limit must be at least 1, got %d", limit)
	}
	rows, err := histstore.Resolve(db).Query(ctx, captureDeltasSQL, capture, prev, limit,
		AnchorLookback)
	if err != nil {
		return nil, false, fmt.Errorf("capture deltas at %v: %w", capture, err)
	}
	defer rows.Close()
	var out []Delta
	reset := false
	for rows.Next() {
		var d Delta
		var brk bool
		if err := rows.Scan(&d.Calls, &d.TotalMs, &brk); err != nil {
			return nil, false, fmt.Errorf("capture deltas at %v: %w", capture, err)
		}
		reset = reset || brk
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("capture deltas at %v: %w", capture, err)
	}
	return out, reset, nil
}

// RecentCaptures returns the newest n distinct capture times of db's query
// store within the last within, newest first.
func RecentCaptures(ctx context.Context, db any, within time.Duration, n int) ([]time.Time,
	error) {
	if n < 1 || within <= 0 {
		return nil, errors.New("recent captures: n and within must be positive")
	}
	rows, err := histstore.Resolve(db).Query(ctx, recentCapturesSQL, within, n)
	if err != nil {
		return nil, fmt.Errorf("recent captures: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return nil, fmt.Errorf("recent captures: %w", err)
	}
	return out, nil
}
