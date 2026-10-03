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
	"github.com/pg-sage/sidecar/internal/recommendation"
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
// slot is held, the action runs through Apply on a context detached from the
// caller (an HTTP request) with an explicit deadline, so a client disconnect
// cannot cancel CREATE INDEX CONCURRENTLY half way and leave an INVALID
// index with no action_log row.
func (e *Executor) ExecuteManual(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	approvedBy *int,
) (int64, error) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return 0, fmt.Errorf("SQL validation: %w", err)
	}
	if err := checkIndexRollback(sql, rollbackSQL); err != nil {
		return 0, err
	}
	release, err := e.acquireDDLSlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	runCtx, cancel := e.detachedDDLContext(ctx)
	defer cancel()
	// Refuse early without a ledger row; record once the finding checks out.
	if preview := e.explainOperatorAction(runCtx, sql); preview.Decision != PolicyDecisionExecute {
		return 0, fmt.Errorf("policy refused operator action: %s",
			humanPolicyReason(preview))
	}
	finding, err := e.verifyManualFinding(runCtx, findingID, sql)
	if err != nil {
		return 0, err
	}
	if err := e.checkManualUnusedEvidence(runCtx, finding); err != nil {
		return 0, err
	}
	run := &manualRun{executor: e, findingID: findingID, sql: sql,
		rollbackSQL: rollbackSQL, detail: finding.detail, approvedBy: approvedBy}
	return e.Apply(runCtx, ActionIntent{
		Authorize: func(ctx context.Context) (ActionPolicyDecision, error) {
			decision, err := e.authorizeOperatorAction(ctx, sql, findingID, approvedBy)
			return standingPolicyDecision(decision), err
		},
		TargetLease: operatorLease(sql, approvedBy),
		SlotHeld:    true, Execute: run.execute, Verify: run.verify,
	})
}

// detachedDDLContext outlives the caller (an HTTP request) but never the
// execution deadline.
func (e *Executor) detachedDDLContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), e.applyTimeout())
}

// manualRun carries one operator-approved action from execution to its
// rollback monitor.
type manualRun struct {
	executor    *Executor
	findingID   int
	sql         string
	rollbackSQL string
	detail      json.RawMessage
	approvedBy  *int
	// covered marks a CREATE INDEX an existing valid index already covers:
	// recorded as done, with nothing to monitor.
	covered bool
	// claim is the recommendation this action applies, when there is one.
	claim *recommendation.Claim
	// config is the captured prior state of a config change (G-P0-1).
	config *configChange
}

// execute claims the recommendation the operator's SQL applies (the
// action is its approval; a lost race or a revised recommendation stops
// here), runs it and settles the claim.
func (r *manualRun) execute(
	ctx context.Context, decision ActionPolicyDecision,
) (int64, error) {
	claim, err := r.executor.claimForOperator(ctx, r.findingID, r.sql, r.approvedBy)
	if err != nil {
		return 0, err
	}
	r.claim = claim
	actionID, err := r.run(ctx, decision)
	r.executor.settleClaim(ctx, claim, actionID, err)
	return actionID, err
}

// run re-checks the hard stops, runs the operator's SQL and records it.
func (r *manualRun) run(
	ctx context.Context, decision ActionPolicyDecision,
) (int64, error) {
	e, decisionID := r.executor, decision.DecisionID
	beforeState := e.snapshotBeforeState(ctx, nil)
	if categorizeAction(r.sql) == "create_index" {
		done, actionID, err := e.prepareManualCreateIndex(
			ctx, r.findingID, r.sql, r.rollbackSQL, beforeState, r.approvedBy, decisionID)
		if err != nil || done {
			r.covered = done
			return actionID, err
		}
	}
	if err := e.manualMutationBlock(ctx); err != nil {
		return 0, err
	}
	if err := r.prepareConfig(ctx, beforeState); err != nil {
		return 0, err
	}
	e.predictAction(ctx, r.sql, detailMap(r.detail), beforeState)
	execErr := e.runManualSQL(ctx, r.findingID, r.sql, r.detail, r.approvedBy, decision)
	actionID := e.logManualActionWithDecision(ctx, r.findingID, r.sql, r.rollbackSQL,
		beforeState, execErr, r.approvedBy, decisionID, r.claim)
	if execErr != nil {
		return 0, fmt.Errorf("executing SQL: %w", execErr)
	}
	e.recordPrediction(ctx, actionID, beforeState)
	return actionID, nil
}

// verify starts the verification monitor or verifies the action at once.
func (r *manualRun) verify(ctx context.Context, actionID int64) error {
	if r.covered {
		return nil
	}
	r.executor.notifyPostDDL(ctx, r.sql)
	if r.executor.settleConfigChange(ctx, actionID, r.config) {
		r.executor.finishManualAction(ctx, actionID, r.rollbackSQL)
	}
	return nil
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
		beforeState, nil, approvedBy, decisionID, nil)
	if actionID > 0 {
		settleOutcome(ctx, e.pool, coveredIndexOutcome(actionID), e.logFn)
	}
	return true, actionID, nil
}

