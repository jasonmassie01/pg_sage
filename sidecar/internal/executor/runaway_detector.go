package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// RunawayDetector loads current backend evidence and advances one tracker.
type RunawayDetector struct {
	pool    *pgxpool.Pool
	tracker *RunawayTracker
}

func NewRunawayDetector(
	pool *pgxpool.Pool,
	cfg *config.RunawayConfig,
	logFn func(string, string, ...any),
) *RunawayDetector {
	return &RunawayDetector{
		pool:    pool,
		tracker: NewRunawayTracker(cfg, 0, logFn),
	}
}

func (d *RunawayDetector) Detect(
	ctx context.Context,
) ([]analyzer.Finding, error) {
	if d.tracker.cfg == nil || !d.tracker.cfg.Enabled {
		return d.tracker.Evaluate(nil, nil), nil
	}
	if d.pool == nil {
		return nil, fmt.Errorf("runaway detector database is unavailable")
	}
	active, err := d.loadActiveQueries(ctx)
	if err != nil {
		return nil, err
	}
	blockers, err := d.loadBlockerCounts(ctx)
	if err != nil {
		return nil, err
	}
	return d.tracker.Evaluate(active, blockers), nil
}

func (d *RunawayDetector) loadActiveQueries(
	ctx context.Context,
) ([]ActiveQuery, error) {
	rows, err := d.pool.Query(ctx, activeRunawayQueriesSQL)
	if err != nil {
		return nil, fmt.Errorf("loading active queries: %w", err)
	}
	defer rows.Close()
	var active []ActiveQuery
	for rows.Next() {
		var query ActiveQuery
		var durationSeconds float64
		if err := rows.Scan(
			&query.PID, &query.QueryStart, &query.QueryID, &query.Query,
			&query.AppName, &durationSeconds, &query.State,
		); err != nil {
			return nil, fmt.Errorf("scanning active query: %w", err)
		}
		query.Duration = time.Duration(durationSeconds * float64(time.Second))
		active = append(active, query)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active queries: %w", err)
	}
	return active, nil
}

func (d *RunawayDetector) loadBlockerCounts(
	ctx context.Context,
) (map[int]int, error) {
	rows, err := d.pool.Query(ctx, runawayBlockersSQL)
	if err != nil {
		return nil, fmt.Errorf("loading blocker counts: %w", err)
	}
	defer rows.Close()
	counts := make(map[int]int)
	for rows.Next() {
		var pid, count int
		if err := rows.Scan(&pid, &count); err != nil {
			return nil, fmt.Errorf("scanning blocker count: %w", err)
		}
		counts[pid] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading blocker counts: %w", err)
	}
	return counts, nil
}

const activeRunawayQueriesSQL = `/* pg_sage */
SELECT pid,
       query_start,
       COALESCE(query_id, 0),
       LEFT(query, 200),
       application_name,
       EXTRACT(EPOCH FROM (clock_timestamp() - query_start)),
       state
  FROM pg_stat_activity
 WHERE state = 'active'
   AND pid <> pg_backend_pid()
   AND query_start IS NOT NULL`

const runawayBlockersSQL = `/* pg_sage */
SELECT blocker_pid, count(*)::integer
  FROM pg_stat_activity AS blocked
 CROSS JOIN LATERAL unnest(pg_blocking_pids(blocked.pid)) AS blocker_pid
 GROUP BY blocker_pid`
