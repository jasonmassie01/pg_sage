package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// r2State is what the statistics and reindex verifiers read from an
// action's before_state (recorded by statisticsBaseline/reindexBaseline).
type r2State struct {
	StatisticsTable  string                 `json:"statistics_table"`
	EstimateBaseline *verify.EstimateSample `json:"estimate_baseline"`
	ReindexTarget    string                 `json:"reindex_target"`
	ReindexTable     bool                   `json:"reindex_table"`
	ReindexBytes     *int64                 `json:"reindex_bytes"`
	// AnalyzeMark is the table's last analyze time before the action
	// (analyzeMarkNever when it had none); absent in older rows.
	AnalyzeMark *string `json:"statistics_analyze_mark"`
}

// isR2Class reports a class judged by judgeR2.
func isR2Class(class string) bool {
	return class == verify.ClassStatistics || class == verify.ClassReindex
}

// judgeR2 judges a CREATE STATISTICS (row estimates and the targets'
// time) or a REINDEX (index size, validity and the table's queries).
// Without a prediction only a regression is recorded as such.
func (p monitorPlan) judgeR2(
	ctx context.Context, pool *pgxpool.Pool, now time.Time, queries *verify.Comparison,
	o verify.Outcome,
) judgement {
	var accruing bool
	if p.class == verify.ClassStatistics {
		est := p.judgeEstimates(ctx, pool, now, o.Evidence)
		o.Verdict, o.Reason, accruing = verify.DecideStatistics(queries, est)
		o.Observed = verify.Observed{Metric: verify.MetricRowEstimateError,
			Before: est.Before, After: est.After, ChangePct: est.ChangePct}
	} else {
		size := p.judgeIndexSize(ctx, pool, o.Evidence)
		o.Verdict, o.Reason, accruing = verify.DecideReindex(queries, size)
		o.Observed = verify.Observed{Metric: verify.MetricIndexBytes,
			Before: size.Before, After: size.After, ChangePct: size.ChangePct}
	}
	if !p.prediction.Predicts() && o.Verdict != verify.OutcomeRegressed {
		o.Verdict, accruing = verify.OutcomeUnverifiable, false
		o.Reason = "no prediction (" + nonEmpty(p.prediction.Note, "none recorded") +
			"): not credited"
	}
	return judgement{outcome: o, accruing: accruing}
}

// statisticsBuiltSQL is when the table was last analyzed: extended
// statistics are empty until then.
const statisticsBuiltSQL = `/* pg_sage */ SELECT GREATEST(last_analyze, last_autoanalyze)
	FROM pg_stat_user_tables WHERE relid = to_regclass($1)`

// judgeEstimates compares the targets' row-estimate error before the
// action (frozen baseline) with the plans sampled since the statistics
// were built (the first ANALYZE of the table after the action).
func (p monitorPlan) judgeEstimates(
	ctx context.Context, pool *pgxpool.Pool, now time.Time, evidence map[string]any,
) verify.EstimateJudgement {
	var before verify.EstimateSample
	if p.r2.EstimateBaseline != nil {
		before = *p.r2.EstimateBaseline
	}
	if before.Plans == 0 {
		return verify.JudgeEstimates(before, verify.EstimateSample{}, 0)
	}
	var built *time.Time
	if err := pool.QueryRow(ctx, statisticsBuiltSQL, p.r2.StatisticsTable).
		Scan(&built); err != nil {
		return verify.EstimateJudgement{Verdict: verify.OutcomeInsufficient,
			Reason: fmt.Sprintf("analyze time of %s unavailable: %v", p.r2.StatisticsTable,
				err)}
	}
	if !p.builtSinceAction(built) {
		return verify.EstimateJudgement{Verdict: verify.OutcomeInsufficient,
			Reason: "statistics not built yet: no ANALYZE of " + p.r2.StatisticsTable +
				" since the action"}
	}
	after, err := verify.NewPostgresObservationSource(pool).EstimateErrors(ctx, p.targets,
		*built, now)
	if err != nil {
		return verify.EstimateJudgement{Verdict: verify.OutcomeInsufficient,
			Reason: "plans after the action unavailable: " + err.Error()}
	}
	j := verify.JudgeEstimates(before, after, 0)
	evidence["estimates"] = map[string]any{"before": before, "after": after,
		"after_from": built.UTC(), "verdict": j.Verdict, "reason": j.Reason}
	return j
}

