package executor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/value"
)

type regressionVerdict int

const (
	regressionNone regressionVerdict = iota
	regressionDetected
	regressionUnverifiable
)

// Health probes are scoped to the connected database so another database's
// workload can neither trigger nor hide a regression (Codex C17).
const cacheHitRatioSQL = `/* pg_sage */ SELECT coalesce(
	blks_hit::float / nullif(blks_hit + blks_read, 0), 1.0)
	FROM pg_stat_database WHERE datname = current_database()`

// writeLatencySQL is the application's write latency: pg_sage's own
// statements are tagged after their first keyword ("INSERT /* pg_sage */
// INTO sage.action_log ...") and matched 'INSERT%' too (perf-selfexcl).
var writeLatencySQL = `/* pg_sage */ SELECT coalesce(avg(mean_exec_time), 0)
	FROM pg_stat_statements
	WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND (query LIKE 'INSERT%' OR query LIKE 'UPDATE%')
	  AND ` + selfmonitor.StatementExclusionSQL("query")

// monitorableOutcomes are states a post-action monitor may still change.
// Terminal outcomes (rolled_back, rollback_failed, failed, ...) are final.
const monitorableOutcomes = `('monitoring', 'pending', 'interrupted')`

// evaluateRegression compares before-state metrics with current metrics.
// Missing or unreadable evidence is unverifiable, never "no regression".
func evaluateRegression(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, thresholdPct int,
) regressionVerdict {
	if ids, executedAt := actionTargetQueries(ctx, pool, actionID); len(ids) > 0 &&
		!executedAt.IsZero() {
		return perQueryRegression(ctx, pool, ids, executedAt, thresholdPct)
	}
	var beforeCacheHit, beforeMeanMs float64
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT
		coalesce((before_state->>'cache_hit_ratio')::float, -1),
		coalesce((before_state->>'mean_exec_time_ms')::float, -1)
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&beforeCacheHit, &beforeMeanMs)
	if err != nil || beforeCacheHit <= 0 {
		return regressionUnverifiable
	}
	var currentCacheHit float64
	if err := pool.QueryRow(ctx, cacheHitRatioSQL).Scan(&currentCacheHit); err != nil {
		return regressionUnverifiable
	}
	dropPct := ((beforeCacheHit - currentCacheHit) / beforeCacheHit) * 100
	if dropPct > float64(thresholdPct) {
		return regressionDetected
	}
	if beforeMeanMs <= 0 {
		return regressionNone
	}
	var currentMeanMs float64
	if err := pool.QueryRow(ctx, writeLatencySQL).Scan(&currentMeanMs); err != nil {
		return regressionUnverifiable
	}
	if currentMeanMs > 0 && ((currentMeanMs-beforeMeanMs)/beforeMeanMs)*100 > 20.0 {
		return regressionDetected
	}
	return regressionNone
}

// perQueryRegression compares each targeted query's latency before vs after
// the action. When no query has data in both windows the result is
// unverifiable rather than a silent pass.
func perQueryRegression(
	ctx context.Context, pool *pgxpool.Pool, queryIDs []int64,
	executedAt time.Time, thresholdPct int,
) regressionVerdict {
	const baselineWindow = 30 * time.Minute
	measured := 0
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
		measured++
		if isQueryRegressed(baseline, current, thresholdPct) {
			return regressionDetected
		}
	}
	if measured == 0 {
		return regressionUnverifiable
	}
	return regressionNone
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

// CheckHysteresis returns true if this finding — or an earlier finding with
// the same category and object — was rolled back (or its rollback was
// withheld or failed) within the cooldown. Findings are re-inserted with new
// ids after resolution, so the id alone is not a stable identity.
func CheckHysteresis(
	ctx context.Context, pool *pgxpool.Pool, findingID int64, cooldownDays int,
) bool {
	var one int
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT 1 FROM sage.action_log al
		 LEFT JOIN sage.findings prev ON prev.id = al.finding_id
		 WHERE al.outcome IN ('rolled_back', 'rollback_failed', 'rollback_skipped')
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

// updateActionOutcome sets the outcome and rollback_reason for an action.
func updateActionOutcome(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, outcome, reason string,
) {
	_, _ = pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.action_log
		 SET outcome = $1, rollback_reason = $2, measured_at = now()
		 WHERE id = $3`,
		outcome, reason, actionID,
	)
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
	tag, err := pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_log
		 SET outcome = $1, rollback_reason = $2, measured_at = now()
		 WHERE id = $3 AND outcome IN `+monitorableOutcomes,
		outcome, reason, actionID)
	return err == nil && tag.RowsAffected() == 1
}

// updateActionSuccess marks an action as successful and snapshots the
// current state as after_state. Terminal rollback/failure outcomes are never
// overwritten, and value is credited only when this call made the change.
func updateActionSuccess(ctx context.Context, pool *pgxpool.Pool, actionID int64) {
	var cacheHit float64
	_ = pool.QueryRow(ctx, cacheHitRatioSQL).Scan(&cacheHit)
	tag, err := pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.action_log
		 SET outcome = 'success',
		     after_state = jsonb_build_object('cache_hit_ratio', $1::float8),
		     measured_at = now()
		 WHERE id = $2 AND outcome NOT IN ('rolled_back', 'rolling_back',
		       'rollback_failed', 'rollback_skipped', 'failed', 'unverifiable')`,
		cacheHit, actionID,
	)
	if err != nil || tag.RowsAffected() != 1 {
		return
	}
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
	return verificationID, err
}
