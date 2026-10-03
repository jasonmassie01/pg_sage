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

// QueryDailyQueryAggs returns daily query call volume aggregates (a
// one-off read: a Forecaster remembers the decoded samples across runs).
func QueryDailyQueryAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DayQueryAgg, error) {
	aggs, err := (&dayHistory{}).queryAggs(ctx, pool, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query query aggs: %w", err)
	}
	return aggs, nil
}

// QueryDailySeqAggs returns daily sequence usage aggregates (a one-off
// read: a Forecaster remembers the decoded samples across runs).
func QueryDailySeqAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DaySeqAgg, error) {
	aggs, err := (&dayHistory{}).seqAggs(ctx, pool, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query seq aggs: %w", err)
	}
	return aggs, nil
}

// dailyQueryAggs is QueryDailyQueryAggs over the Forecaster's samples.
func (f *Forecaster) dailyQueryAggs(ctx context.Context) ([]DayQueryAgg, error) {
	aggs, err := f.history.queryAggs(ctx, f.pool, f.cfg.LookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query query aggs: %w", err)
	}
	return aggs, nil
}

// dailySeqAggs is QueryDailySeqAggs over the Forecaster's samples.
func (f *Forecaster) dailySeqAggs(ctx context.Context) ([]DaySeqAgg, error) {
	aggs, err := f.history.seqAggs(ctx, f.pool, f.cfg.LookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query seq aggs: %w", err)
	}
	return aggs, nil
}
