package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// RunCycle is called after each analyzer cycle. It acts on the durable
// recommendations (C07): the analyzer's in-memory findings of the latest
// cycle are never the source, so a recommendation proposed earlier stays
// eligible when policy, trust or the window later permits it.
func (e *Executor) RunCycle(ctx context.Context, isReplica bool) {
	ctx, done := selfbudget.Process().TrackFor(ctx, "executor")
	defer done()
	e.resumeOnce.Do(func() {
		if err := e.resumeOrphanedMonitors(ctx); err != nil {
			e.logFn("executor", "resume rollback monitors: %v", err)
		}
	})
	if e.indexVerification != nil {
		e.resumeIndexVerification(ctx)
	}
	if !isReplica {
		if err := e.ResumeIndexReplaces(ctx); err != nil {
			e.logFn("executor", "resume index replacements: %v", err)
		}
		// Every cycle, not once: a claim younger than the grace at startup,
		// or a terminal write lost while running, settles later.
		e.settleStuckRollbacks(ctx)
	}
	e.reconcileRecommendations(ctx)
	// Queue housekeeping runs in every mode: a proposal whose reason is
	// gone must not wait for an operator (dogfood round 2 item 5).
	if n, err := e.supersedeStaleApprovals(ctx); err != nil {
		e.logFn("executor", "supersede stale approvals: %v", err)
	} else if n > 0 {
		e.logFn("executor", "superseded %d queued proposal(s) whose reason is gone", n)
	}
	// Scheduled approvals are people's decisions: they run in every mode,
	// and the gate re-checks each one (executor-disabled included).
	if !isReplica {
		e.RunScheduledApprovals(ctx)
	}
	// Manual mode and executor-disabled are hard background-action stops.
	if e.effectiveExecMode() == "manual" || !e.ExecutorEnabled() {
		return
	}
	e.pruneRecentActions()
	for _, c := range e.actionableRecommendations(ctx) {
		e.processCandidate(ctx, c, isReplica)
	}
}

// processFinding authorizes one recommendation's finding, then queues it
// for approval or runs it through Apply, which re-authorizes after its
// waits.
func (e *Executor) processFinding(
	ctx context.Context, f analyzer.Finding, isReplica bool,
	cand *recommendation.Candidate,
) {
	if f.RecommendedSQL == "" || e.unusedDropRefused(ctx, f) {
		return
	}
	if err := prepareFindingRollback(&f); err != nil {
		e.logFn("executor", "refused %q: %v", f.Title, err)
		return
	}
	// The read-only skips run before the gate, which records a decision
	// (dogfood lifeos: a skipped candidate still wrote one every cycle).
	if e.isCascadeCooldown(f.ObjectIdentifier) {
		return
	}
	findingID := e.lookupFindingID(ctx, f)
	if e.withheldBeforeGate(ctx, f, findingID) {
		return
	}
	f, ok := e.gateFinding(ctx, f, findingID, cand)
	if !ok {
		return
	}
	decision := e.evaluateFindingPolicy(ctx, f, isReplica)
	e.recordShadow(ctx, findingRequest(f, isReplica), f, findingID, cand, decision)
	switch decision.Decision {
	case PolicyDecisionExecute:
	case PolicyDecisionQueueApproval:
		e.queueFinding(ctx, f, findingID, decision, cand)
		return
	default:
		// Blocked, observe-only and parked verdicts are recorded; the next
		// cycle evaluates the candidate again, Apply must not (lifeos: a
		// park fell through to Apply and was evaluated twice per cycle).
		return
	}
	if e.recentlyRolledBack(ctx, findingID) {
		e.releaseBudget(ctx, decision.DecisionID)
		e.logFn("executor", "skipping %q — rolled back recently (cooldown)", f.Title)
		return
	}
	intent := e.findingIntent(f, findingID, isReplica, cand)
	intent.FirstDecision = &decision
	_, err := e.Apply(ctx, intent)
	if errors.Is(err, ErrDDLSlotUnavailable) {
		e.logFn("executor", "DDL concurrency limit reached, skipping %s", f.RecommendedSQL)
	}
}

