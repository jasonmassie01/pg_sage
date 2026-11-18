package executor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/value"
	"github.com/pg-sage/sidecar/internal/verify"
)

// RollbackMonitorConfig configures one post-action monitor. Authorize is
// required: a nil authorizer withholds every automatic rollback.
type RollbackMonitorConfig struct {
	// V2 computes verification v2 beside the monitor's v1 verdict, stored
	// as the other verdict (lab-01c); nil: v1 only.
	V2               *verify.V2Options
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
	// Decide, when set, replaces Authorize and names why a rollback is
	// withheld (no contract, a policy refusal, an SQL mismatch).
	Decide func(context.Context, string) RollbackVerdict
	// HoldForStop, when set, records a rollback the emergency stop withholds
	// as a visible approval card instead of skipping it (the soft-drop
	// restore, executor-01).
	HoldForStop func(ctx context.Context, actionID int64, rollbackSQL string)
	// QueueRevert, when set, offers a withheld rollback to a person as a
	// "Revert action N" approval item carrying the reason (policy-05).
	QueueRevert func(ctx context.Context, actionID int64, rollbackSQL string,
		verdict RollbackVerdict)
	Acquire func(context.Context) (func(), error)
	// Track registers the rollback as in-flight work the emergency stop
	// cancels; a latched stop refuses it.
	Track func(ctx context.Context, label string) (context.Context, func(), error)
	// ActionPool is the separate action identity the rollback statement
	// runs as; nil runs it on the monitor's pool (security-09).
	ActionPool *pgxpool.Pool
	// RoleLock, when set, takes the cluster-wide lock of a role a role
	// budget rollback changes, as the forward change took it (market-08b).
	RoleLock func(ctx context.Context, role string) (func(), error)

	execRollback func(context.Context, string, time.Duration, ...DDLOption) error
	applyConfig  func(context.Context, string) configApplyOutcome
	observe      queryObserver
}

// RollbackVerdict is an authorizer's answer for one automatic rollback.
type RollbackVerdict struct {
	Allowed bool
	// Reason is why the rollback was withheld, recorded as rollback_reason.
	Reason string
	// LockCeilingMS is the policy's lock ceiling on an allowed rollback; it
	// caps an in-transaction rollback's lock_timeout (0: none).
	LockCeilingMS int64
	// DecisionID is the gate decision behind the verdict (0: none).
	DecisionID int64
}

// standingWithheld is the reason of a refusal by a yes/no authorizer.
const standingWithheld = "standing policy withheld automatic rollback"

// verdict asks Decide, or else Authorize; neither withholds the rollback.
func (c RollbackMonitorConfig) verdict(ctx context.Context, sql string) RollbackVerdict {
	if c.Decide != nil {
		return c.Decide(ctx, sql)
	}
	if c.Authorize != nil && c.Authorize(ctx, sql) {
		return RollbackVerdict{Allowed: true}
	}
	return RollbackVerdict{Reason: standingWithheld}
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
	if plan.rollbackSQL == "" {
		finishUnrollable(ctx, pool, plan, o, logFn)
		return
	}
	rollbackRegressedAction(ctx, pool, plan.actionID, plan.rollbackSQL, cfg, logFn)
	var outcome string
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT outcome FROM sage.action_log
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
	// Scope a legacy hint DELETE first, so a rollback held for an
	// emergency stop or checked by policy is the scoped one (tuning-02a).
	rollbackSQL, err := scopeHintRollback(ctx, pool, actionID, rollbackSQL)
	if err != nil {
		logFn("rollback", "rollback refused for action %d: %v", actionID, err)
		setMonitoredOutcome(ctx, pool, actionID, "rollback_skipped", err.Error())
		return
	}
	verdict, held := withheldRollback(ctx, pool, actionID, rollbackSQL, cfg, logFn)
	if held {
		return
	}
	cfg.LockTimeoutMs = rollbackLockTimeoutMS(rollbackSQL, cfg.LockTimeoutMs,
		verdict.LockCeilingMS)
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
		recordActionOutcome(ctx, pool, logFn, actionID, "rollback_failed",
			"rollback execution failed: "+err.Error())
		return
	}
	recordActionOutcome(ctx, pool, logFn, actionID, "rolled_back", reason)
	_, _ = finalizeActionVerification(ctx, pool, actionID, "revert", reason)
	_, _ = value.NewPostgresRepository(pool).ZeroCreditOnRevert(ctx, actionID, "rolled_back")
}

