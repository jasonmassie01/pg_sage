package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/notify"
)

// RunCycle is called after each analyzer cycle to evaluate and execute
// any actionable findings.
func (e *Executor) RunCycle(ctx context.Context, isReplica bool) {
	e.resumeOnce.Do(func() {
		if err := e.resumeOrphanedMonitors(ctx); err != nil {
			e.logFn("executor", "resume rollback monitors: %v", err)
		}
	})
	if e.indexVerification != nil {
		if err := e.indexVerification.ResumeDue(ctx); err != nil {
			e.logFn("executor", "resume index verification: %v", err)
		}
	}
	// Manual mode and executor-disabled are hard background-action stops.
	if e.effectiveExecMode() == "manual" || !e.ExecutorEnabled() {
		return
	}
	e.pruneRecentActions()
	for _, f := range e.analyzer.Findings() {
		e.processFinding(ctx, f, isReplica)
	}
}

// processFinding authorizes one finding, then queues it for approval or
// runs it through Apply, which re-authorizes after its waits.
func (e *Executor) processFinding(ctx context.Context, f analyzer.Finding, isReplica bool) {
	if f.RecommendedSQL == "" {
		return
	}
	decision := e.evaluateFindingPolicy(ctx, f, isReplica)
	if decision.Decision == PolicyDecisionBlocked ||
		decision.Decision == PolicyDecisionObserveOnly ||
		e.isCascadeCooldown(f.ObjectIdentifier) {
		return
	}
	findingID := e.lookupFindingID(ctx, f)
	// Anti-oscillation: an object that keeps reverting externally stops
	// being re-applied.
	if findingID <= 0 || e.exceedsMaxRetries(ctx, findingID) ||
		e.exceedsOscillationLimit(ctx, f, findingID) {
		return
	}
	if decision.Decision == PolicyDecisionQueueApproval {
		e.queueFinding(ctx, f, findingID, decision)
		return
	}
	if CheckHysteresis(ctx, e.pool, findingID, e.cfg.Trust.RollbackCooldownDays) {
		e.logFn("executor", "skipping %q — rolled back recently (cooldown)", f.Title)
		return
	}
	_, err := e.Apply(ctx, e.findingIntent(f, findingID, isReplica))
	if errors.Is(err, ErrDDLSlotUnavailable) {
		e.logFn("executor", "DDL concurrency limit reached, skipping %s", f.RecommendedSQL)
	}
}

// findingIntent routes a background finding through Apply. A denied change
// lease is recorded as a failed action, as before.
func (e *Executor) findingIntent(
	f analyzer.Finding, findingID int64, isReplica bool,
) ActionIntent {
	lease := f
	return ActionIntent{
		Request: findingRequest(f, isReplica), Lease: &lease,
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			return e.runAuthorizedFinding(ctx, f, findingID, decision), nil
		},
		Refused: func(ctx context.Context, decisionID int64, err error) {
			before := e.snapshotBeforeState(ctx, targetQueryIDs(f))
			e.logActionWithDecision(ctx, f, findingID, before, decisionID, err)
			e.logFn("executor", "DDL lease denied for %q: %v", f.Title, err)
		},
	}
}

// queueFinding proposes a finding for operator approval unless an equal
// proposal is pending or was recently rejected.
func (e *Executor) queueFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	decision ActionPolicyDecision,
) {
	if e.actionStore == nil {
		e.logFn("executor", "cannot queue %q: action store unavailable", f.Title)
		return
	}
	if e.pendingApproval(ctx, f, findingID) || e.recentlyRejected(ctx, f, findingID) {
		return
	}
	proposal := f
	proposal.ActionRisk = decision.RiskTier
	if _, err := e.proposeForApproval(ctx, int(findingID), proposal); err != nil {
		e.logFn("executor", "failed to queue %q for approval: %v", f.Title, err)
		return
	}
	e.logFn("executor", "queued %q for approval", f.Title)
	e.dispatchEvent(ctx, notify.ApprovalNeededEvent(
		f.Title, f.RecommendedSQL, e.databaseName, decision.RiskTier))
}

