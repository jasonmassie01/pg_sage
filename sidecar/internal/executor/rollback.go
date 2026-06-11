package executor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// RollbackMonitor provides rollback monitoring for executed actions.
type RollbackMonitor struct {
	pool  *pgxpool.Pool
	cfg   *config.Config
	logFn func(string, string, ...any)
}

// NewRollbackMonitor creates a new RollbackMonitor.
func NewRollbackMonitor(
	pool *pgxpool.Pool,
	cfg *config.Config,
	logFn func(string, string, ...any),
) *RollbackMonitor {
	return &RollbackMonitor{pool: pool, cfg: cfg, logFn: logFn}
}

// CheckHysteresis returns true if the given finding was rolled back within
// the cooldown period, preventing re-execution of the same remediation.
func CheckHysteresis(
	ctx context.Context,
	pool *pgxpool.Pool,
	findingID int64,
	cooldownDays int,
) bool {
	var one int
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT 1 FROM sage.action_log
		 WHERE finding_id = $1
		   AND outcome = 'rolled_back'
		   AND executed_at > now() - make_interval(days => $2)`,
		findingID, cooldownDays,
	).Scan(&one)

	return err == nil
}

// MonitorAndRollback runs as a goroutine to monitor the effect of an
// executed action. After the rollback window elapses, it re-checks
// metrics. If regression exceeds the threshold, it rolls back the
// change and marks the action as rolled_back. Otherwise, it marks
// the action as success and records the after_state.
//
// shutdownCh may be nil. When non-nil, closing the channel aborts
// the wait window without performing the post-window regression
// check — the action_log entry is updated with an "interrupted"
// outcome so the operator can tell why it never completed.
func MonitorAndRollback(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
	rollbackSQL string,
	thresholdPct int,
	windowMinutes int,
	logFn func(string, string, ...any),
	shutdownCh <-chan struct{},
) {
	timer := time.NewTimer(time.Duration(windowMinutes) * time.Minute)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		logFn("rollback", "context cancelled for action %d", actionID)
		return
	case <-shutdownCh:
		logFn("rollback",
			"shutdown before rollback window for action %d — "+
				"leaving action in pending state", actionID)
		updateActionOutcome(ctx, pool, actionID, "interrupted",
			"sidecar shutdown before rollback window elapsed")
		return
	case <-timer.C:
		// Window elapsed — check for regression.
	}

	regressed := checkRegression(ctx, pool, actionID, thresholdPct)

	if regressed {
		// Honor an active emergency stop: the rollback fires autonomous
		// DDL, so if the operator has halted all autonomous activity we
		// must not run it. Flag the action for manual handling instead.
		if CheckEmergencyStop(ctx, pool) {
			logFn("rollback",
				"emergency stop active — skipping auto-rollback for "+
					"action %d (manual rollback required)", actionID)
			updateActionOutcome(ctx, pool, actionID, "rollback_skipped",
				"emergency stop active; automatic rollback withheld")
			return
		}
		logFn("rollback",
			"regression detected for action %d, executing rollback",
			actionID,
		)
		var err error
		if NeedsConcurrently(rollbackSQL) || NeedsTopLevel(rollbackSQL) {
			err = ExecConcurrently(ctx, pool, rollbackSQL, 60*time.Second)
		} else {
			err = ExecInTransaction(ctx, pool, rollbackSQL, 60*time.Second)
		}
		if err != nil {
			logFn("rollback",
				"rollback failed for action %d: %v", actionID, err,
			)
			updateActionOutcome(ctx, pool, actionID, "rollback_failed",
				"rollback execution failed: "+err.Error())
			return
		}
		updateActionOutcome(ctx, pool, actionID, "rolled_back",
			"automatic rollback due to regression")
		return
	}

	// No regression — mark success and populate after_state.
	logFn("rollback",
		"no regression for action %d, marking success", actionID,
	)
	updateActionSuccess(ctx, pool, actionID)
}

// checkRegression compares before-state metrics with current metrics.
// Returns true if the current state is worse by more than thresholdPct.
func checkRegression(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
	thresholdPct int,
) bool {
	// Per-query verify-and-revert (F1): if the action recorded the
	// queries it targeted, compare each query's latency before vs after
	// the action using the query store. This is precise where the old
	// global cache-hit / avg-write heuristic was coarse and could mask a
	// per-query regression.
	if ids, executedAt := actionTargetQueries(ctx, pool, actionID); len(ids) > 0 &&
		!executedAt.IsZero() {
		return perQueryRegression(ctx, pool, ids, executedAt, thresholdPct)
	}

	// Read the before_state to determine what metric to check.
	var beforeCacheHit float64
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(
			(before_state->>'cache_hit_ratio')::float, -1
		 ) FROM sage.action_log WHERE id = $1`,
		actionID,
	).Scan(&beforeCacheHit)
	if err != nil || beforeCacheHit < 0 {
		// Cannot determine before-state — assume no regression.
		return false
	}

	// Measure current cache hit ratio as a proxy for overall health.
	var currentCacheHit float64
	err = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(
			sum(blks_hit)::float /
			nullif(sum(blks_hit) + sum(blks_read), 0),
			1.0
		 ) FROM pg_stat_database`,
	).Scan(&currentCacheHit)
	if err != nil {
		return false
	}

	if beforeCacheHit == 0 {
		return false
	}

	dropPct := ((beforeCacheHit - currentCacheHit) / beforeCacheHit) * 100
	if dropPct > float64(thresholdPct) {
		return true
	}

	// Additional signal: check if mean_exec_time for INSERT/UPDATE
	// on the affected table spiked.
	var beforeMeanMs, currentMeanMs float64
	_ = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(
			(before_state->>'mean_exec_time_ms')::float, -1
		 ) FROM sage.action_log WHERE id = $1`,
		actionID,
	).Scan(&beforeMeanMs)

	if beforeMeanMs > 0 {
		_ = pool.QueryRow(ctx,
			`/* pg_sage */ SELECT coalesce(avg(mean_exec_time), 0)
			 FROM pg_stat_statements
			 WHERE query LIKE 'INSERT%' OR query LIKE 'UPDATE%'`,
		).Scan(&currentMeanMs)

		if currentMeanMs > 0 && beforeMeanMs > 0 {
			writeDelta := ((currentMeanMs - beforeMeanMs) / beforeMeanMs) * 100
			if writeDelta > 20.0 {
				return true
			}
		}
	}

	return false
}