func (e *Executor) manualDDLOptions() (time.Duration, DDLOption) {
	return e.ddlTimeout(), WithLockTimeout(e.cfg.Safety.LockTimeout())
}

// runManualSQL runs an operator-approved statement under the decision's
// lock timeout (the policy lock ceiling caps in-transaction statements).
func (e *Executor) runManualSQL(
	ctx context.Context, findingID int, sql string,
	findingDetail json.RawMessage, approvedBy *int, decision ActionPolicyDecision,
) error {
	if _, _, isSignal := parseBackendSignal(sql); isSignal {
		return e.executeApprovedBackendSignal(ctx, sql, findingDetail, approvedBy)
	}
	if categorizeAction(sql) == "analyze" {
		return e.executeManualAnalyze(ctx, findingID, sql, e.lockTimeoutMS(sql, decision))
	}
	if err := e.checkGUCValueSafety(ctx, sql); err != nil {
		return err
	}
	return e.execManualSQLWithRetry(ctx, sql, e.ddlTimeout(), e.lockOption(sql, decision))
}

// finishManualAction starts the verification monitor (which re-authorizes
// rollback against the live operator gates) or verifies the action at once.
func (e *Executor) finishManualAction(ctx context.Context, actionID int64, rollbackSQL string) {
	if actionID <= 0 {
		return
	}
	if rollbackSQL == "" {
		e.verifyImmediate(ctx, actionID)
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

// manualFinding is the open finding an operator action applies.
type manualFinding struct {
	detail   json.RawMessage
	category string
	ident    string
}

func (e *Executor) verifyManualFinding(
	ctx context.Context,
	findingID int,
	sql string,
) (manualFinding, error) {
	var recommendedSQL *string
	var f manualFinding
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT recommended_sql, detail, category, object_identifier
		   FROM sage.findings
		  WHERE id = $1
		    AND status = 'open'
		    AND acted_on_at IS NULL
		    AND resolved_at IS NULL`,
		findingID,
	).Scan(&recommendedSQL, &f.detail, &f.category, &f.ident)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return manualFinding{}, ErrFindingNotActionable
		}
		return manualFinding{}, fmt.Errorf("checking finding %d: %w", findingID, err)
	}
	if recommendedSQL == nil ||
		compactSQL(*recommendedSQL) == "" ||
		!strings.EqualFold(compactSQL(*recommendedSQL), compactSQL(sql)) {
		return manualFinding{}, ErrFindingSQLMismatch
	}
	return f, nil
}

func compactSQL(sql string) string {
	return strings.Join(strings.Fields(
		strings.TrimSuffix(strings.TrimSpace(sql), ";"),
	), " ")
}

func (e *Executor) executeManualAnalyze(
	ctx context.Context, findingID int, sql string, lockTimeoutMs int,
) error {
	finding, err := e.manualAnalyzeFinding(ctx, findingID, sql)
	if err != nil {
		return err
	}
	return e.executeAnalyze(ctx, finding, lockTimeoutMs)
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
		beforeState, execErr, approvedBy, 0, nil)
}

// logManualActionWithDecision records a manual action linked to its
// operator decision and stamped with the executor's database identity,
// together with the claimed recommendation's outcome when claim is set.
func (e *Executor) logManualActionWithDecision(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	beforeState map[string]any,
	execErr error, approvedBy *int, decisionID int64,
	claim *recommendation.Claim,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)
	outcome := actionOutcome(execErr)
	actionID, err := e.recordAction(ctx, claim, execErr,
		func(q actionLogWriter) (int64, error) {
			var id int64
			err := q.QueryRow(ctx,
				`/* pg_sage */ INSERT INTO sage.action_log
				 (action_type, finding_id, sql_executed, rollback_sql,
				  before_state, outcome, approved_by, approved_at, decision_id, database_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7::int,
				  CASE WHEN $7::int IS NOT NULL THEN now() ELSE NULL END,
				  NULLIF($8::bigint, 0), $9::bigint)
				 RETURNING id`,
				categorizeAction(sql), findingID, sql,
				store.NilIfEmpty(rollbackSQL), beforeJSON, outcome,
				approvedBy, decisionID, e.databaseIDValue(),
			).Scan(&id)
			return id, err
		})
	if err != nil {
		e.logFn("executor", "failed to log manual action: %v", err)
		return 0
	}
	if outcome != "failed" {
		e.markFindingActioned(ctx, int64(findingID), actionID)
	}
	return actionID
}
