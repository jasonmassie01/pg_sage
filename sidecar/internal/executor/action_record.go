package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// snapshotBeforeState captures current database health metrics
// to serve as a comparison baseline for rollback decisions.
func (e *Executor) snapshotBeforeState(
	ctx context.Context, queryIDs []int64,
) map[string]any {
	state := map[string]any{}
	// Record which queries this action targets so verify-and-revert can
	// compare their per-query latency before/after, instead of relying on
	// coarse global metrics (F1).
	if len(queryIDs) > 0 {
		state["target_queryids"] = queryIDs
	}

	var cacheHit float64
	err := e.pool.QueryRow(ctx, cacheHitRatioSQL).Scan(&cacheHit)
	if err == nil {
		state["cache_hit_ratio"] = cacheHit
	}

	var activeBackends int
	err = e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM pg_stat_activity
		 WHERE state = 'active' AND datname = current_database()`,
	).Scan(&activeBackends)
	if err == nil {
		state["active_backends"] = activeBackends
	}

	var meanExecMs float64
	err = e.pool.QueryRow(ctx, writeLatencySQL).Scan(&meanExecMs)
	if err == nil && meanExecMs > 0 {
		state["mean_exec_time_ms"] = meanExecMs
	}

	return state
}

// logAction records the executed action in sage.action_log.
func (e *Executor) logAction(
	ctx context.Context,
	f analyzer.Finding,
	findingID int64,
	beforeState map[string]any,
	execErr error,
) int64 {
	return e.logActionWithDecision(ctx, f, findingID, beforeState, 0, execErr)
}

func (e *Executor) logActionWithDecision(
	ctx context.Context,
	f analyzer.Finding,
	findingID int64,
	beforeState map[string]any,
	decisionID int64,
	execErr error,
) int64 {
	return e.logClaimedAction(ctx, f, findingID, beforeState, decisionID, execErr, nil)
}

// categorizeAction derives an action_type label from the SQL statement.
func categorizeAction(sql string) string {
	upper := strings.ToUpper(sql)
	switch {
	case strings.Contains(upper, "CREATE INDEX"):
		return "create_index"
	case strings.Contains(upper, "DROP INDEX"):
		return "drop_index"
	case strings.Contains(upper, "REINDEX"):
		return "reindex"
	case strings.Contains(upper, "VACUUM"):
		return "vacuum"
	case strings.Contains(upper, "ANALYZE"):
		return "analyze"
	case strings.Contains(upper, "PG_CANCEL_BACKEND"):
		return "cancel_backend"
	case strings.Contains(upper, "PG_TERMINATE_BACKEND"):
		return "terminate_backend"
	case strings.Contains(upper, "ALTER"):
		return "alter"
	default:
		return "ddl"
	}
}

// actionOutcome returns "failed" when execErr is non-nil, "monitoring"
// otherwise. Successful reversible actions stay in monitoring until
// post-action verification marks them success or rolled_back.
// This determines whether acted_on_at is set on the finding — failed actions
// must leave the finding retryable.
func actionOutcome(execErr error) string {
	if execErr != nil {
		return "failed"
	}
	return "monitoring"
}

// nilIfEmpty returns nil for empty strings, used for nullable SQL params.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
