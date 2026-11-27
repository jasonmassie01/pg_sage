package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/value"
)

// Context probes recorded in before_state (not verification: Phase 1.3
// judges the targeted queries), scoped to the connected database so another
// database's workload never shows up in them (Codex C17).
const cacheHitRatioSQL = `/* pg_sage executor */ SELECT coalesce(
	blks_hit::float / nullif(blks_hit + blks_read, 0), 1.0)
	FROM pg_stat_database WHERE datname = current_database()`

// writeLatencySQL is the application's write latency: pg_sage's own
// statements are tagged after their first keyword ("INSERT /* pg_sage executor */
// INTO sage.action_log ...") and matched 'INSERT%' too (perf-selfexcl).
// rel is the qualified pg_stat_statements view.
func writeLatencySQL(rel string) string {
	return `/* pg_sage executor */ SELECT coalesce(avg(mean_exec_time), 0)
	FROM ` + rel + `
	WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND (query LIKE 'INSERT%' OR query LIKE 'UPDATE%')
	  AND ` + selfmonitor.StatementExclusionSQL("query")
}

// monitorableOutcomes are states a post-action monitor may still change.
// Terminal outcomes (rolled_back, rollback_failed, failed, ...) are final.
const monitorableOutcomes = `('monitoring', 'pending', 'interrupted')`

// CheckHysteresis returns true if this finding — or an earlier finding with
// the same category and object — was rolled back (or its rollback was
// withheld or failed) within the cooldown. Findings are re-inserted with new
// ids after resolution, so the id alone is not a stable identity.
func CheckHysteresis(
	ctx context.Context, pool *pgxpool.Pool, findingID int64, cooldownDays int,
) bool {
	var one int
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT 1 FROM sage.action_log al
		 LEFT JOIN sage.findings prev ON prev.id = al.finding_id
		 WHERE al.outcome IN ('rolled_back', 'rollback_failed', 'rollback_skipped',
		                      'rollback_withheld')
		   AND al.executed_at > now() - make_interval(days => $2)
		   AND (al.finding_id = $1 OR EXISTS (
		        SELECT 1 FROM sage.findings cur WHERE cur.id = $1
		           AND cur.category = prev.category
		           AND cur.object_identifier = prev.object_identifier))
		 LIMIT 1`, findingID, cooldownDays).Scan(&one)
	return err == nil
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

// outcomeWriteAttempts is the first terminal write plus three retries;
// outcomeWriteBackoff doubles between them (executor-07).
const outcomeWriteAttempts = 4

var outcomeWriteBackoff = 100 * time.Millisecond

// updateActionOutcome sets the outcome and rollback_reason for an action,
// retrying a failed write with backoff. A lost terminal write after a
// rollback's DDL left the row in rolling_back. It returns the last error.
func updateActionOutcome(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, outcome, reason string,
) error {
	if pool == nil {
		return fmt.Errorf("record outcome %s for action %d: no database pool",
			outcome, actionID)
	}
	var err error
	var tag pgconn.CommandTag
	wait := outcomeWriteBackoff
	for attempt := 1; ; attempt++ {
		tag, err = pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.action_log
			 SET outcome = $1, rollback_reason = $2, measured_at = now()
			 WHERE id = $3`, outcome, reason, actionID)
		if err == nil || attempt == outcomeWriteAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait *= 2
	}
	if err != nil {
		return fmt.Errorf("record outcome %s for action %d: %w", outcome, actionID, err)
	}
	if tag.RowsAffected() > 0 {
		countActionOutcome(ctx, pool, actionID, outcome)
	}
	return nil
}

// recordActionOutcome is updateActionOutcome that logs the action id and
// outcome when every attempt failed; the caller may still act on the error.
func recordActionOutcome(
	ctx context.Context, pool *pgxpool.Pool, logFn func(string, string, ...any),
	actionID int64, outcome, reason string,
) error {
	err := updateActionOutcome(ctx, pool, actionID, outcome, reason)
	if err != nil && logFn != nil {
		logFn("executor", "action %d: outcome %s was not recorded (the stuck-rollback "+
			"reconciler settles a rolling_back row): %v", actionID, outcome, err)
	}
	return err
}

// setMonitoredOutcome is a compare-and-set: it changes the outcome only while
// the action is still monitorable, so a monitor never overwrites an operator
// rollback or another terminal state. It reports whether the row changed.
func setMonitoredOutcome(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, outcome, reason string,
) bool {
	if pool == nil {
		return false
	}
	tag, err := pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.action_log
		 SET outcome = $1, rollback_reason = $2, measured_at = now()
		 WHERE id = $3 AND outcome IN `+monitorableOutcomes,
		outcome, reason, actionID)
	if err != nil || tag.RowsAffected() != 1 {
		return false
	}
	countActionOutcome(ctx, pool, actionID, outcome)
	return true
}

// updateActionSuccess marks an action whose own post-check verified it (a
// custodian) as successful and snapshots the current state as after_state.
// Terminal rollback/failure outcomes are never overwritten, and value is
// credited only when this call made the change. Phase 1.3 actions settle
// through settleOutcome instead.
func updateActionSuccess(ctx context.Context, pool *pgxpool.Pool, actionID int64) {
	var cacheHit float64
	_ = pool.QueryRow(ctx, cacheHitRatioSQL).Scan(&cacheHit)
	tag, err := pool.Exec(ctx,
		`/* pg_sage executor */ UPDATE sage.action_log
		 SET outcome = 'success',
		     after_state = jsonb_build_object('cache_hit_ratio', $1::float8),
		     measured_at = now()
		 WHERE id = $2 AND outcome NOT IN ('rolled_back', 'rolling_back',
		       'rollback_failed', 'rollback_skipped', 'rollback_withheld', 'failed',
		       'unverifiable')`,
		cacheHit, actionID,
	)
	if err != nil || tag.RowsAffected() != 1 {
		return
	}
	countActionOutcome(ctx, pool, actionID, "success")
	verificationID, err := finalizeActionVerification(
		ctx, pool, actionID, "success", "post-action checks passed",
	)
	if err == nil && verificationID > 0 {
		_, _ = value.NewService(value.NewPostgresRepository(pool)).
			CreditVerifiedAction(ctx, actionID)
	}
}

func finalizeActionVerification(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, verdict, reason string,
) (int64, error) {
	if pool == nil || actionID <= 0 {
		return 0, nil
	}
	var verificationID int64
	err := pool.QueryRow(ctx, `WITH candidate AS (
		SELECT decision_id FROM sage.action_log
		WHERE id=$1 AND decision_id IS NOT NULL AND verification_id IS NULL
		FOR UPDATE
	), inserted AS (
		INSERT INTO sage.verification
			(decision_id, action_log_id, criterion, baseline, minimum_samples,
			 next_evaluation_at, hard_deadline_at, verdict, reason, completed_at)
		SELECT decision_id, $1, '{"kind":"executor_postcheck"}'::jsonb,
			'{}'::jsonb, 1, now(), now(), $2, $3, now() FROM candidate
		RETURNING id
	)
	UPDATE sage.action_log al SET verification_id=inserted.id
	FROM inserted WHERE al.id=$1 RETURNING inserted.id`,
		actionID, verdict, reason).Scan(&verificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err == nil {
		countActionVerdict(ctx, pool, actionID, verdict)
	}
	return verificationID, err
}
