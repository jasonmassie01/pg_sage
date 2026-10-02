package forecaster

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DaySystemAgg holds daily aggregated system-level metrics.
type DaySystemAgg struct {
	Day               time.Time
	AvgDBSizeBytes    float64
	MaxDBSizeBytes    float64
	MaxActiveBackends float64
	MaxTotalBackends  float64
	MaxConnections    float64
	AvgCacheHitRatio  float64
	TotalCheckpoints  float64
}

// DayQueryAgg holds daily aggregated query volume.
type DayQueryAgg struct {
	Day        time.Time
	TotalCalls float64
}

// DaySeqAgg holds daily aggregated sequence usage.
type DaySeqAgg struct {
	Day      time.Time
	SeqName  string
	PctUsed  float64
	MaxValue int64
}

const systemAggsSQL = `/* pg_sage */
SELECT date_trunc('day', collected_at) AS day,
       avg((data->>'db_size_bytes')::bigint)     AS avg_db_size,
       max((data->>'db_size_bytes')::bigint)     AS max_db_size,
       max((data->>'active_backends')::int)      AS max_active,
       max((data->>'total_backends')::int)       AS max_total,
       max((data->>'max_connections')::int)      AS max_conns,
       -- cache_hit_ratio is a fraction; legacy rows stored a percent and
       -- negative values mean "no data" (C01).
       COALESCE(avg(CASE WHEN (data->>'cache_hit_ratio')::float > 1
                         THEN (data->>'cache_hit_ratio')::float / 100
                         ELSE (data->>'cache_hit_ratio')::float END)
                FILTER (WHERE (data->>'cache_hit_ratio')::float > 0),
                -1)                              AS avg_cache_hit,
       max((data->>'total_checkpoints')::bigint) AS total_chkpts
FROM sage.snapshots
WHERE category = 'system'
  AND collected_at > now() - make_interval(days => $1)
GROUP BY 1 ORDER BY 1`

// QueryDailySystemAggs returns daily system metric aggregates for
// the given lookback period.
func QueryDailySystemAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DaySystemAgg, error) {
	rows, err := pool.Query(ctx, systemAggsSQL, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query system aggs: %w", err)
	}
	defer rows.Close()

	var aggs []DaySystemAgg
	for rows.Next() {
		var a DaySystemAgg
		if err := rows.Scan(
			&a.Day,
			&a.AvgDBSizeBytes, &a.MaxDBSizeBytes,
			&a.MaxActiveBackends, &a.MaxTotalBackends,
			&a.MaxConnections,
			&a.AvgCacheHitRatio,
			&a.TotalCheckpoints,
		); err != nil {
			return nil, fmt.Errorf("scan system agg: %w", err)
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate system aggs: %w", err)
	}
	return aggs, nil
}

// daySamplesSQL picks, for one snapshot category in the lookback, the
// first and last non-empty snapshot of each day (dogfood lifeos-1: every
// snapshot used to be expanded; lifeos had 1,999 'sequences' snapshots of
// 12,000 elements, > 70 s). An empty or null snapshot is at most 12 bytes
// of jsonb, so pg_column_size tells them apart without detoasting.
const daySamplesSQL = `
    SELECT s.id, s.collected_at, s.data,
           row_number() OVER (PARTITION BY date_trunc('day', s.collected_at)
                              ORDER BY s.collected_at, s.id) AS first_rank,
           row_number() OVER (PARTITION BY date_trunc('day', s.collected_at)
                              ORDER BY s.collected_at DESC, s.id DESC) AS last_rank
    FROM sage.snapshots s
    WHERE s.category = %s
      AND s.collected_at > now() - make_interval(days => $1)
      AND pg_column_size(s.data) > 12`

// queryAggsSQL sums per-day call deltas. pg_stat_statements counters are
// cumulative since the last reset, so each sample contributes
// calls - previous calls for the same queryid; a drop (reset or eviction)
// contributes the new count, and a queryid's first sample in the window
// contributes 0 because its baseline is unknown (C10). Only each day's
// first and last snapshot are sampled: for monotonic counters the daily
// totals telescope to the same sums.
var queryAggsSQL = `/* pg_sage */
WITH d AS (` + fmt.Sprintf(daySamplesSQL, "'queries'") + `
)
SELECT day, COALESCE(sum(delta), 0)::float8 AS total_calls
FROM (
    SELECT date_trunc('day', collected_at) AS day,
           CASE WHEN prev_calls IS NULL THEN 0
                WHEN calls >= prev_calls THEN calls - prev_calls
                ELSE calls
           END AS delta
    FROM (
        SELECT d.collected_at,
               (elem->>'calls')::bigint AS calls,
               lag((elem->>'calls')::bigint) OVER (
                   PARTITION BY (elem->>'queryid')::bigint
                   ORDER BY d.collected_at, d.id) AS prev_calls
        FROM d, jsonb_array_elements(d.data) AS elem
        WHERE d.first_rank = 1 OR d.last_rank = 1
    ) samples
) deltas
GROUP BY day ORDER BY day`

// QueryDailyQueryAggs returns daily query call volume aggregates.
func QueryDailyQueryAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DayQueryAgg, error) {
	rows, err := pool.Query(ctx, queryAggsSQL, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query query aggs: %w", err)
	}
	defer rows.Close()

	var aggs []DayQueryAgg
	for rows.Next() {
		var a DayQueryAgg
		if err := rows.Scan(&a.Day, &a.TotalCalls); err != nil {
			return nil, fmt.Errorf("scan query agg: %w", err)
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate query aggs: %w", err)
	}
	return aggs, nil
}

// seqAggsSQL reads each day's last non-empty 'sequences' snapshot: a
// sequence's use only grows (a restart is a new, lower reading), so the
// day's last reading is its use that day. Sequences under 1% are left
// out: none can be within the forecaster's horizons (the collector keeps
// those only as its top N).
var seqAggsSQL = `/* pg_sage */
WITH d AS (` + fmt.Sprintf(daySamplesSQL, "'sequences'") + `
)
SELECT date_trunc('day', d.collected_at) AS day,
       (elem->>'schemaname') || '.' ||
           (elem->>'sequencename')       AS seq_name,
       max((elem->>'pct_used')::float)   AS pct_used,
       max((elem->>'max_value')::bigint) AS max_value
FROM d, jsonb_path_query(d.data, '$[*] ? (@.pct_used >= 1)') AS elem
WHERE d.last_rank = 1
GROUP BY 1, 2 ORDER BY 1`

// QueryDailySeqAggs returns daily sequence usage aggregates.
func QueryDailySeqAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DaySeqAgg, error) {
	rows, err := pool.Query(ctx, seqAggsSQL, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query seq aggs: %w", err)
	}
	defer rows.Close()

	var aggs []DaySeqAgg
	for rows.Next() {
		var a DaySeqAgg
		if err := rows.Scan(
			&a.Day, &a.SeqName, &a.PctUsed, &a.MaxValue,
		); err != nil {
			return nil, fmt.Errorf("scan seq agg: %w", err)
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seq aggs: %w", err)
	}
	return aggs, nil
}