// gateFinding is f as the gate, the shadow and the queue must all see it:
// with its open finding's current evidence (the revision's is immutable),
// and marked approval_required when its VACUUM targets a partitioned
// parent (vacuum-01) or its step cannot be verified (lab-01c). ok is
// false for a stale VACUUM finding whose table no longer exists.
func (e *Executor) gateFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	cand *recommendation.Candidate,
) (analyzer.Finding, bool) {
	f = e.currentGateEvidence(ctx, f, findingID, cand)
	f, stale := e.guardVacuumTarget(ctx, f)
	return e.guardUnverifiable(ctx, f), !stale
}

// withheldBeforeGate reports the read-only skips after the finding lookup:
// an unknown finding, retries exhausted, anti-oscillation (an object that
// keeps reverting externally stops being re-applied), a parked withhold,
// or a CREATE INDEX an existing index already covers.
func (e *Executor) withheldBeforeGate(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	return findingID <= 0 || e.exceedsMaxRetries(ctx, findingID) ||
		e.exceedsOscillationLimit(ctx, f, findingID) ||
		e.parkedWithhold(ctx, f, findingID) || e.skipCoveredCreate(ctx, f, findingID)
}

// findingIntent routes a background finding through Apply. A denied change
// lease is recorded as a failed action, as before.
func (e *Executor) findingIntent(
	f analyzer.Finding, findingID int64, isReplica bool,
	cand *recommendation.Candidate,
) ActionIntent {
	lease := f
	return ActionIntent{
		Request: findingRequest(f, isReplica), Lease: &lease,
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			return e.runAuthorizedFinding(ctx, f, findingID, decision, cand), nil
		},
		Refused: func(ctx context.Context, decisionID int64, err error) {
			before := e.snapshotBeforeState(ctx, targetQueryIDs(f))
			e.logRefusedAction(ctx, f, findingID, before, decisionID, err)
			e.logFn("executor", "DDL lease denied for %q: %v", f.Title, err)
		},
	}
}

// queueFinding proposes a finding for operator approval unless an equal
// proposal is pending or was recently rejected.
func (e *Executor) queueFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	decision ActionPolicyDecision, cand *recommendation.Candidate,
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
	queueID, err := e.proposeForApproval(ctx, int(findingID), proposal, cand)
	if err != nil {
		e.logFn("executor", "failed to queue %q for approval: %v", f.Title, err)
		return
	}
	e.logFn("executor", "queued %q for approval", f.Title)
	e.requestApproval(ctx, f.Title, f.RecommendedSQL, decision.RiskTier,
		decision.DecisionID, queueID)
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
// change lease and DDL slot, records it, and starts its verification. The
// recommendation is claimed right before its SQL runs and its outcome is
// recorded with the action. It returns the action_log id (0 when load
// admission withheld the build or another worker holds the claim).
func (e *Executor) runAuthorizedFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	decision ActionPolicyDecision, cand *recommendation.Candidate,
) int64 {
	decisionID := decision.DecisionID
	beforeState := e.snapshotBeforeState(ctx, targetQueryIDs(f))
	if refusal := e.findingRefusal(f); refusal != nil {
		return e.logRefusedAction(ctx, f, findingID, beforeState, decisionID, refusal)
	}
	if !e.retireStaleApprovals(ctx, f, findingID, decisionID, cand) {
		return 0
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
			markContentBoundWithhold(f, beforeState, err)
			return e.logRefusedAction(ctx, f, findingID, beforeState, decisionID, err)
		}
	}
	claim, err := e.claimCandidate(ctx, cand, decisionID)
	if err != nil {
		e.logFn("executor", "skipping %q: %v", f.Title, err)
		return 0
	}
	config, execErr := e.prepareAndRunFinding(ctx, &f, beforeState, decision)
	if verifiedCreate && execErr == nil {
		e.recordCreatedIndexIdentity(ctx, verified.IndexName, beforeState)
	}
	actionID := e.logClaimedAction(ctx, f, findingID, beforeState, decisionID, execErr, claim)
	e.settleClaim(ctx, claim, actionID, execErr)
	if execErr != nil {
		e.recordFindingFailure(ctx, f, actionID, execErr)
		return actionID
	}
	e.finishFinding(ctx, f, actionID)
	switch {
	case verifiedCreate:
		e.watchVerifiedCreate(ctx, verified, actionID)
	case e.settleConfigChange(ctx, actionID, config):
		e.monitorFinding(ctx, f, actionID)
	}
	return actionID
}