// targetQueryIDs extracts the queryids an action targets from a finding's
// detail (slow-query/tuning findings carry "queryid"). Used to seed
// before_state for per-query verify-and-revert (F1).
func targetQueryIDs(f analyzer.Finding) []int64 {
	if f.Detail == nil {
		return nil
	}
	var ids []int64
	if id := detailInt64(f.Detail["queryid"]); id != 0 {
		ids = append(ids, id)
	}
	// queryids may be []int64 (in-memory, from the optimizer) or []any
	// (round-tripped through JSON).
	switch list := f.Detail["queryids"].(type) {
	case []int64:
		for _, id := range list {
			if id != 0 {
				ids = append(ids, id)
			}
		}
	case []any:
		for _, x := range list {
			if id := detailInt64(x); id != 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func detailInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// actionTargetQueries reads the target queryids and execution time an
// action recorded in before_state.
func actionTargetQueries(
	ctx context.Context, pool *pgxpool.Pool, actionID int64,
) ([]int64, time.Time) {
	var idsJSON []byte
	var executedAt time.Time
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT before_state->'target_queryids', executed_at
		   FROM sage.action_log WHERE id = $1`,
		actionID,
	).Scan(&idsJSON, &executedAt)
	if err != nil || len(idsJSON) == 0 {
		return nil, time.Time{}
	}
	var ids []int64
	if json.Unmarshal(idsJSON, &ids) != nil {
		return nil, time.Time{}
	}
	return ids, executedAt
}

// isQueryRegressed reports whether currentMs is worse than baselineMs by
// more than thresholdPct. Pure decision for F1.
func isQueryRegressed(baselineMs, currentMs float64, thresholdPct int) bool {
	if baselineMs <= 0 {
		return false
	}
	deltaPct := ((currentMs - baselineMs) / baselineMs) * 100
	return deltaPct > float64(thresholdPct)
}

// perQueryRegression returns true if any targeted query is slower after
// the action than in the window before it, by more than thresholdPct,
// using the query store windowed latency (F1).
func perQueryRegression(
	ctx context.Context,
	pool *pgxpool.Pool,
	queryIDs []int64,
	executedAt time.Time,
	thresholdPct int,
) bool {
	const baselineWindow = 30 * time.Minute
	for _, qid := range queryIDs {
		baseline, okB, err := querystore.WindowedLatencyMsBetween(
			ctx, pool, qid, executedAt.Add(-baselineWindow), executedAt)
		if err != nil || !okB {
			continue
		}
		current, okC, err := querystore.WindowedLatencyMsBetween(
			ctx, pool, qid, executedAt, time.Now())
		if err != nil || !okC {
			continue
		}
		if isQueryRegressed(baseline, current, thresholdPct) {
			return true
		}
	}
	return false
}

// updateActionOutcome sets the outcome and rollback_reason for an action.
func updateActionOutcome(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
	outcome string,
	reason string,
) {
	_, _ = pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.action_log
		 SET outcome = $1, rollback_reason = $2, measured_at = now()
		 WHERE id = $3`,
		outcome, reason, actionID,
	)
}

// updateActionSuccess marks an action as successful and snapshots
// the current state as after_state.
func updateActionSuccess(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
) {
	// Capture a lightweight after-state snapshot.
	var cacheHit float64
	_ = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT coalesce(
			sum(blks_hit)::float /
			nullif(sum(blks_hit) + sum(blks_read), 0),
			1.0
		 ) FROM pg_stat_database`,
	).Scan(&cacheHit)

	_, _ = pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.action_log
		 SET outcome = 'success',
		     after_state = jsonb_build_object('cache_hit_ratio', $1::float8),
		     measured_at = now()
		 WHERE id = $2`,
		cacheHit, actionID,
	)
}
