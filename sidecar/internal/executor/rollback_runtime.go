package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// rollbackMonitorConfig builds the monitor settings shared by every path.
// authorize is mandatory; rollbacks always use the lock timeout and the
// shared DDL semaphore.
func (e *Executor) rollbackMonitorConfig(
	authorize func(context.Context, string) bool,
) RollbackMonitorConfig {
	cfg, _, _ := e.policySnapshot()
	result := RollbackMonitorConfig{Authorize: authorize, Acquire: e.acquireDDLSlot}
	if cfg != nil {
		result.ThresholdPct = cfg.Trust.RollbackThresholdPct
		result.WindowMinutes = cfg.Trust.RollbackWindowMinutes
		result.CapMinutes = cfg.Verify.WindowMaxMinutes
		result.MinCalls = cfg.Verify.MinSamples
		result.GainPct = cfg.Verify.MinGainPct
		result.DropWindow = cfg.Verify.DropWindow()
		result.StatementTimeout = cfg.Safety.DDLTimeout()
		result.LockTimeoutMs = cfg.Safety.LockTimeout()
		result.CloudEnvironment = cfg.CloudEnvironment
	}
	return result
}

// manualRollbackAuthorizer re-checks the operator mutation gates (executor
// enabled, trust level, emergency stop) at rollback time for actions that
// were approved or run manually.
func (e *Executor) manualRollbackAuthorizer() func(context.Context, string) bool {
	return func(ctx context.Context, _ string) bool {
		return e.manualMutationBlock(ctx) == nil
	}
}

// standingRollbackAuthorizer authorizes an automatic rollback through the
// standing gate, like the forward action.
func (e *Executor) standingRollbackAuthorizer(
	finding analyzer.Finding,
) func(context.Context, string) bool {
	return func(ctx context.Context, rollbackSQL string) bool {
		candidate := finding
		candidate.RecommendedSQL = rollbackSQL
		decision := e.evaluateFindingPolicy(ctx, candidate, false)
		return decision.Decision == PolicyDecisionExecute
	}
}

// acquireDDLSlot takes one slot of the executor-wide DDL semaphore shared by
// RunCycle, manual/approved execution, custodians and rollbacks.
func (e *Executor) acquireDDLSlot(ctx context.Context) (func(), error) {
	if e.ddlSem == nil {
		return func() {}, nil
	}
	select {
	case e.ddlSem <- struct{}{}:
		return func() { <-e.ddlSem }, nil
	default:
	}
	select {
	case e.ddlSem <- struct{}{}:
		return func() { <-e.ddlSem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a DDL slot: %w", ctx.Err())
	}
}

// resumeOrphanedMonitors restarts legacy rollback monitors for actions left
// in monitoring/interrupted after a restart. Actions watched by the durable
// verification engine are excluded; it resumes those itself.
func (e *Executor) resumeOrphanedMonitors(ctx context.Context) error {
	if e.pool == nil {
		return nil
	}
	rows, err := e.pool.Query(ctx, `/* pg_sage */ SELECT al.id, al.rollback_sql, al.sql_executed,
		COALESCE(al.decision_id, 0), al.executed_at
		FROM sage.action_log al
		WHERE al.outcome IN ('monitoring', 'interrupted')
		  AND COALESCE(al.rollback_sql, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM sage.verification v
		                   WHERE v.action_log_id = al.id)
		ORDER BY al.id`)
	if err != nil {
		return fmt.Errorf("load orphaned monitors: %w", err)
	}
	type orphan struct {
		id, decisionID   int64
		rollbackSQL, sql string
		executedAt       time.Time
	}
	var orphans []orphan
	for rows.Next() {
		var item orphan
		if err := rows.Scan(&item.id, &item.rollbackSQL, &item.sql, &item.decisionID,
			&item.executedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan orphaned monitor: %w", err)
		}
		orphans = append(orphans, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read orphaned monitors: %w", err)
	}
	for _, item := range orphans {
		e.resumeMonitor(ctx, item.id, item.decisionID, item.rollbackSQL, item.sql,
			item.executedAt)
	}
	return nil
}

// resumeMonitor restarts a monitor after a restart. Its schedule is
// relative to the action's execution, so it resumes where it was; one
// whose cap ended long ago is expired instead of judged.
func (e *Executor) resumeMonitor(
	ctx context.Context, actionID, decisionID int64, rollbackSQL, sql string,
	executedAt time.Time,
) {
	authorize := e.manualRollbackAuthorizer()
	if decisionID > 0 {
		authorize = e.standingRollbackAuthorizer(analyzer.Finding{RecommendedSQL: rollbackSQL})
	}
	cfg := e.rollbackMonitorConfig(authorize)
	_, capW, _ := monitorWindows(verificationClass(sql), cfg)
	if verificationExpired(executedAt, capW, time.Now()) {
		expireMonitor(ctx, e.pool, actionID, executedAt, capW, e.logFn)
		return
	}
	e.logFn("executor", "resuming rollback monitor for action %d", actionID)
	e.startRollbackMonitor(func() {
		MonitorAndRollback(context.WithoutCancel(ctx), e.pool, actionID, rollbackSQL,
			cfg, e.logFn, e.shutdownCh)
	})
}
