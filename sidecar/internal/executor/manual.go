package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/value"
)

var (
	ErrFindingNotActionable = errors.New("finding not found or not actionable")
	ErrFindingSQLMismatch   = errors.New("sql does not match finding recommendation")
)

// ExecuteManual runs a specific SQL action outside the normal cycle.
// Used for manual "Take Action" and approved queue items.
// Returns the action_log ID.
//
// The caller's context only bounds the wait for a shared DDL slot. Once a
// slot is held, execution and logging run on a context detached from the
// caller (an HTTP request) with an explicit DDL deadline, so a client
// disconnect cannot cancel CREATE INDEX CONCURRENTLY half way and leave an
// INVALID index with no action_log row.
func (e *Executor) ExecuteManual(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	approvedBy *int,
) (int64, error) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return 0, fmt.Errorf("SQL validation: %w", err)
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	runCtx, cancel := e.detachedDDLContext(ctx)
	defer cancel()
	return e.executeManualDetached(runCtx, findingID, sql, rollbackSQL, approvedBy)
}

func (e *Executor) detachedDDLContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := 5 * time.Minute
	if cfg, _, _ := e.policySnapshot(); cfg != nil {
		timeout = cfg.Safety.DDLTimeout() + time.Minute
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (e *Executor) executeManualDetached(
	ctx context.Context, findingID int, sql, rollbackSQL string, approvedBy *int,
) (int64, error) {
	if err := e.manualMutationBlock(ctx); err != nil {
		return 0, err
	}
	findingDetail, err := e.verifyManualFinding(ctx, findingID, sql)
	if err != nil {
		return 0, err
	}
	beforeState := e.snapshotBeforeState(ctx, nil)
	decisionID := e.recordOperatorDecision(ctx, sql, findingID, approvedBy)
	if categorizeAction(sql) == "create_index" {
		done, actionID, err := e.prepareManualCreateIndex(
			ctx, findingID, sql, rollbackSQL, beforeState, approvedBy, decisionID)
		if err != nil || done {
			return actionID, err
		}
	}
	if err := e.manualMutationBlock(ctx); err != nil {
		return 0, err
	}
	execErr := e.runManualSQL(ctx, findingID, sql, findingDetail, approvedBy)
	actionID := e.logManualActionWithDecision(ctx, findingID, sql, rollbackSQL,
		beforeState, execErr, approvedBy, decisionID)
	if execErr != nil {
		return 0, fmt.Errorf("executing SQL: %w", execErr)
	}
	e.notifyPostDDL(ctx, sql)
	e.finishManualAction(ctx, actionID, rollbackSQL)
	return actionID, nil
}

// prepareManualCreateIndex removes a failed remnant of this exact index and
// short-circuits when a valid index already covers the columns.
func (e *Executor) prepareManualCreateIndex(
	ctx context.Context, findingID int, sql, rollbackSQL string,
	beforeState map[string]any, approvedBy *int, decisionID int64,
) (bool, int64, error) {
	ddlTimeout, lockOpt := e.manualDDLOptions()
	if err := e.dropFailedCreateIndexRemnant(ctx, sql, ddlTimeout, lockOpt); err != nil {
		return true, 0, fmt.Errorf("dropping invalid index remnant: %w", err)
	}
	exists, err := e.createIndexCoverageExists(ctx, sql)
	if err != nil {
		return true, 0, fmt.Errorf("checking existing index coverage: %w", err)
	}
	if !exists {
		return false, 0, nil
	}
	if err := e.manualMutationBlock(ctx); err != nil {
		return true, 0, err
	}
	actionID := e.logManualActionWithDecision(ctx, findingID, sql, rollbackSQL,
		beforeState, nil, approvedBy, decisionID)
	if actionID > 0 {
		updateActionSuccess(ctx, e.pool, actionID)
	}
	return true, actionID, nil
}

func (e *Executor) manualDDLOptions() (time.Duration, DDLOption) {
	return e.cfg.Safety.DDLTimeout(), WithLockTimeout(e.cfg.Safety.LockTimeout())
}

func (e *Executor) runManualSQL(
	ctx context.Context, findingID int, sql string,
	findingDetail json.RawMessage, approvedBy *int,
) error {
	if _, _, isSignal := parseBackendSignal(sql); isSignal {
		return e.executeApprovedBackendSignal(ctx, sql, findingDetail, approvedBy)
	}
	if categorizeAction(sql) == "analyze" {
		return e.executeManualAnalyze(ctx, findingID, sql)
	}
	if err := e.checkGUCValueSafety(ctx, sql); err != nil {
		return err
	}
	ddlTimeout, lockOpt := e.manualDDLOptions()
	return e.execManualSQLWithRetry(ctx, sql, ddlTimeout, lockOpt)
}

// finishManualAction starts the rollback monitor (which re-authorizes the
// rollback against the live operator gates) or records immediate success.
func (e *Executor) finishManualAction(ctx context.Context, actionID int64, rollbackSQL string) {
	if actionID <= 0 {
		return
	}
	if rollbackSQL == "" {
		updateActionSuccess(ctx, e.pool, actionID)
		return
	}
	monitorCfg := e.rollbackMonitorConfig(e.manualRollbackAuthorizer())
	e.startRollbackMonitor(func() {
		MonitorAndRollback(context.WithoutCancel(ctx), e.pool, actionID, rollbackSQL,
			monitorCfg, e.logFn, e.shutdownCh)
	})
}

// RollbackAction executes the stored rollback SQL for an action log row.
// The row is claimed with a compare-and-set so a concurrent monitor cannot
// repeat or overwrite the rollback; index rollbacks run CONCURRENTLY under
// the lock timeout, and GUC rollbacks are reloaded.
func (e *Executor) RollbackAction(
	ctx context.Context,
	actionID int64,
	reason string,
) error {
	if err := e.manualMutationBlock(ctx); err != nil {
		return err
	}
	rollbackSQL, err := e.loadRollbackSQL(ctx, actionID)
	if err != nil {
		return err
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	runCtx, cancel := e.detachedDDLContext(ctx)
	defer cancel()
	if err := e.manualMutationBlock(runCtx); err != nil {
		return err
	}
	if !e.claimRollback(runCtx, actionID) {
		return fmt.Errorf("action already rolled back or rollback in progress")
	}
	return e.runClaimedRollback(runCtx, actionID, rollbackSQL, reason)
}

func (e *Executor) runClaimedRollback(
	ctx context.Context, actionID int64, rollbackSQL, reason string,
) error {
	cfg := e.rollbackMonitorConfig(e.manualRollbackAuthorizer())
	cfg.Acquire = nil // the caller already holds the DDL slot
	note, execErr := executeRollbackSQL(ctx, e.pool, rollbackSQL, cfg)
	if execErr != nil {
		updateActionOutcome(ctx, e.pool, actionID, "rollback_failed",
			"manual rollback failed: "+execErr.Error())
		return fmt.Errorf("executing rollback SQL: %w", execErr)
	}
	e.notifyPostDDL(ctx, rollbackSQL)
	if strings.TrimSpace(reason) == "" {
		reason = "manual rollback"
	}
	if isAlterSystem(rollbackSQL) {
		reason += " (" + note + ")"
	}
	updateActionOutcome(ctx, e.pool, actionID, "rolled_back", reason)
	_, _ = value.NewPostgresRepository(e.pool).ZeroCreditOnRevert(ctx, actionID, "rolled_back")
	return nil
}

func (e *Executor) loadRollbackSQL(ctx context.Context, actionID int64) (string, error) {
	var rollbackSQL *string
	var outcome string
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT rollback_sql, outcome
		   FROM sage.action_log WHERE id = $1`,
		actionID,
	).Scan(&rollbackSQL, &outcome)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("action not found")
		}
		return "", fmt.Errorf("loading action %d: %w", actionID, err)
	}
	if outcome == "rolled_back" {
		return "", fmt.Errorf("action already rolled back")
	}
	if rollbackSQL == nil || strings.TrimSpace(*rollbackSQL) == "" {
		return "", fmt.Errorf("action has no rollback SQL")
	}
	if err := ValidateExecutorSQL(*rollbackSQL); err != nil {
		return "", fmt.Errorf("rollback SQL validation: %w", err)
	}
	return *rollbackSQL, nil
}

func (e *Executor) claimRollback(ctx context.Context, actionID int64) bool {
	tag, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_log
		SET outcome = 'rolling_back', measured_at = now()
		WHERE id = $1 AND outcome NOT IN ('rolled_back', 'rolling_back')`, actionID)
	return err == nil && tag.RowsAffected() == 1
}

func (e *Executor) manualMutationBlock(ctx context.Context) error {
	cfg, _, enabled := e.policySnapshot()
	if !enabled {
		return fmt.Errorf("executor is disabled")
	}
	if cfg == nil {
		return fmt.Errorf("execution policy is unavailable")
	}
	if cfg.Trust.Level == "observation" {
		return fmt.Errorf("observation trust is cases only")
	}
	if cfg.Trust.Level != "advisory" && cfg.Trust.Level != "autonomous" {
		return fmt.Errorf("unknown trust level")
	}
	if e.checkEmergencyStop(ctx) {
		return fmt.Errorf("emergency stop active")
	}
	return nil
}

func (e *Executor) verifyManualFinding(
	ctx context.Context,
	findingID int,
	sql string,
) (json.RawMessage, error) {
	var recommendedSQL *string
	var detail json.RawMessage
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT recommended_sql, detail
		   FROM sage.findings
		  WHERE id = $1
		    AND status = 'open'
		    AND acted_on_at IS NULL
		    AND resolved_at IS NULL`,
		findingID,
	).Scan(&recommendedSQL, &detail)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFindingNotActionable
		}
		return nil, fmt.Errorf("checking finding %d: %w", findingID, err)
	}
	if recommendedSQL == nil ||
		compactSQL(*recommendedSQL) == "" ||
		!strings.EqualFold(compactSQL(*recommendedSQL), compactSQL(sql)) {
		return nil, ErrFindingSQLMismatch
	}
	return detail, nil
}

func compactSQL(sql string) string {
	return strings.Join(strings.Fields(
		strings.TrimSuffix(strings.TrimSpace(sql), ";"),
	), " ")
}

func (e *Executor) executeManualAnalyze(
	ctx context.Context,
	findingID int,
	sql string,
) error {
	finding, err := e.manualAnalyzeFinding(ctx, findingID, sql)
	if err != nil {
		return err
	}
	return e.executeAnalyze(ctx, finding)
}

func (e *Executor) manualAnalyzeFinding(
	ctx context.Context,
	findingID int,
	sql string,
) (analyzer.Finding, error) {
	var objectIdentifier string
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT COALESCE(object_identifier, '')
		   FROM sage.findings
		  WHERE id = $1`,
		findingID,
	).Scan(&objectIdentifier)
	if err != nil {
		return analyzer.Finding{}, fmt.Errorf(
			"loading analyze finding %d: %w", findingID, err)
	}
	return analyzer.Finding{
		ObjectIdentifier: objectIdentifier,
		RecommendedSQL:   sql,
		Detail:           map[string]any{},
	}, nil
}

