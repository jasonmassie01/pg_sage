package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/store"
)

// ExecuteManual runs a specific SQL action outside the normal cycle.
// Used for manual "Take Action" and approved queue items.
// Returns the action_log ID.
func (e *Executor) ExecuteManual(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	approvedBy *int,
) (int64, error) {
	if CheckEmergencyStop(ctx, e.pool) {
		return 0, fmt.Errorf("emergency stop active")
	}

	if err := ValidateExecutorSQL(sql); err != nil {
		return 0, fmt.Errorf("SQL validation: %w", err)
	}

	beforeState := e.snapshotBeforeState(ctx)

	ddlTimeout := e.cfg.Safety.DDLTimeout()
	lockOpt := WithLockTimeout(e.cfg.Safety.LockTimeout())
	var execErr error
	if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
		execErr = ExecConcurrently(
			ctx, e.pool, sql, ddlTimeout, lockOpt,
		)
	} else {
		execErr = ExecInTransaction(
			ctx, e.pool, sql, ddlTimeout, lockOpt,
		)
	}

	actionID := e.logManualAction(
		ctx, findingID, sql, rollbackSQL,
		beforeState, execErr, approvedBy,
	)
	if execErr != nil {
		return 0, fmt.Errorf("executing SQL: %w", execErr)
	}

	if rollbackSQL != "" && actionID > 0 {
		go MonitorAndRollback(
			ctx, e.pool, actionID, rollbackSQL,
			e.cfg.Trust.RollbackThresholdPct,
			e.cfg.Trust.RollbackWindowMinutes,
			e.logFn,
		)
	} else if actionID > 0 {
		updateActionSuccess(ctx, e.pool, actionID)
	}

	return actionID, nil
}

// logManualAction records a manually-triggered action.
func (e *Executor) logManualAction(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	beforeState map[string]any,
	execErr error, approvedBy *int,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)
	outcome := actionOutcome(execErr)
	actionType := categorizeAction(sql)

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, approved_by, approved_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7,
		  CASE WHEN $7 IS NOT NULL THEN now() ELSE NULL END)
		 RETURNING id`,
		actionType, findingID, sql,
		store.NilIfEmpty(rollbackSQL), beforeJSON, outcome,
		approvedBy,
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log manual action: %v", err)
		return 0
	}

	if outcome != "failed" {
		_, _ = e.pool.Exec(ctx,
			`UPDATE sage.findings
			 SET acted_on_at = now(), action_log_id = $1
			 WHERE id = $2`,
			actionID, findingID,
		)
	}
	return actionID
}