// judgeIndexSize compares the REINDEX target's size now with the size
// recorded before the action, and checks the rebuilt index is valid.
func (p monitorPlan) judgeIndexSize(
	ctx context.Context, pool *pgxpool.Pool, evidence map[string]any,
) verify.SizeJudgement {
	if p.r2.ReindexTarget == "" || p.r2.ReindexBytes == nil {
		j := verify.JudgeIndexSize(0, -1, false)
		j.Reason = "no index size was recorded before the action"
		return j
	}
	after, valid, err := verify.NewPostgresObservationSource(pool).IndexFootprint(ctx,
		p.r2.ReindexTarget, p.r2.ReindexTable)
	if err != nil {
		j := verify.JudgeIndexSize(*p.r2.ReindexBytes, -1, false)
		j.Reason = "index size unavailable: " + err.Error()
		return j
	}
	j := verify.JudgeIndexSize(*p.r2.ReindexBytes, after, valid)
	evidence["index"] = map[string]any{"target": p.r2.ReindexTarget,
		"table": p.r2.ReindexTable, "before_bytes": *p.r2.ReindexBytes,
		"after_bytes": after, "valid": valid}
	return j
}

// finishUnrollable records the regression of an action that has no
// rollback (a REINDEX): nothing is executed; the verdict is the trust
// ledger's demerit.
func finishUnrollable(
	ctx context.Context, pool *pgxpool.Pool, plan monitorPlan, o verify.Outcome,
	logFn func(string, string, ...any),
) {
	reason := "regressed; this action has no rollback, nothing was undone: " + o.Reason
	if !setMonitoredOutcome(ctx, pool, plan.actionID, "rollback_skipped", reason) {
		logFn("rollback", "action %d changed state; regression not recorded", plan.actionID)
		return
	}
	o.Evidence["rollback"] = map[string]any{"outcome": "none", "restored": false}
	recordVerdict(ctx, pool, o, logFn)
	if _, err := finalizeActionVerification(ctx, pool, plan.actionID,
		verificationVerdictFor(o.Verdict), reason); err != nil {
		logFn("rollback", "finalize verification of action %d: %v", plan.actionID, err)
	}
}

// analyzeMarkNever marks a table never analyzed before the action.
const analyzeMarkNever = "never"

// analyzeMark is the table's last analyze time now, in PostgreSQL's clock
// (RFC 3339), recorded before a CREATE STATISTICS runs.
func (e *Executor) analyzeMark(ctx context.Context, table string) string {
	var built *time.Time
	if err := e.pool.QueryRow(ctx, statisticsBuiltSQL, table).Scan(&built); err != nil {
		e.logFn("executor", "analyze time of %s unavailable: %v", table, err)
		return ""
	}
	if built == nil {
		return analyzeMarkNever
	}
	return built.UTC().Format(time.RFC3339Nano)
}

// builtSinceAction reports an ANALYZE of the table after the action
// started. The executor's own ANALYZE runs before action_log.executed_at
// is stamped, so the pre-action mark decides when it was recorded; rows
// without one fall back to executed_at.
func (p monitorPlan) builtSinceAction(built *time.Time) bool {
	if built == nil {
		return false
	}
	if p.r2.AnalyzeMark != nil {
		switch mark := *p.r2.AnalyzeMark; mark {
		case analyzeMarkNever:
			return true
		case "":
		default:
			if at, err := time.Parse(time.RFC3339Nano, mark); err == nil {
				return built.After(at)
			}
		}
	}
	return !built.Before(p.executedAt)
}
