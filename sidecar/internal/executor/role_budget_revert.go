package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/verify"
)

// market-08b: an enforced role statement_timeout is judged by the real
// statement-timeout cancels its budget's ledger counts from the server log
// (sage.budget_shadow.actual_cancels) against the shadow prediction, and
// reverted through the rollback path when they exceed it twice over
// (budget.RevertCheck) within 24 hours of apply.

// cancelWatch is what the budget monitor put in the finding
// (detail.cancel_watch) plus the ledger's count when the action ran,
// kept in before_state.cancel_watch.
type cancelWatch struct {
	Budget           string  `json:"budget"`
	Definition       string  `json:"definition,omitempty"`
	LedgerDatabase   string  `json:"ledger_database"`
	Role             string  `json:"role"`
	PredictedPerHour float64 `json:"predicted_per_hour"`
	PredictionDays   int     `json:"prediction_days,omitempty"`
	Source           string  `json:"source"`
	ActualAtApply    int64   `json:"actual_at_apply"`
}

// revertGrace is how long past the 24 h watch a late check still judges
// the cancels (a check is due every 5 minutes).
const revertGrace = 10 * time.Minute

// isRoleStatementTimeout reports a role budget's statement_timeout
// enforcement (a SET, not a RESET or another setting).
func isRoleStatementTimeout(sql string) bool {
	stmt, ok := parseRoleBudgetSQL(sql)
	return ok && !stmt.ConnLimit && !stmt.Reset && stmt.Name == budget.SettingStatementTimeout
}

// recordCancelWatch copies the finding's cancel watch into before with
// the ledger's count of the apply day, so only later cancels count.
func (e *Executor) recordCancelWatch(ctx context.Context, detail, before map[string]any) {
	raw, ok := detail["cancel_watch"]
	if !ok || e.pool == nil {
		return
	}
	encoded, err := json.Marshal(raw)
	var w cancelWatch
	if err != nil || json.Unmarshal(encoded, &w) != nil || w.Budget == "" {
		e.logFn("executor", "malformed cancel_watch in finding detail; the timeout is "+
			"not judged by its cancels")
		return
	}
	err = e.pool.QueryRow(ctx, `/* pg_sage executor */
SELECT COALESCE(max(actual_cancels), 0) FROM sage.budget_shadow
 WHERE database_name = $1 AND budget = $2 AND day = $3::date`, w.LedgerDatabase, w.Budget,
		time.Now().UTC()).Scan(&w.ActualAtApply)
	if err != nil {
		e.logFn("executor", "read budget %q's cancels at apply: %v", w.Budget, err)
	}
	before["cancel_watch"] = w
}

// cancelsSince sums the budget's ledger cancels from the apply day, net
// of the count at apply; lower marks a lower bound and source is the
// latest day's cancel source.
func cancelsSince(ctx context.Context, pool *pgxpool.Pool, w cancelWatch,
	executedAt time.Time) (actual int64, lower bool, source string, err error) {
	err = pool.QueryRow(ctx, `/* pg_sage executor */
SELECT COALESCE(sum(n), 0)::bigint, COALESCE(bool_or(lb), false),
       COALESCE((array_agg(src ORDER BY day DESC))[1], 'none')
  FROM (SELECT day, max(actual_cancels) AS n, bool_or(cancels_lower_bound) AS lb,
               max(cancel_source) AS src
          FROM sage.budget_shadow
         WHERE database_name = $1 AND budget = $2 AND day >= $3::date
         GROUP BY day) d`, w.LedgerDatabase, w.Budget, executedAt.UTC()).
		Scan(&actual, &lower, &source)
	if err != nil {
		return 0, false, "", fmt.Errorf("read budget %q's cancels: %w", w.Budget, err)
	}
	return max(actual-w.ActualAtApply, 0), lower, source, nil
}