// prepareAndRunFinding runs the finding's SQL. For a config change it
// first captures the prior state and replaces the proposed rollback with
// the one that restores it (G-P0-1); without a faithful rollback the
// change does not run, and the attempt fails like any other. A DROP
// INDEX's re-create rollback is re-rendered under the pinned search_path.
func (e *Executor) prepareAndRunFinding(
	ctx context.Context, f *analyzer.Finding, beforeState map[string]any,
	decision ActionPolicyDecision,
) (*configChange, error) {
	config, err := e.prepareConfigChange(ctx, f.RecommendedSQL)
	if err != nil {
		e.logFn("executor", "refused config change %q: %v", f.Title, err)
		return nil, err
	}
	if f.RollbackSQL, err = e.pinDropRollback(ctx, f.RecommendedSQL, f.RollbackSQL); err != nil {
		return nil, err
	}
	if config != nil {
		f.RollbackSQL = config.rollbackSQL
		config.record(beforeState)
	}
	e.predictAction(ctx, f.RecommendedSQL, f.Detail, beforeState)
	return config, e.runFindingSQLRecorded(ctx, *f, decision, beforeState)
}

// findingRefusal is a reason never to run the finding unattended: backend
// signals need an operator approval and the evidence-matched signal path,
// and managed providers change configuration through their own API.
func (e *Executor) findingRefusal(f analyzer.Finding) error {
	if IsIndexReplaceSQL(f.RecommendedSQL) {
		return ErrReplaceApprovalRequired
	}
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
	return e.runFindingSQLRecorded(ctx, f, decision, nil)
}

// runFindingSQLRecorded is runFindingSQL that records a failed
// CONCURRENTLY statement's remnant cleanup in beforeState.
func (e *Executor) runFindingSQLRecorded(
	ctx context.Context, f analyzer.Finding, decision ActionPolicyDecision,
	beforeState map[string]any,
) error {
	sql := f.RecommendedSQL
	if err := e.checkGUCValueSafety(ctx, sql); err != nil {
		return err
	}
	lockOpt := e.lockOption(sql, decision)
	switch {
	case roleBudgetActionType(sql) != "":
		return e.execRoleBudget(ctx, sql, beforeState, e.ddlTimeoutFor(sql),
			e.lockTimeoutMS(sql, decision))
	case categorizeAction(sql) == "create_statistics":
		return ExecStatistics(ctx, e.actionDB(), sql, e.ddlTimeoutFor(sql), lockOpt)
	case categorizeAction(sql) == "analyze":
		return e.executeAnalyze(ctx, f, e.lockTimeoutMS(sql, decision))
	case statementRunsTopLevel(sql):
		return e.execConcurrentDDL(ctx, sql, beforeState, lockOpt)
	default:
		return ExecInTransaction(ctx, e.actionDB(), sql, e.ddlTimeoutFor(sql), lockOpt)
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

// finishFinding notifies and writes the audit justification. A config
// change is reloaded and read back by settleConfigChange.
func (e *Executor) finishFinding(ctx context.Context, f analyzer.Finding, actionID int64) {
	e.notifyPostDDL(ctx, f.RecommendedSQL)
	e.logFn("executor", "executed %q (action %d)", f.Title, actionID)
	e.dispatchEvent(ctx, notify.ActionExecutedEvent(
		f.Title, f.RecommendedSQL, e.databaseName))
	e.noteRecentAction(f.ObjectIdentifier)
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

// monitorFinding starts the verification monitor for a reversible action
// (and for REINDEX / CREATE STATISTICS, watched without one), or verifies
// VACUUM/ANALYZE by its metric at once. The monitor is detached from the
// execution deadline; Shutdown aborts it.
func (e *Executor) monitorFinding(ctx context.Context, f analyzer.Finding, actionID int64) {
	if actionID <= 0 {
		return
	}
	if f.RollbackSQL == "" && !monitoredWithoutRollback(f.RecommendedSQL) {
		e.verifyImmediate(ctx, actionID)
		return
	}
	monitorCfg := e.findingMonitorConfig(f, actionID)
	e.startRollbackMonitor(func() {
		MonitorAndRollback(
			context.WithoutCancel(ctx), e.pool, actionID, f.RollbackSQL,
			monitorCfg, e.logFn, e.shutdownCh,
		)
	})
}
