package executor

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/value"
	"github.com/pg-sage/sidecar/internal/verify"
)

// RollbackMonitorConfig configures one post-action monitor. Authorize is
// required: a nil authorizer withholds every automatic rollback.
type RollbackMonitorConfig struct {
	ThresholdPct     int // regression bar on the targets' call-weighted mean
	WindowMinutes    int // minimum window before the first verdict
	CapMinutes       int // longest a non-drop action is watched for evidence
	MinCalls         int // calls each window needs (verify.min_samples)
	GainPct          float64
	DropWindow       time.Duration // an index drop's business cycle
	StatementTimeout time.Duration
	LockTimeoutMs    int
	CloudEnvironment string
	Authorize        func(context.Context, string) bool
	Acquire          func(context.Context) (func(), error)

	execRollback func(context.Context, string, time.Duration, ...DDLOption) error
	applyConfig  func(context.Context, string) configApplyOutcome
	observe      queryObserver
}

// queryObserver reads the targeted queries' measurements over a window.
type queryObserver func(context.Context, []int64, time.Time, time.Time) (
	map[int64]verify.Measurement, error)

func (c RollbackMonitorConfig) observer(pool *pgxpool.Pool) queryObserver {
	if c.observe != nil {
		return c.observe
	}
	return verify.NewPostgresObservationSource(pool).QueryMeasurements
}

// MonitorAndRollback runs as a goroutine to verify an executed action
// against what it targeted (Phase 1.3). It checks the action on its
// class's schedule: a regression is rolled back at once (for an index
// drop that re-creates the kept definition: the soft drop); otherwise the
// verdict is recorded once the minimum window passed with evidence, or at
// the cap. Missing evidence is never success, and an action already in a
// terminal state (an operator rollback) is never judged or overwritten.
func MonitorAndRollback(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
	rollbackSQL string,
	cfg RollbackMonitorConfig,
	logFn func(string, string, ...any),
	shutdownCh <-chan struct{},
) {
	if pool == nil {
		logFn("rollback", "no database for action %d; not monitored", actionID)
		return
	}
	plan, err := loadMonitorPlan(ctx, pool, actionID, cfg)
	if err != nil {
		logFn("rollback", "action %d not monitored: %v", actionID, err)
		return
	}
	if !plan.monitorable {
		logFn("rollback", "action %d already settled; not monitored", actionID)
		return
	}
	if rollbackSQL != "" {
		plan.rollbackSQL = rollbackSQL
	}
	next := plan.firstCheck()
	for {
		if !waitRollbackWindow(ctx, pool, actionID, time.Until(next), logFn, shutdownCh) {
			return
		}
		now := time.Now()
		j := plan.judge(ctx, pool, cfg, now)
		if j.outcome.Verdict == verify.OutcomeRegressed {
			finishRegressed(ctx, pool, plan, j.outcome, cfg, logFn)
			return
		}
		if plan.final(now, j.outcome.Verdict) || !j.accruing &&
			j.outcome.Verdict == verify.OutcomeInsufficient {
			finishMonitored(ctx, pool, plan, j.outcome, logFn)
			return
		}
		next = plan.nextCheck(now)
	}
}

