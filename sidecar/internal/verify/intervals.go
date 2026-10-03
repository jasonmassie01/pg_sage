package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// queryIntervalsSQL returns a query's call and time deltas between
// consecutive query_store samples in the window, summed per time bucket
// of $5 seconds, with whether the bucket saw a statistics epoch break. A
// window that spans more than one epoch -- a counter decrease, a changed
// stats_epoch (reset or restart, even after counters regrew), or an
// unknown epoch next to a known one -- is refused as a whole (R10).
// Samples are written only when counters move, so the window starts at
// the last sample before $2 when there is one within $4
// (querystore.AnchorLookback). It reads one index range of
// idx_query_store_qid_time.
const queryIntervalsSQL = `/* pg_sage */ WITH anchor AS (
		SELECT max(captured_at) AS at FROM sage.query_store
		WHERE queryid=$1 AND captured_at < $2 AND captured_at >= $2 - $4::interval
	), samples AS (
		SELECT captured_at,
			calls - lag(calls) OVER w AS d_calls,
			total_exec_time - lag(total_exec_time) OVER w AS d_time,
			calls < lag(calls) OVER w
				OR total_exec_time < lag(total_exec_time) OVER w
				OR (row_number() OVER w > 1
					AND stats_epoch IS DISTINCT FROM lag(stats_epoch) OVER w)
				AS epoch_break
		FROM sage.query_store
		WHERE queryid=$1
			AND captured_at BETWEEN COALESCE((SELECT at FROM anchor), $2) AND $3
		WINDOW w AS (ORDER BY captured_at, id)
	)
	SELECT COALESCE(sum(d_calls), 0)::bigint, COALESCE(sum(d_time), 0)::float8,
		COALESCE(bool_or(epoch_break), false)
	FROM samples WHERE d_calls IS NOT NULL
	GROUP BY floor(extract(epoch FROM captured_at) / $5)
	ORDER BY floor(extract(epoch FROM captured_at) / $5)`

// minBucket is the finest bucket. Empty buckets do not count, so a bucket
// finer than the collector's cadence holds one sample interval.
const minBucket = 5 * time.Second

// bucketWidth sizes the time buckets of a window: about 48 buckets, never
// finer than minBucket or coarser than 6 hours.
func bucketWidth(window time.Duration) time.Duration {
	width := window / 48
	switch {
	case width < minBucket:
		return minBucket
	case width > 6*time.Hour:
		return 6 * time.Hour
	}
	return width
}

// queryMeasurement summarizes one query's window from its bucketed
// intervals: calls, call-weighted mean and its standard error.
func (s *PostgresObservationSource) queryMeasurement(
	ctx context.Context, id int64, from, to time.Time,
) (Measurement, error) {
	rows, err := s.queryer.Query(ctx, queryIntervalsSQL, id, from, to,
		querystore.AnchorLookback, bucketWidth(to.Sub(from)).Seconds())
	if err != nil {
		return Measurement{}, err
	}
	defer rows.Close()
	var intervals []Interval
	broken := false
	for rows.Next() {
		var iv Interval
		var brk bool
		if err := rows.Scan(&iv.Calls, &iv.TotalMs, &brk); err != nil {
			return Measurement{}, fmt.Errorf("scan query %d interval: %w", id, err)
		}
		broken = broken || brk
		intervals = append(intervals, iv)
	}
	if err := rows.Err(); err != nil {
		return Measurement{}, err
	}
	if broken {
		return Measurement{}, nil
	}
	return Summarize(intervals), nil
}
