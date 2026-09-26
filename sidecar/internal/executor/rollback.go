package executor

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/value"
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

// RollbackMonitorConfig configures one post-action monitor. Authorize is
// required: a nil authorizer withholds every automatic rollback.
type RollbackMonitorConfig struct {
	ThresholdPct     int
	WindowMinutes    int
	Delay            time.Duration // extra wait, used when resuming monitors
	StatementTimeout time.Duration
	LockTimeoutMs    int
	CloudEnvironment string
	Authorize        func(context.Context, string) bool
	Acquire          func(context.Context) (func(), error)

	execRollback func(context.Context, string, time.Duration, ...DDLOption) error
	applyConfig  func(context.Context, string) configApplyOutcome
}

func (c RollbackMonitorConfig) window() time.Duration {
	return c.Delay + time.Duration(c.WindowMinutes)*time.Minute
}

// MonitorAndRollback runs as a goroutine to monitor the effect of an
// executed action. After the window it evaluates a tri-state verdict:
// success is recorded only with evidence, missing evidence is recorded as
// unverifiable, and a regression is rolled back only when authorized and
// only if the action is still in a monitorable state (an operator rollback
// or other terminal outcome is never overwritten).
func MonitorAndRollback(
	ctx context.Context,
	pool *pgxpool.Pool,
	actionID int64,
	rollbackSQL string,
	cfg RollbackMonitorConfig,
	logFn func(string, string, ...any),
	shutdownCh <-chan struct{},
) {
	if !waitRollbackWindow(ctx, pool, actionID, cfg.window(), logFn, shutdownCh) {
		return
	}
	switch evaluateRegression(ctx, pool, actionID, cfg.ThresholdPct) {
	case regressionNone:
		logFn("rollback", "no regression for action %d, marking success", actionID)
		updateActionSuccess(ctx, pool, actionID)
	case regressionUnverifiable:
		logFn("rollback", "action %d has no usable post-action evidence", actionID)
		if setMonitoredOutcome(ctx, pool, actionID, "unverifiable",
			"post-action evidence unavailable; not credited") {
			_, _ = finalizeActionVerification(ctx, pool, actionID, "unverifiable",
				"post-action evidence unavailable")
		}
	case regressionDetected:
		rollbackRegressedAction(ctx, pool, actionID, rollbackSQL, cfg, logFn)
	}
}

func waitRollbackWindow(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, window time.Duration,
	logFn func(string, string, ...any), shutdownCh <-chan struct{},
) bool {
	timer := time.NewTimer(window)
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
	reason, err := executeRollbackSQL(ctx, pool, rollbackSQL, cfg)
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