func (e *Executor) execManualSQLWithRetry(
	ctx context.Context,
	sql string,
	ddlTimeout time.Duration,
	lockOpt DDLOption,
) error {
	isCreateIndex := categorizeAction(sql) == "create_index"
	attempts := 1
	if isCreateIndex {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := e.dropFailedCreateIndexRemnant(
				ctx, sql, ddlTimeout, lockOpt,
			); err != nil {
				return err
			}
			time.Sleep(time.Duration(i) * 500 * time.Millisecond)
		}
		var err error
		if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
			err = ExecConcurrently(ctx, e.pool, sql, ddlTimeout, lockOpt)
		} else {
			err = ExecInTransaction(ctx, e.pool, sql, ddlTimeout, lockOpt)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if !isCreateIndex || !errors.Is(err, ErrLockNotAvailable) {
			return err
		}
	}
	return lastErr
}

// logManualAction records a manually-triggered action.
func (e *Executor) logManualAction(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	beforeState map[string]any,
	execErr error, approvedBy *int,
) int64 {
	return e.logManualActionWithDecision(ctx, findingID, sql, rollbackSQL,
		beforeState, execErr, approvedBy, 0)
}

// logManualActionWithDecision records a manual action linked to its
// operator decision and stamped with the executor's database identity.
func (e *Executor) logManualActionWithDecision(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	beforeState map[string]any,
	execErr error, approvedBy *int, decisionID int64,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)
	outcome := actionOutcome(execErr)
	actionType := categorizeAction(sql)

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, approved_by, approved_at, decision_id, database_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::int,
		  CASE WHEN $7::int IS NOT NULL THEN now() ELSE NULL END,
		  NULLIF($8::bigint, 0), $9::bigint)
		 RETURNING id`,
		actionType, findingID, sql,
		store.NilIfEmpty(rollbackSQL), beforeJSON, outcome,
		approvedBy, decisionID, e.databaseIDValue(),
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log manual action: %v", err)
		return 0
	}

	if outcome != "failed" {
		e.markFindingActioned(ctx, int64(findingID), actionID)
	}
	return actionID
}