// judgeCancels is the class metric of an enforced statement_timeout:
// regressed past twice the prediction within 24 hours of apply, improved
// once 24 hours passed within it, insufficient (still watching) before.
func (p monitorPlan) judgeCancels(
	ctx context.Context, pool *pgxpool.Pool, now time.Time, evidence map[string]any,
) *metricJudgement {
	w := *p.cancels
	metric := verify.MetricStatementTimeoutCancels
	actual, lower, source, err := cancelsSince(ctx, pool, w, p.executedAt)
	if err != nil {
		return &metricJudgement{Verdict: verify.OutcomeInsufficient, Metric: metric,
			Reason: "statement-timeout cancels unavailable: " + err.Error()}
	}
	elapsed := now.Sub(p.executedAt)
	check := budget.RevertCheck{Actual: actual, PredictedPerHour: w.PredictedPerHour,
		Elapsed: min(elapsed, budget.RevertWatch), LowerBound: lower}
	revert, reason := check.Decide()
	evidence["cancels"] = map[string]any{"budget": w.Budget, "role": w.Role,
		"actual_cancels": actual, "elapsed_hours": check.Elapsed.Hours(),
		"predicted_per_hour": w.PredictedPerHour, "predicted": check.Predicted(),
		"threshold": check.Threshold(), "lower_bound": lower, "cancel_source": source}
	j := &metricJudgement{Metric: metric, Before: check.Predicted(), After: float64(actual),
		Reason: reason}
	switch {
	case elapsed > budget.RevertWatch+revertGrace:
		j.Verdict, j.Reason = lateVerdict(check, source, elapsed)
		j.Terminal = true
	case revert:
		j.Verdict = verify.OutcomeRegressed
	case source != budget.CancelSourceLog:
		j.Verdict = verify.OutcomeInsufficient
		j.Reason = reason + "; the server log is not read now, so later cancels are unknown"
	case elapsed >= budget.RevertWatch:
		j.Verdict, j.Terminal = verify.OutcomeImproved, true
	default:
		j.Verdict = verify.OutcomeInsufficient
	}
	return j
}

// lateVerdict settles a check past the 24 h watch (a late or resumed
// monitor): the rule no longer reverts, and the timeout is credited only
// when the server log was read and its cancels stayed within the
// threshold; otherwise it is left uncredited for an operator to review.
func lateVerdict(check budget.RevertCheck, source string, elapsed time.Duration) (string,
	string) {
	numbers := fmt.Sprintf("%d cancels by %.1f h after apply, threshold %.0f",
		check.Actual, elapsed.Hours(), check.Threshold())
	switch {
	case source != budget.CancelSourceLog:
		return verify.OutcomeInsufficient, numbers + "; the 24 h watch ended and the " +
			"server log is not read now, so its cancels are unproven: not credited"
	case float64(check.Actual) > check.Threshold():
		return verify.OutcomeInsufficient, numbers + "; past twice the prediction but " +
			"checked after the 24 h watch ended, so it is not reverted automatically: " +
			"review it"
	}
	return verify.OutcomeImproved, fmt.Sprintf("the enforced timeout cancelled %d "+
		"statements in the 24 h watch, within twice the prediction", check.Actual)
}

// lockRoleRollback takes the cluster role lock of a role budget rollback,
// as the forward change took it; release is a no-op for other rollbacks.
func (c RollbackMonitorConfig) lockRoleRollback(ctx context.Context, sql string) (func(),
	error) {
	stmt, ok := parseRoleBudgetSQL(sql)
	if !ok || c.RoleLock == nil {
		return func() {}, nil
	}
	release, err := c.RoleLock(ctx, stmt.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: cluster lock for role %s: %v", ErrRoleBudgetRefused,
			stmt.Role, err)
	}
	return release, nil
}

// execRollbackStatement runs a rollback statement, a role budget's under
// its role lock with a privilege refusal (42501) named as such.
func (c RollbackMonitorConfig) execRollbackStatement(
	ctx context.Context, pool *pgxpool.Pool, sql string, timeout time.Duration,
) error {
	release, err := c.lockRoleRollback(ctx, sql)
	if err != nil {
		return err
	}
	defer release()
	err = c.exec(pool)(ctx, sql, timeout, WithLockTimeout(c.LockTimeoutMs))
	if roleBudgetActionType(sql) != "" {
		return roleBudgetExecError(sql, err)
	}
	return err
}