// finishRegressed rolls a regressed action back through the existing
// rollback path, then records the verdict with whether the rollback (for
// a drop: the soft-drop re-create) took effect.
func finishRegressed(
	ctx context.Context, pool *pgxpool.Pool, plan monitorPlan, o verify.Outcome,
	cfg RollbackMonitorConfig, logFn func(string, string, ...any),
) {
	logFn("rollback", "action %d regressed: %s", plan.actionID, o.Reason)
	rollbackRegressedAction(ctx, pool, plan.actionID, plan.rollbackSQL, cfg, logFn)
	var outcome string
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT outcome FROM sage.action_log
		WHERE id = $1`, plan.actionID).Scan(&outcome)
	restored := err == nil && (outcome == "rolled_back" || outcome == "already_restored")
	o.Evidence["rollback"] = map[string]any{"outcome": outcome, "restored": restored}
	if soft, ok := o.Evidence["soft_drop"].(map[string]any); ok {
		soft["recreated"] = restored
	}
	recordVerdict(ctx, pool, o, logFn)
}

// finishMonitored records a kept action's verdict (and, for a config
// change, its metric in after_state.config_outcome).
func finishMonitored(
	ctx context.Context, pool *pgxpool.Pool, plan monitorPlan, o verify.Outcome,
	logFn func(string, string, ...any),
) {
	if plan.isConfig {
		detail := map[string]any{"verified": o.Verdict == verify.OutcomeImproved,
			"verdict": o.Verdict, "reason": o.Reason}
		if plan.config != nil {
			detail["metric"] = plan.config.Metric
		}
		if err := mergeAfterState(ctx, pool, plan.actionID, "config_outcome",
			detail); err != nil {
			logFn("rollback", "record config outcome for action %d: %v", plan.actionID, err)
		}
	}
	settleOutcome(ctx, pool, o, logFn)
}

func waitRollbackWindow(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, window time.Duration,
	logFn func(string, string, ...any), shutdownCh <-chan struct{},
) bool {
	timer := time.NewTimer(max(window, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		logFn("rollback", "context cancelled for action %d", actionID)
		return false
	case <-shutdownCh:
		logFn("rollback",
			"shutdown before rollback window for action %d — "+
				"leaving action in pending state", actionID)
		setMonitoredOutcome(ctx, pool, actionID, "interrupted",
			"sidecar shutdown before rollback window elapsed")
		return false
	case <-timer.C:
		return true
	}
}

func rollbackRegressedAction(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, rollbackSQL string,
	cfg RollbackMonitorConfig, logFn func(string, string, ...any),
) {
	if CheckEmergencyStop(ctx, pool) {
		logFn("rollback", "emergency stop active — skipping auto-rollback for "+
			"action %d (manual rollback required)", actionID)
		setMonitoredOutcome(ctx, pool, actionID, "rollback_skipped",
			"emergency stop active; automatic rollback withheld")
		return
	}
	if cfg.Authorize == nil || !cfg.Authorize(ctx, rollbackSQL) {
		logFn("rollback", "policy withheld rollback for action %d", actionID)
		setMonitoredOutcome(ctx, pool, actionID, "rollback_skipped",
			"standing policy withheld automatic rollback")
		return
	}
	if !setMonitoredOutcome(ctx, pool, actionID, "rolling_back", "regression detected") {
		logFn("rollback", "action %d changed state; rollback not repeated", actionID)
		return
	}
	logFn("rollback", "regression detected for action %d, executing rollback", actionID)
	if restoredOutside(ctx, pool, actionID, rollbackSQL, logFn) {
		return
	}
	reason, err := executeRollbackSQL(ctx, pool, rollbackSQL, cfg)
	if err != nil && restoredOutside(ctx, pool, actionID, rollbackSQL, logFn) {
		return // recreated between the check and the DDL
	}
	if err != nil {
		logFn("rollback", "rollback failed for action %d: %v", actionID, err)
		updateActionOutcome(ctx, pool, actionID, "rollback_failed",
			"rollback execution failed: "+err.Error())
		return
	}
	updateActionOutcome(ctx, pool, actionID, "rolled_back", reason)
	_, _ = finalizeActionVerification(ctx, pool, actionID, "revert", reason)
	_, _ = value.NewPostgresRepository(pool).ZeroCreditOnRevert(ctx, actionID, "rolled_back")
}

// executeRollbackSQL runs rollback DDL concurrently where possible, under a
// lock timeout and the shared DDL slot, and reloads config after a GUC
// rollback so the reverted value is actually live.
func executeRollbackSQL(
	ctx context.Context, pool *pgxpool.Pool, rollbackSQL string, cfg RollbackMonitorConfig,
) (string, error) {
	if cfg.Acquire != nil {
		release, err := cfg.Acquire(ctx)
		if err != nil {
			return "", err
		}
		defer release()
	}
	sql := concurrentIndexRollbackSQL(rollbackSQL)
	timeout := cfg.StatementTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	if err := cfg.exec(pool)(ctx, sql, timeout, WithLockTimeout(cfg.LockTimeoutMs)); err != nil {
		return "", err
	}
	reason := "automatic rollback due to regression"
	if isAlterSystem(sql) {
		reason += "; " + cfg.reload(pool)(ctx, sql).Note
	}
	return reason, nil
}

func (c RollbackMonitorConfig) exec(
	pool *pgxpool.Pool,
) func(context.Context, string, time.Duration, ...DDLOption) error {
	if c.execRollback != nil {
		return c.execRollback
	}
	return func(ctx context.Context, sql string, timeout time.Duration, opts ...DDLOption) error {
		if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
			return ExecConcurrently(ctx, pool, sql, timeout, opts...)
		}
		return ExecInTransaction(ctx, pool, sql, timeout, opts...)
	}
}

func (c RollbackMonitorConfig) reload(
	pool *pgxpool.Pool,
) func(context.Context, string) configApplyOutcome {
	if c.applyConfig != nil {
		return c.applyConfig
	}
	return func(ctx context.Context, sql string) configApplyOutcome {
		return applyConfigChange(ctx, pool, sql, c.CloudEnvironment, nil)
	}
}

var indexDDLPrefix = regexp.MustCompile(
	`(?is)^(\s*(?:CREATE\s+(?:UNIQUE\s+)?|DROP\s+)INDEX\s+)(.*)$`)

// concurrentIndexRollbackSQL rewrites CREATE/DROP INDEX rollbacks (e.g. a
// pg_get_indexdef() rebuild) to their CONCURRENTLY form so a rollback never
// takes a write-blocking lock on the table.
func concurrentIndexRollbackSQL(sql string) string {
	match := indexDDLPrefix.FindStringSubmatch(sql)
	if len(match) != 3 || strings.HasPrefix(strings.ToUpper(match[2]), "CONCURRENTLY") {
		return sql
	}
	return match[1] + "CONCURRENTLY " + match[2]
}