// pendingApproval reports an unresolved proposal for the finding or its SQL.
// A failed check counts as pending: never queue a duplicate blind.
func (e *Executor) pendingApproval(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	if checker, ok := e.actionStore.(PendingActionChecker); ok {
		pending, err := checker.HasPendingForFinding(ctx, int(findingID))
		if err != nil {
			e.logFn("executor", "failed to check pending approval for %q: %v", f.Title, err)
			return true
		}
		if pending {
			return true
		}
	}
	checker, ok := e.actionStore.(PendingActionSQLChecker)
	if !ok {
		return false
	}
	pending, err := checker.HasPendingForSQL(ctx, f.RecommendedSQL)
	if err != nil {
		e.logFn("executor", "failed to check duplicate approval SQL for %q: %v",
			f.Title, err)
		return true
	}
	return pending
}

// recentlyRejected reports an operator rejection of the finding or its SQL
// within the cascade cooldown. A failed check counts as rejected.
func (e *Executor) recentlyRejected(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	checker, ok := e.actionStore.(RejectedActionChecker)
	if !ok {
		return false
	}
	cooldown := e.cascadeCooldown()
	rejected, err := checker.HasRecentlyRejectedForFinding(ctx, int(findingID), cooldown)
	if err != nil {
		e.logFn("executor", "failed to check rejected approval for %q: %v", f.Title, err)
		return true
	}
	if rejected {
		return true
	}
	rejected, err = checker.HasRecentlyRejectedForSQL(ctx, f.RecommendedSQL, cooldown)
	if err != nil {
		e.logFn("executor", "failed to check rejected approval SQL for %q: %v",
			f.Title, err)
		return true
	}
	return rejected
}

// runAuthorizedFinding runs one authorized finding while Apply holds its
// change lease and DDL slot, records it, and starts its verification. It
// returns the action_log id (0 when load admission withheld the build).
func (e *Executor) runAuthorizedFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	decision ActionPolicyDecision,
) int64 {
	decisionID := decision.DecisionID
	beforeState := e.snapshotBeforeState(ctx, targetQueryIDs(f))
	if refusal := e.findingRefusal(f); refusal != nil {
		return e.logActionWithDecision(ctx, f, findingID, beforeState, decisionID, refusal)
	}
	var verified verifiedIndexAction
	verifiedCreate := categorizeAction(f.RecommendedSQL) == "create_index"
	if verifiedCreate {
		var err error
		verified, err = e.admitVerifiedCreate(ctx, &f, beforeState, findingID, decisionID)
		if isAdmissionWithheld(err) {
			return 0 // recorded once per finding and reason; stays retryable
		}
		if err != nil {
			e.logFn("executor", "withheld unverifiable CREATE INDEX %q: %v", f.Title, err)
			return e.logActionWithDecision(ctx, f, findingID, beforeState, decisionID, err)
		}
	}
	execErr := e.runFindingSQL(ctx, f, decision)
	if verifiedCreate && execErr == nil {
		e.recordCreatedIndexIdentity(ctx, verified.IndexName, beforeState)
	}
	actionID := e.logActionWithDecision(ctx, f, findingID, beforeState, decisionID, execErr)
	if execErr != nil {
		e.recordFindingFailure(ctx, f, actionID, execErr)
		return actionID
	}
	e.finishFinding(ctx, f, actionID)
	if verifiedCreate {
		e.watchVerifiedCreate(ctx, verified, actionID)
	} else {
		e.monitorFinding(ctx, f, actionID)
	}
	return actionID
}

// findingRefusal is a reason never to run the finding unattended: backend
// signals need an operator approval and the evidence-matched signal path,
// and managed providers change configuration through their own API.
func (e *Executor) findingRefusal(f analyzer.Finding) error {
	if _, _, isSignal := parseBackendSignal(f.RecommendedSQL); isSignal {
		e.logFn("executor", "refused autonomous backend signal %q", f.Title)
		return ErrBackendApprovalRequired
	}
	if isAlterSystem(f.RecommendedSQL) && isManagedProvider(e.cfg.CloudEnvironment) {
		param := configParamFromSQL(f.RecommendedSQL)
		guidance := managedConfigGuidance(e.cfg.CloudEnvironment, param)
		e.logFn("executor", "%s", guidance)
		return fmt.Errorf("%s", guidance)
	}
	return nil
}

