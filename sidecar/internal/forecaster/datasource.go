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

const systemAggsSQL = `
SELECT date_trunc('day', collected_at) AS day,
       avg((data->>'db_size_bytes')::bigint)     AS avg_db_size,
       max((data->>'db_size_bytes')::bigint)     AS max_db_size,
       max((data->>'active_backends')::int)      AS max_active,
       max((data->>'total_backends')::int)       AS max_total,
       max((data->>'max_connections')::int)      AS max_conns,
       avg((data->>'cache_hit_ratio')::float)    AS avg_cache_hit,
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

const queryAggsSQL = `
SELECT day, sum(max_calls) AS total_calls
FROM (
    SELECT date_trunc('day', s.collected_at) AS day,
           (elem->>'queryid')::bigint        AS qid,
           max((elem->>'calls')::bigint)     AS max_calls
    FROM sage.snapshots s,
         jsonb_array_elements(s.data) AS elem
    WHERE s.category = 'queries'
      AND s.collected_at > now() - make_interval(days => $1)
    GROUP BY 1, 2
) sub
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

const seqAggsSQL = `
SELECT date_trunc('day', s.collected_at) AS day,
       (elem->>'schemaname') || '.' ||
           (elem->>'sequencename')       AS seq_name,
       max((elem->>'pct_used')::float)   AS pct_used,
       max((elem->>'max_value')::bigint) AS max_value
FROM sage.snapshots s,
     jsonb_array_elements(s.data) AS elem
WHERE s.category = 'sequences'
  AND s.collected_at > now() - make_interval(days => $1)
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
