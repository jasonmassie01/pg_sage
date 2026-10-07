package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
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
		 WHERE state = 'active' AND datname = current_database()
		   AND backend_type = 'client backend'
		   AND `+selfmonitor.ActivityExclusionSQL(""),
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

// logRefusedAction records a finding the executor refused or withheld
// before running its SQL (backend signal, managed provider, denied change
// lease, unverifiable CREATE INDEX). Like an execution failure it is a
// failed action_log row, closed at once with a terminal "failed"
// verification whose reason says it never ran, so the ledger self-audit
// (every recent action has a decision and a verification) holds without
// exempting anything (lifeos 1.8.3: action 6385 was reported as
// missing_verification).
func (e *Executor) logRefusedAction(
	ctx context.Context, f analyzer.Finding, findingID int64,
	beforeState map[string]any, decisionID int64, refusal error,
) int64 {
	actionID := e.logActionWithDecision(ctx, f, findingID, beforeState, decisionID, refusal)
	if _, err := finalizeActionVerification(ctx, e.pool, actionID, "failed",
		"not executed: "+refusal.Error()); err != nil {
		e.logFn("executor", "close verification of refused action %d (%q): %v",
			actionID, f.Title, err)
	}
	return actionID
}

// categorizeAction derives an action_type label from the SQL statement.
func categorizeAction(sql string) string {
	upper := strings.ToUpper(sql)
	switch {
	case IsIndexReplaceSQL(sql):
		return ActionTypeReplaceIndex
	case extstats.IsCreate(sql):
		return "create_statistics"
	case extstats.IsDrop(sql):
		return "drop_statistics"
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
