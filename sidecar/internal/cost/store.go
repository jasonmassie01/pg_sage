package cost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// UsageRetention is how long hourly host usage is kept.
const UsageRetention = 35 * 24 * time.Hour

// Execer runs a statement (a pool, connection or transaction).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Querier reads rows.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// upsertHourlySQL replaces the resource-hour's row with a fuller
// aggregate, or with any aggregate of a new size (a resize mid-hour); a
// restarted process's smaller aggregate never overwrites a fuller one.
const upsertHourlySQL = `/* pg_sage */ INSERT INTO sage.host_usage_hourly AS h
	(resource, hour, instance_class, vcpus, serverless, memory_total_bytes,
	 allocated_storage_bytes, cpu_p50, cpu_p95, cpu_max, mem_available_min, db_load_p95,
	 connections_max, samples, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
ON CONFLICT (resource, hour) DO UPDATE SET instance_class = EXCLUDED.instance_class,
	vcpus = EXCLUDED.vcpus, serverless = EXCLUDED.serverless,
	memory_total_bytes = EXCLUDED.memory_total_bytes,
	allocated_storage_bytes = EXCLUDED.allocated_storage_bytes,
	cpu_p50 = EXCLUDED.cpu_p50, cpu_p95 = EXCLUDED.cpu_p95, cpu_max = EXCLUDED.cpu_max,
	mem_available_min = EXCLUDED.mem_available_min, db_load_p95 = EXCLUDED.db_load_p95,
	connections_max = EXCLUDED.connections_max, samples = EXCLUDED.samples,
	updated_at = now()
WHERE EXCLUDED.samples >= h.samples OR EXCLUDED.instance_class <> h.instance_class
	OR EXCLUDED.vcpus <> h.vcpus`

const pruneHourlySQL = `/* pg_sage */ DELETE FROM sage.host_usage_hourly
WHERE resource = $1 AND hour < $2`

// RecordHourly stores u (cloudtel.UsageSink) and drops the resource's rows
// older than UsageRetention.
func RecordHourly(ctx context.Context, db Execer, u cloudtel.HourlyUsage) error {
	if u.Resource == "" || u.Hour.IsZero() || u.Samples <= 0 {
		return errors.New("host usage row needs a resource, an hour and samples")
	}
	mem, alloc := positive(u.MemoryTotalBytes), positive(u.AllocatedStorageBytes)
	hour := u.Hour.UTC().Truncate(time.Hour)
	if _, err := db.Exec(ctx, upsertHourlySQL, u.Resource, hour, u.InstanceClass, u.VCPUs,
		u.Serverless, mem, alloc, u.CPUP50, u.CPUP95, u.CPUMax, u.MemAvailableMin,
		u.DBLoadP95, u.ConnectionsMax, u.Samples); err != nil {
		return fmt.Errorf("record host usage of %s at %s: %w", u.Resource,
			hour.Format(time.RFC3339), err)
	}
	if _, err := db.Exec(ctx, pruneHourlySQL, u.Resource,
		hour.Add(-UsageRetention)); err != nil {
		return fmt.Errorf("prune host usage of %s: %w", u.Resource, err)
	}
	return nil
}

func positive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

const loadHoursSQL = `/* pg_sage */ SELECT hour, instance_class, vcpus, serverless,
	COALESCE(memory_total_bytes, 0), COALESCE(allocated_storage_bytes, 0), cpu_p95, cpu_max,
	mem_available_min, db_load_p95, samples
FROM sage.host_usage_hourly WHERE resource = $1 AND hour >= $2 ORDER BY hour`

// LoadHours reads resource's hourly rows since since, oldest first.
func LoadHours(ctx context.Context, db Querier, resource string,
	since time.Time) ([]HourRow, error) {
	rows, err := db.Query(ctx, loadHoursSQL, resource, since)
	if err != nil {
		return nil, fmt.Errorf("read host usage of %s: %w", resource, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (HourRow, error) {
		var h HourRow
		err := r.Scan(&h.Hour, &h.InstanceClass, &h.VCPUs, &h.Serverless,
			&h.MemoryTotalBytes, &h.AllocatedStorageBytes, &h.CPUP95, &h.CPUMax,
			&h.MemAvailableMin, &h.DBLoadP95, &h.Samples)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("read host usage of %s: %w", resource, err)
	}
	return out, nil
}

// queryDeltasSQL sums each query's execution time between consecutive
// samples whose later sample falls in ($1, $2], counting pairs across an
// epoch break (R10) separately: they are dropped, never differenced. A
// pair starts at most AnchorLookback ($3) before the window.
var queryDeltasSQL = `/* pg_sage */ WITH s AS (
	SELECT q.queryid, q.captured_at, q.calls, q.total_exec_time, q.stats_epoch,
		lag(q.calls) OVER w AS prev_calls,
		lag(q.total_exec_time) OVER w AS prev_total_exec_time,
		lag(q.stats_epoch) OVER w AS prev_stats_epoch
	FROM sage.query_store q
	WHERE {db:q} AND q.captured_at >= $1::timestamptz - $3::interval
		AND q.captured_at <= $2
	WINDOW w AS (PARTITION BY q.queryid ORDER BY q.captured_at, q.id)
), p AS (
	SELECT s.queryid, s.total_exec_time - s.prev_total_exec_time AS delta,
		` + querystore.EpochBreak("s.", "s.prev_") + ` AS brk
	FROM s WHERE s.prev_calls IS NOT NULL AND s.captured_at > $1
)
SELECT p.queryid, COALESCE(sum(p.delta) FILTER (WHERE NOT p.brk), 0)::float8,
	count(*) FILTER (WHERE NOT p.brk)::int, count(*) FILTER (WHERE p.brk)::int
FROM p GROUP BY p.queryid`

// QueryDeltas is each query's execution time in (from, to] from db's
// history store (histstore.Resolve), labelled with database, and the
// number of pairs dropped across a statistics reset.
func QueryDeltas(ctx context.Context, db any, database string,
	from, to time.Time) ([]QueryDelta, int, error) {
	rows, err := histstore.Resolve(db).Query(ctx, queryDeltasSQL, from, to,
		querystore.AnchorLookback)
	if err != nil {
		return nil, 0, fmt.Errorf("read query deltas of %s: %w", database, err)
	}
	defer rows.Close()
	out := []QueryDelta{}
	dropped := 0
	for rows.Next() {
		var d QueryDelta
		var valid, broken int
		if err := rows.Scan(&d.QueryID, &d.ExecMs, &valid, &broken); err != nil {
			return nil, 0, fmt.Errorf("read query deltas of %s: %w", database, err)
		}
		dropped += broken
		if valid > 0 {
			d.Database = database
			out = append(out, d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read query deltas of %s: %w", database, err)
	}
	return out, dropped, nil
}
