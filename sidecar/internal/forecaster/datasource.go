package forecaster

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// DaySystemAgg holds daily aggregated system-level metrics.
type DaySystemAgg struct {
	Day               time.Time
	AvgDBSizeBytes    float64
	MaxDBSizeBytes    float64
	MaxActiveBackends float64
	MaxTotalBackends  float64
	MaxConnections    float64
	// The cache inputs of a closed day (forecast-04): the interval block
	// accesses (blks_hit + blks_read growth between two snapshots,
	// collection-06) summed over the day, and the share that hit
	// shared_buffers, a fraction. The lifetime ratio is not read.
	CacheHitRatio       float64
	CacheBlocksAccessed float64
	// The split checkpoint counters at the day's last snapshot that carried
	// them, and that snapshot's time (forecast-03). Only closed days
	// carry them: today's partial day is unknown, so the newest input does
	// not move with the hour.
	CheckpointsTimed     float64
	CheckpointsRequested float64
	CheckpointsLastAt    time.Time
	// An input is unknown on a day where every snapshot left it null (or,
	// for the size, wrote 0): its value above is then 0 and the rules
	// that read it skip the day (forecast-07). Checkpoints are also
	// unknown on legacy rows without the split and on the current day.
	DBSizeUnknown         bool
	BackendsUnknown       bool
	MaxConnectionsUnknown bool
	CheckpointsUnknown    bool
	// CacheUnknown: no snapshot of the closed day carried interval
	// counters (legacy rows, a stats reset, today), no block was accessed,
	// or (gateCacheDays) fewer than forecaster.cache_min_blocks_per_day.
	CacheUnknown bool
}

// DayQueryAgg holds daily aggregated query volume.
type DayQueryAgg struct {
	Day        time.Time
	TotalCalls float64
}

const systemAggsSQL = `/* pg_sage */
SELECT date_trunc('day', collected_at) AS day,
       -- db_size_bytes is null until pg_database_size succeeds; legacy
       -- rows wrote 0. A day with no measured size stays NULL (forecast-07).
       avg((data->>'db_size_bytes')::bigint)
           FILTER (WHERE (data->>'db_size_bytes')::bigint > 0) AS avg_db_size,
       max((data->>'db_size_bytes')::bigint)
           FILTER (WHERE (data->>'db_size_bytes')::bigint > 0) AS max_db_size,
       max((data->>'active_backends')::int)      AS max_active,
       max((data->>'total_backends')::int)       AS max_total,
       max((data->>'max_connections')::int)      AS max_conns,
       -- Interval block counts of closed days (forecast-04). A reset
       -- interval (null blocks) and a legacy row add nothing; a day with
       -- none stays NULL, unknown.
       sum(c.accessed) FILTER (WHERE c.closed) AS cache_accessed,
       sum(c.accessed * COALESCE(c.ratio, 0)) FILTER (WHERE c.closed) AS cache_hits,
       -- Split checkpoint counters of closed days only, read at the day's
       -- last split sample so a mid-day stats reset shows as a drop
       -- rather than as the pre-reset maximum (forecast-03).
       (array_agg((data->>'checkpoints_timed')::bigint ORDER BY s.collected_at DESC)
           FILTER (WHERE k.split))[1] AS chk_timed,
       (array_agg((data->>'checkpoints_requested')::bigint ORDER BY s.collected_at DESC)
           FILTER (WHERE k.split))[1] AS chk_req,
       max(s.collected_at) FILTER (WHERE k.split) AS chk_last_at
FROM sage.snapshots s
CROSS JOIN LATERAL (SELECT s.collected_at < date_trunc('day', now()) AS closed,
       (s.data->'interval'->>'blks_accessed')::float8 AS accessed,
       (s.data->'interval'->>'cache_hit_ratio')::float8 AS ratio) c
CROSS JOIN LATERAL (SELECT c.closed
       AND s.data->>'checkpoints_timed' IS NOT NULL
       AND s.data->>'checkpoints_requested' IS NOT NULL AS split) k
WHERE {db:s} AND s.category = 'system'
  AND s.collected_at > now() - make_interval(days => $1)
GROUP BY 1 ORDER BY 1`

// QueryDailySystemAggs returns daily system metric aggregates for
// the given lookback period.
func QueryDailySystemAggs(
	ctx context.Context,
	pool *pgxpool.Pool,
	lookbackDays int,
) ([]DaySystemAgg, error) {
	rows, err := histstore.Resolve(pool).Query(ctx, systemAggsSQL, lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query system aggs: %w", err)
	}
	defer rows.Close()

	var aggs []DaySystemAgg
	for rows.Next() {
		a, err := scanSystemAgg(rows)
		if err != nil {
			return nil, err
		}
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate system aggs: %w", err)
	}
	return aggs, nil
}

// scanSystemAgg reads one day of systemAggsSQL. The size, backend,
// max_connections, cache and checkpoint aggregates are NULL on a day
// where no snapshot measured them (checkpoints: no split row of a closed
// day; cache: no interval counters on a closed day); such an input is
// marked unknown, not failed.
func scanSystemAgg(rows pgx.Rows) (DaySystemAgg, error) {
	var a DaySystemAgg
	var avgSize, maxSize, active, total, maxConns, timed, requested *float64
	var accessed, hits *float64
	var lastAt *time.Time
	if err := rows.Scan(
		&a.Day, &avgSize, &maxSize, &active, &total, &maxConns,
		&accessed, &hits, &timed, &requested, &lastAt,
	); err != nil {
		return DaySystemAgg{}, fmt.Errorf("scan system agg: %w", err)
	}
	a.AvgDBSizeBytes, a.MaxDBSizeBytes = orZero(avgSize), orZero(maxSize)
	a.DBSizeUnknown = maxSize == nil
	a.MaxActiveBackends, a.MaxTotalBackends = orZero(active), orZero(total)
	a.BackendsUnknown = active == nil && total == nil
	a.MaxConnections = orZero(maxConns)
	a.MaxConnectionsUnknown = maxConns == nil
	a.CacheBlocksAccessed = orZero(accessed)
	a.CacheUnknown = a.CacheBlocksAccessed <= 0 || hits == nil
	if !a.CacheUnknown {
		a.CacheHitRatio = *hits / a.CacheBlocksAccessed
	}
	a.CheckpointsTimed, a.CheckpointsRequested = orZero(timed), orZero(requested)
	a.CheckpointsUnknown = timed == nil || requested == nil || lastAt == nil
	if !a.CheckpointsUnknown {
		a.CheckpointsLastAt = *lastAt
	}
	return a, nil
}

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
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

// dailyQueryAggs is QueryDailyQueryAggs over the Forecaster's samples.
func (f *Forecaster) dailyQueryAggs(ctx context.Context) ([]DayQueryAgg, error) {
	aggs, err := f.history.queryAggs(ctx, f.pool, f.cfg.LookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query query aggs: %w", err)
	}
	return aggs, nil
}
