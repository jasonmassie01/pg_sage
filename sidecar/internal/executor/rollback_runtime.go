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
	result := RollbackMonitorConfig{Authorize: authorize, Acquire: e.acquireDDLSlot,
		Track: e.trackRun, QueueRevert: e.queueRevert, ActionPool: e.actionPool,
		RoleLock: e.roleLock}
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

// standingRollbackAuthorizer is the yes/no view of standingRollbackDecider.
func (e *Executor) standingRollbackAuthorizer(
	finding analyzer.Finding,
) func(context.Context, string) bool {
	decide := e.standingRollbackDecider(finding)
	return func(ctx context.Context, rollbackSQL string) bool {
		return decide(ctx, rollbackSQL).Allowed
	}
}

// standingRollbackDecider authorizes an automatic rollback through the
// standing gate, like the forward action. A rollback no action contract
// classifies is withheld as such, before the gate, so the operator reads
// it apart from a policy refusal.
func (e *Executor) standingRollbackDecider(
	finding analyzer.Finding,
) func(context.Context, string) RollbackVerdict {
	return e.findingRollbackDecider(finding, 0)
}

// findingRollbackDecider is standingRollbackDecider for the revert of
// action actionID (0: unknown): the verdict carries the gate's lock
// ceiling and decision, and the decision names the reverted action
// (policy-05).
func (e *Executor) findingRollbackDecider(
	finding analyzer.Finding, actionID int64,
) func(context.Context, string) RollbackVerdict {
	return func(ctx context.Context, rollbackSQL string) RollbackVerdict {
		if _, ok := rollbackContractFor(finding.RecommendedSQL, rollbackSQL); !ok {
			return RollbackVerdict{Reason: rollbackNoContract + ": no action contract " +
				"classifies the rollback SQL; automatic rollback withheld (the operator " +
				"rollback remains)"}
		}
		candidate := finding
		candidate.RecommendedSQL = rollbackSQL
		decision := e.evaluateRollbackPolicy(ctx, candidate, actionID)
		// A rollback restores the prior state and never counted toward a
		// budget: its authorization holds no slot.
		e.releaseBudget(ctx, decision.DecisionID)
		return rollbackVerdictFor(decision)
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
	rows, err := e.pool.Query(ctx, `/* pg_sage executor */ SELECT al.id,
		COALESCE(al.rollback_sql, ''), al.sql_executed,
		al.executed_at
		FROM sage.action_log al
		WHERE al.outcome IN ('monitoring', 'interrupted')
		  AND al.action_type <> 'replace_index'
		  AND (COALESCE(al.rollback_sql, '') <> ''
		       OR al.sql_executed ~* '^\s*(REINDEX|CREATE\s+STATISTICS)\s')
		  AND NOT EXISTS (SELECT 1 FROM sage.verification v
		                   WHERE v.action_log_id = al.id)
		ORDER BY al.id`)
	if err != nil {
		return fmt.Errorf("load orphaned monitors: %w", err)
	}
	type orphan struct {
		id               int64
		rollbackSQL, sql string
		executedAt       time.Time
	}
	var orphans []orphan
	for rows.Next() {
		var item orphan
		if err := rows.Scan(&item.id, &item.rollbackSQL, &item.sql,
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
		e.resumeMonitor(ctx, item.id, item.rollbackSQL, item.sql, item.executedAt)
	}
	return nil
}

// resumeMonitor restarts a monitor after a restart. Its schedule is
// relative to the action's execution, so it resumes where it was; one
// whose cap ended long ago is expired instead of judged.
func (e *Executor) resumeMonitor(
	ctx context.Context, actionID int64, rollbackSQL, sql string, executedAt time.Time,
) {
	if isDropActionSQL(sql) {
		// Rows written before executor-01 hold the raw definition.
		rollbackSQL = restoreSQLForDrop(rollbackSQL)
	}
	// Self-initiated, approved and manual actions alike: the revert is a
	// recorded rollback decision of the standing gate (policy-05).
	cfg := e.actionRollbackConfig(actionID, sql)
	_, capW, _ := monitorWindows(verificationClass(sql), cfg)
	if verificationExpired(executedAt, capW, time.Now()) {
		e.expireMonitorAndAct(ctx, actionID, rollbackSQL, executedAt, capW)
		return
	}
	e.logFn("executor", "resuming rollback monitor for action %d", actionID)
	e.startRollbackMonitor(func() {
		MonitorAndRollback(context.WithoutCancel(ctx), e.pool, actionID, rollbackSQL,
			cfg, e.logFn, e.shutdownCh)
	})
}
