package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/value"
)

// RollbackAction executes the stored rollback SQL for an action log row.
// The row is claimed with a compare-and-set so a concurrent monitor cannot
// repeat or overwrite the rollback; index rollbacks run CONCURRENTLY under
// the lock timeout, and GUC rollbacks are reloaded.
func (e *Executor) RollbackAction(
	ctx context.Context,
	actionID int64,
	reason string,
) error {
	if e.pool == nil {
		return fmt.Errorf("rollback of action %d: no database", actionID)
	}
	if e.isReplaceAction(ctx, actionID) {
		return e.RollbackIndexReplace(ctx, actionID, reason)
	}
	rollbackSQL, err := e.loadRollbackSQL(ctx, actionID)
	if err != nil {
		return err
	}
	if rollbackSQL, err = scopeHintRollback(ctx, e.pool, actionID, rollbackSQL); err != nil {
		return fmt.Errorf("rollback refused: %w", err)
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	runCtx, cancel := e.detachedDDLContext(ctx, "")
	defer cancel()
	// A person's revert is a recorded rollback decision (policy-05): hard
	// stops, replica, provider and trust bind; windows and budgets do not.
	decision := e.authorizeRollback(runCtx, actionID, rollbackSQL, true)
	if decision.Decision != PolicyDecisionExecute {
		return operatorRollbackRefusal(decision)
	}
	if !e.claimRollback(runCtx, actionID) {
		return fmt.Errorf("action already rolled back or rollback in progress")
	}
	return e.runClaimedRollback(runCtx, actionID, rollbackSQL, reason, decision)
}

func (e *Executor) runClaimedRollback(
	ctx context.Context, actionID int64, rollbackSQL, reason string,
	decision ActionPolicyDecision,
) error {
	cfg := e.rollbackMonitorConfig(nil)
	cfg.Acquire = nil // the caller already holds the DDL slot
	cfg.LockTimeoutMs = rollbackLockTimeoutMS(rollbackSQL, cfg.LockTimeoutMs,
		decision.LockCeilingMS)
	note, execErr := executeRollbackSQL(ctx, e.pool, rollbackSQL, cfg)
	if execErr != nil {
		_ = recordActionOutcome(ctx, e.pool, e.logFn, actionID, "rollback_failed",
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
	if err := recordActionOutcome(ctx, e.pool, e.logFn, actionID, "rolled_back",
		reason); err != nil {
		return fmt.Errorf("rollback ran but its outcome was not recorded: %w", err)
	}
	_, _ = value.NewPostgresRepository(e.pool).ZeroCreditOnRevert(ctx, actionID, "rolled_back")
	return nil
}

func (e *Executor) loadRollbackSQL(ctx context.Context, actionID int64) (string, error) {
	var rollbackSQL *string
	var outcome string
	var category, reason string
	err := e.pool.QueryRow(ctx,
		`/* pg_sage executor */ SELECT a.rollback_sql, a.outcome, COALESCE(f.category,''),
       COALESCE(a.rollback_reason, '')
 FROM sage.action_log a LEFT JOIN sage.findings f ON f.id=a.finding_id WHERE a.id = $1`,
		actionID,
	).Scan(&rollbackSQL, &outcome, &category, &reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("action not found")
		}
		return "", fmt.Errorf("loading action %d: %w", actionID, err)
	}
	if category == "sre_maintenance" && statisticsRun(ctx) == nil {
		return "", fmt.Errorf("SRE maintenance rollback requires its typed runner")
	}
	if outcome == "rolled_back" {
		return "", fmt.Errorf("action already rolled back")
	}
	if err := refusedRevertRetry(outcome, reason); err != nil {
		return "", err
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
	tag, err := e.pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.action_log
		SET outcome = 'rolling_back', measured_at = now()
		WHERE id = $1 AND outcome NOT IN ('rolled_back', 'rolling_back')`, actionID)
	return err == nil && tag.RowsAffected() == 1
}
