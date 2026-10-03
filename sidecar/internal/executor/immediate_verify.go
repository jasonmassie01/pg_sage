package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// maintenanceGainShare is the share of the baseline VACUUM/ANALYZE must
// remove to be an improvement.
const maintenanceGainShare = 0.5

// statsSettle bounds how long the post-action read waits for the
// statistics to show the action (PG14 reports through the collector).
const statsSettle = 3 * time.Second

// verifyImmediate verifies an action that has no rollback, right after it
// ran: VACUUM by the dead tuples it removed, ANALYZE by the modifications
// it reset. Anything else has no post-action measurement and is
// unverifiable (never "success" by default).
func (e *Executor) verifyImmediate(ctx context.Context, actionID int64) {
	if actionID <= 0 || e.pool == nil {
		return
	}
	o, table, err := loadImmediate(ctx, e.pool, actionID)
	if err != nil {
		e.logFn("verify", "verify action %d: %v", actionID, err)
		return
	}
	switch {
	case o.Class != verify.ClassVacuum && o.Class != verify.ClassAnalyze:
		o.Verdict = verify.OutcomeUnverifiable
		o.Reason = "no post-action measurement for this kind of action: not credited"
	case o.Predicted.Baseline == nil || table == "":
		o.Verdict = verify.OutcomeUnverifiable
		o.Reason = "no pre-action baseline was recorded: not credited"
	default:
		before := *o.Predicted.Baseline
		after := settledMaintenanceMetric(ctx, e.pool, o.Predicted.Metric, table, before)
		j := judgeMaintenanceMetric(o.Class, o.Predicted.Metric, before, after)
		o.Verdict, o.Reason = j.Verdict, j.Reason
		o.Observed = verify.Observed{Metric: o.Predicted.Metric, Before: before,
			After: after, ChangePct: j.ObservedPct}
	}
	end := time.Now()
	o.WindowEnd = &end
	settleOutcome(ctx, e.pool, o, e.logFn)
}

func loadImmediate(
	ctx context.Context, pool *pgxpool.Pool, actionID int64,
) (verify.Outcome, string, error) {
	var sql string
	var raw []byte
	var executedAt time.Time
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT sql_executed, executed_at,
		COALESCE(before_state, '{}'::jsonb) FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&sql, &executedAt, &raw)
	if err != nil {
		return verify.Outcome{}, "", fmt.Errorf("load action: %w", err)
	}
	var state struct {
		Predicted *verify.Prediction `json:"predicted_effect"`
		Table     string             `json:"maintenance_table"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return verify.Outcome{}, "", fmt.Errorf("decode before_state: %w", err)
	}
	class := verificationClass(sql)
	o := verify.Outcome{ActionLogID: actionID, Class: class, WindowStart: &executedAt,
		Predicted: verify.NoPrediction(class, "the action was recorded without one")}
	if state.Predicted != nil {
		o.Predicted = *state.Predicted
	}
	return o, state.Table, nil
}

// settledMaintenanceMetric reads the metric after the action, waiting up
// to statsSettle for the statistics to move off the baseline; -1 when it
// cannot be read.
func settledMaintenanceMetric(
	ctx context.Context, pool *pgxpool.Pool, metric, table string, before float64,
) float64 {
	deadline := time.Now().Add(statsSettle)
	for {
		after, err := readMaintenanceMetric(ctx, pool, metric, table)
		if err != nil {
			return -1
		}
		if after != before || time.Now().After(deadline) {
			return after
		}
		select {
		case <-ctx.Done():
			return after
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// maintenanceMetricSQL reads each maintenance metric of a table ($1).
var maintenanceMetricSQL = map[string]string{
	verify.MetricDeadTuples: `/* pg_sage */ SELECT n_dead_tup::float8
		FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
	verify.MetricModsSinceAnalyze: `/* pg_sage */ SELECT n_mod_since_analyze::float8
		FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
	verify.MetricFrozenXIDAge: `/* pg_sage */ SELECT age(relfrozenxid)::float8
		FROM pg_class WHERE oid = to_regclass($1)`,
}

// readMaintenanceMetric reads a table's dead tuples (VACUUM), rows
// modified since the last analyze (ANALYZE) or relfrozenxid age (VACUUM
// FREEZE).
func readMaintenanceMetric(
	ctx context.Context, pool *pgxpool.Pool, metric, table string,
) (float64, error) {
	query, ok := maintenanceMetricSQL[metric]
	if !ok {
		return 0, fmt.Errorf("no maintenance metric %q", metric)
	}
	var value float64
	if err := pool.QueryRow(ctx, query, table).Scan(&value); err != nil {
		return 0, fmt.Errorf("read %s of %s: %w", metric, table, err)
	}
	return value, nil
}

// judgeMaintenance decides VACUUM/ANALYZE on the class's default metric.
func judgeMaintenance(class string, before, after float64) metricJudgement {
	metric := verify.MetricDeadTuples
	if class == verify.ClassAnalyze {
		metric = verify.MetricModsSinceAnalyze
	}
	return judgeMaintenanceMetric(class, metric, before, after)
}

// judgeMaintenanceMetric decides VACUUM/ANALYZE from the metric before
// and after: improved when at least half of it went, neutral when less did
// (an old snapshot holding dead tuples back), insufficient when there was
// nothing to remove.
func judgeMaintenanceMetric(class, metric string, before, after float64) metricJudgement {
	j := metricJudgement{Before: before, After: after, Metric: metric}
	switch {
	case class != verify.ClassVacuum && class != verify.ClassAnalyze:
		j.Verdict, j.Reason = verify.OutcomeUnverifiable, "no maintenance metric for "+class
		return j
	case before < 0 || after < 0:
		j.Verdict, j.Reason = verify.OutcomeUnverifiable, metric+" could not be read"
		return j
	case before < 1:
		j.Verdict, j.Reason = verify.OutcomeInsufficient, "nothing to remove before the "+
			"action ("+metric+" was 0)"
		return j
	}
	change := (after - before) * 100 / before
	j.ObservedPct = &change
	if after <= before*(1-maintenanceGainShare) {
		j.Verdict = verify.OutcomeImproved
		j.Reason = fmt.Sprintf("%s fell from %.0f to %.0f", metric, before, after)
		return j
	}
	j.Verdict = verify.OutcomeNeutral
	j.Reason = fmt.Sprintf("%s barely moved (%.0f -> %.0f)", metric, before, after)
	return j
}

// retentionVerdict judges a retention batch: improved when the verified
// batch deleted rows, neutral when it deleted none, regressed when its
// integrity check failed (rows outside the predicate or the bound).
func retentionVerdict(deleted, candidates int64, verifyErr error) metricJudgement {
	j := metricJudgement{Metric: verify.MetricRowsDeleted, Before: float64(candidates),
		After: float64(deleted)}
	switch {
	case verifyErr != nil:
		j.Verdict, j.Reason = verify.OutcomeRegressed, verifyErr.Error()
		return j
	case candidates <= 0:
		j.Verdict, j.Reason = verify.OutcomeInsufficient, "no candidate rows were counted"
		return j
	}
	change := -float64(deleted) * 100 / float64(candidates)
	j.ObservedPct = &change
	if deleted > 0 {
		j.Verdict = verify.OutcomeImproved
		j.Reason = fmt.Sprintf("deleted %d of %d candidate rows", deleted, candidates)
		return j
	}
	j.Verdict, j.Reason = verify.OutcomeNeutral, "the batch deleted no rows"
	return j
}
