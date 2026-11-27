package querystore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// queryIntervalsManySQL is queryIntervalsSQL for the distinct queries of
// $1 at once: each query's window starts at its own anchor (the last
// sample strictly before $2 within $4), its consecutive samples are
// differenced within the query only, and the deltas are summed per bucket
// of $5 seconds. It reads one index range of idx_query_store_qid_time per
// query.
var queryIntervalsManySQL = `/* pg_sage */ WITH ids AS (
	SELECT DISTINCT i.qid FROM unnest($1::int8[]) AS i(qid)
), anchor AS (
	SELECT ids.qid, (SELECT max(a.captured_at) FROM sage.query_store a
		WHERE ` + anchorRange("a", "ids.qid", "$2", "<", "$4") + `) AS at
	FROM ids
), s AS (
	SELECT q.queryid, q.captured_at, q.calls, q.total_exec_time, q.stats_epoch,
		lag(q.calls) OVER w AS prev_calls,
		lag(q.total_exec_time) OVER w AS prev_total_exec_time,
		lag(q.stats_epoch) OVER w AS prev_stats_epoch
	FROM anchor n
	JOIN sage.query_store q ON {db:q} AND q.queryid = n.qid
		AND q.captured_at BETWEEN COALESCE(n.at, $2) AND $3
	WINDOW w AS (PARTITION BY q.queryid ORDER BY q.captured_at, q.id)
)
SELECT s.queryid, COALESCE(sum(s.calls - s.prev_calls), 0)::bigint,
	COALESCE(sum(s.total_exec_time - s.prev_total_exec_time), 0)::float8,
	COALESCE(bool_or(` + epochBreak("s.", "s.prev_") + `), false)
FROM s WHERE s.calls - s.prev_calls IS NOT NULL
GROUP BY s.queryid, floor(extract(epoch FROM s.captured_at) / $5)
ORDER BY s.queryid, floor(extract(epoch FROM s.captured_at) / $5)`

// QueryIntervalsMany returns each query's intervals in [from, to], summed
// per bucket, oldest first, read in db's history store (histstore.Resolve)
// with one statement. A query without two samples in its window is absent
// from the map. No ids reads nothing.
func QueryIntervalsMany(ctx context.Context, db any, ids []int64, from, to time.Time,
	bucket time.Duration) (map[int64][]Interval, error) {
	if bucket <= 0 {
		return nil, fmt.Errorf("intervals of %d queries: bucket must be positive, got %v",
			len(ids), bucket)
	}
	if to.Before(from) {
		return nil, fmt.Errorf("intervals of %d queries: window ends (%v) before it "+
			"starts (%v)", len(ids), to, from)
	}
	out := make(map[int64][]Interval, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := histstore.Resolve(db).Query(ctx, queryIntervalsManySQL, ids, from, to,
		AnchorLookback, bucket.Seconds())
	if err != nil {
		return nil, fmt.Errorf("intervals of %d queries: %w", len(ids), err)
	}
	var qid int64
	var iv Interval
	_, err = pgx.ForEachRow(rows, []any{&qid, &iv.Calls, &iv.TotalMs, &iv.EpochBreak},
		func() error {
			out[qid] = append(out[qid], iv)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("intervals of %d queries: %w", len(ids), err)
	}
	return out, nil
}