// withheldRollback records a rollback the emergency stop or the authorizer
// holds back and reports whether it was held, with the authorizer's
// verdict. The stop is checked first. A withheld rollback is offered to a
// person as a revert item (cfg.QueueRevert), so it never dead-ends.
func withheldRollback(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, rollbackSQL string,
	cfg RollbackMonitorConfig, logFn func(string, string, ...any),
) (RollbackVerdict, bool) {
	if CheckEmergencyStop(ctx, pool) {
		if cfg.HoldForStop != nil {
			cfg.HoldForStop(ctx, actionID, rollbackSQL)
			return RollbackVerdict{}, true
		}
		logFn("rollback", "emergency stop active — skipping auto-rollback for "+
			"action %d (manual rollback required)", actionID)
		skipRollback(ctx, pool, actionID, rollbackSQL,
			RollbackVerdict{Reason: stopWithheldReason}, cfg)
		return RollbackVerdict{}, true
	}
	verdict := cfg.verdict(ctx, rollbackSQL)
	if verdict.Allowed {
		return verdict, false
	}
	logFn("rollback", "rollback of action %d withheld: %s", actionID, verdict.Reason)
	skipRollback(ctx, pool, actionID, rollbackSQL, verdict, cfg)
	return verdict, true
}

// stopWithheldReason is the rollback_reason of a revert the emergency stop
// withheld.
const stopWithheldReason = "emergency stop active; automatic rollback withheld"

// skipRollback records the withheld rollback and, once it claimed the
// action, queues its revert item.
func skipRollback(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, rollbackSQL string,
	verdict RollbackVerdict, cfg RollbackMonitorConfig,
) {
	if setMonitoredOutcome(ctx, pool, actionID, "rollback_skipped", verdict.Reason) &&
		cfg.QueueRevert != nil {
		cfg.QueueRevert(ctx, actionID, rollbackSQL, verdict)
	}
}

// executeRollbackSQL runs rollback DDL concurrently where possible, under a
// lock timeout and the shared DDL slot, and reloads config after a GUC
// rollback so the reverted value is actually live.
func executeRollbackSQL(
	ctx context.Context, pool *pgxpool.Pool, rollbackSQL string, cfg RollbackMonitorConfig,
) (string, error) {
	ctx, release, err := cfg.enterRollback(ctx, rollbackSQL)
	if err != nil {
		return "", err
	}
	defer release()
	if cfg.ActionPool != nil {
		pool = cfg.ActionPool // the rollback is a change: the action identity runs it
	}
	sql := concurrentIndexRollbackSQL(rollbackSQL)
	timeout := cfg.StatementTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	if r := statisticsRun(ctx); r != nil && r.Rollback {
		c, err := extstats.ParseCreate(r.SQL)
		if err != nil {
			return "", err
		}
		if sql != c.Rollback() {
			return "", fmt.Errorf("statistics rollback mismatch")
		}
		o := applyDDLOpts([]DDLOption{WithLockTimeout(cfg.LockTimeoutMs)})
		o.before = r.guard
		err = execStatementsInTransaction(ctx, pool, timeout, o, sql)
		return "created statistics removed; ANALYZE statistics are not restored", err
	}
	if err := cfg.execRollbackStatement(ctx, pool, sql, timeout); err != nil {
		return "", err
	}
	reason := "automatic rollback due to regression"
	if isAlterSystem(sql) {
		reason += "; " + cfg.reload(pool)(ctx, sql).Note
	}
	if stmt, ok := pgconf.ParseAlterDatabase(normalizeSQLText(sql)); ok {
		state, note, _ := readDatabaseSetting(ctx, pool, stmt)
		if state != readbackInEffect {
			return "", fmt.Errorf("database setting rollback read-back: %s", note)
		}
		reason += "; " + note
	}
	return reason, nil
}

// enterRollback registers the rollback as in-flight work the emergency stop
// cancels, then takes the shared DDL slot; release undoes both.
func (c RollbackMonitorConfig) enterRollback(
	ctx context.Context, rollbackSQL string,
) (context.Context, func(), error) {
	done := func() {}
	if c.Track != nil {
		tracked, untrack, err := c.Track(ctx, rollbackSQL)
		if err != nil {
			return ctx, nil, err
		}
		ctx, done = tracked, untrack
	}
	if c.Acquire == nil {
		return ctx, done, nil
	}
	releaseSlot, err := c.Acquire(ctx)
	if err != nil {
		done()
		return ctx, nil, err
	}
	return ctx, func() { releaseSlot(); done() }, nil
}

func (c RollbackMonitorConfig) exec(
	pool *pgxpool.Pool,
) func(context.Context, string, time.Duration, ...DDLOption) error {
	if c.execRollback != nil {
		return c.execRollback
	}
	return func(ctx context.Context, sql string, timeout time.Duration, opts ...DDLOption) error {
		if statementRunsTopLevel(sql) {
			// A failed restore build is cleaned up by the remnant handler;
			// its outcome is named in the rollback's error.
			run := remnantRun{pool: pool, timeout: timeout, dropTimeout: timeout, opts: opts}
			_, err := run.exec(ctx, sql)
			return err
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