// runFindingSQL executes the finding's statement with its statement
// timeout and the decision's lock timeout.
func (e *Executor) runFindingSQL(
	ctx context.Context, f analyzer.Finding, decision ActionPolicyDecision,
) error {
	if err := e.checkGUCValueSafety(ctx, f.RecommendedSQL); err != nil {
		return err
	}
	lockOpt := e.lockOption(f.RecommendedSQL, decision)
	switch {
	case categorizeAction(f.RecommendedSQL) == "analyze":
		return e.executeAnalyze(ctx, f, e.lockTimeoutMS(f.RecommendedSQL, decision))
	case NeedsConcurrently(f.RecommendedSQL) || NeedsTopLevel(f.RecommendedSQL):
		return ExecConcurrently(ctx, e.pool, f.RecommendedSQL, e.ddlTimeout(), lockOpt)
	default:
		return ExecInTransaction(ctx, e.pool, f.RecommendedSQL, e.ddlTimeout(), lockOpt)
	}
}

func (e *Executor) recordFindingFailure(
	ctx context.Context, f analyzer.Finding, actionID int64, execErr error,
) {
	_, _ = finalizeActionVerification(ctx, e.pool, actionID, "failed", execErr.Error())
	if errors.Is(execErr, ErrLockNotAvailable) {
		e.logFn("executor", "lock timeout for %q on %s — circuit-breaking table",
			f.Title, f.ObjectIdentifier)
		e.noteRecentAction(f.ObjectIdentifier)
	}
	e.logFn("executor", "execution failed for %q: %v", f.Title, execErr)
	e.dispatchEvent(ctx, notify.ActionFailedEvent(
		f.Title, f.RecommendedSQL, e.databaseName, execErr.Error()))
}

// finishFinding notifies, applies a configuration change (ALTER SYSTEM only
// writes postgresql.auto.conf) and writes the audit justification.
func (e *Executor) finishFinding(ctx context.Context, f analyzer.Finding, actionID int64) {
	e.notifyPostDDL(ctx, f.RecommendedSQL)
	e.logFn("executor", "executed %q (action %d)", f.Title, actionID)
	e.dispatchEvent(ctx, notify.ActionExecutedEvent(
		f.Title, f.RecommendedSQL, e.databaseName))
	e.noteRecentAction(f.ObjectIdentifier)
	if isAlterSystem(f.RecommendedSQL) {
		outcome := applyConfigChange(
			ctx, e.pool, f.RecommendedSQL, e.cfg.CloudEnvironment, e.logFn)
		e.logFn("executor", "config: %s", outcome.Note)
		updateActionOutcome(ctx, e.pool, actionID,
			outcomeStatus(outcome.InEffect), outcome.Note)
	}
	// Async so LLM latency never blocks the cycle; WithoutCancel so it
	// survives the execution deadline.
	if e.justifier != nil {
		go e.justifyAndStore(context.WithoutCancel(ctx), actionID, f)
	}
}

// watchVerifiedCreate hands a created index to the durable verification
// engine, which retains or reverts it.
func (e *Executor) watchVerifiedCreate(
	ctx context.Context, verified verifiedIndexAction, actionID int64,
) {
	verified.WatchID = fmt.Sprintf("index-action-%d", actionID)
	if err := e.indexVerification.WatchApplied(
		context.WithoutCancel(ctx), verified, actionID,
	); err != nil {
		e.logFn("executor", "index verification failed for action %d: %v", actionID, err)
	}
}

// monitorFinding starts the rollback window for a reversible action, or
// marks an irreversible one (VACUUM, ANALYZE) successful at once. The
// monitor is detached from the execution deadline; Shutdown aborts it.
func (e *Executor) monitorFinding(ctx context.Context, f analyzer.Finding, actionID int64) {
	if actionID <= 0 {
		return
	}
	if f.RollbackSQL == "" {
		updateActionSuccess(ctx, e.pool, actionID)
		return
	}
	monitorCfg := e.rollbackMonitorConfig(e.standingRollbackAuthorizer(f))
	e.startRollbackMonitor(func() {
		MonitorAndRollback(
			context.WithoutCancel(ctx), e.pool, actionID, f.RollbackSQL,
			monitorCfg, e.logFn, e.shutdownCh,
		)
	})
}
