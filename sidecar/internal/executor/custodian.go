package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/runway"
)

var ErrCustodianProposalWithheld = errors.New("custodian proposal withheld by policy")

// StandingGateUnavailable is the blocked reason of a custodian evaluation
// without a standing policy gate.
const StandingGateUnavailable = "standing policy gate is unavailable"

type CustodianProposal struct {
	Feature string
	SQL     string
	// ResponseKind is the custodian's response kind; it types plan-only
	// proposals (contractForCustodianResponse).
	ResponseKind  string
	TargetObjects []string
	IsReplica     bool
	Deadline      *policy.DeadlineContext
	Evidence      map[string]any
	// ObservedAt is when the custodian sampled the evidence; the earned-
	// autonomy ledger downgrades a proposal whose evidence is stale.
	ObservedAt time.Time
}

// SubmitVerifiedIndexProposal runs a custodian CREATE INDEX through Apply
// with durable verification: admission (rollback and workload evidence,
// verifier headroom) is checked after the first authorization.
func (e *Executor) SubmitVerifiedIndexProposal(
	ctx context.Context, proposal CustodianProposal,
	rollbackSQL string, queryIDs []int64,
) error {
	finding := custodianFinding(proposal)
	finding.RollbackSQL = rollbackSQL
	finding.Detail = map[string]any{"queryids": append([]int64(nil), queryIDs...)}
	_, err := e.Apply(ctx, ActionIntent{
		Request: custodianRequest(proposal), Lease: &finding, WaitForSlot: true,
		Admit: func(ctx context.Context, decisionID int64) error {
			return e.admitVerifiedIndex(ctx, finding, decisionID)
		},
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			return e.runAuthorizedFinding(ctx, finding, 0, decision, nil), nil
		},
	})
	e.shadowWithheldCustodian(ctx, proposal, err)
	return custodianWithheld(err, "after admission")
}

// admitVerifiedIndex requires rollback and workload evidence, then runs
// load admission, recorded in the authorizing decision (D6).
func (e *Executor) admitVerifiedIndex(
	ctx context.Context, finding analyzer.Finding, decisionID int64,
) error {
	if _, err := verifiedActionForFinding(finding); err != nil {
		return fmt.Errorf("route custodian index through verification: %w", err)
	}
	if e.indexVerification == nil {
		return ErrVerificationUnavailable
	}
	key := admissionFindingKey(0, finding.RecommendedSQL)
	return e.admitIndexBuild(ctx, key, decisionID)
}

// custodianWithheld reports a policy refusal as ErrCustodianProposalWithheld,
// naming a refusal by the re-authorization with stage. A withhold caused by
// an error (the maintenance precheck's missing privilege) keeps that cause
// in the chain, so the caller can tell a grant to fix from other refusals.
func custodianWithheld(err error, stage string) error {
	var withheld *WithheldError
	if !errors.As(err, &withheld) {
		return err
	}
	if withheld.Reauthorized {
		return fmt.Errorf("%w %s: %s", ErrCustodianProposalWithheld, stage,
			withheld.Decision.BlockedReason)
	}
	var caused *causedWithheldError
	if errors.As(err, &caused) {
		return fmt.Errorf("%w: %s: %w", ErrCustodianProposalWithheld,
			withheld.Decision.BlockedReason, caused.cause)
	}
	return fmt.Errorf("%w: %s", ErrCustodianProposalWithheld, withheld.Decision.BlockedReason)
}

// EvaluateCustodianProposal records the standing gate's verdict for a
// custodian proposal without executing it (observation routes).
func (e *Executor) EvaluateCustodianProposal(
	ctx context.Context, proposal CustodianProposal,
) ActionPolicyDecision {
	e.policyMu.RLock()
	gate := e.policyGate
	e.policyMu.RUnlock()
	if gate == nil {
		return ActionPolicyDecision{
			Decision: PolicyDecisionBlocked, RiskTier: "unknown",
			BlockedReason: StandingGateUnavailable,
		}
	}
	return standingPolicyDecision(gate.Authorize(ctx, custodianRequest(proposal)))
}

func custodianRequest(proposal CustodianProposal) policy.ActionRequest {
	request := policy.ActionRequest{
		SQL: proposal.SQL, Feature: custodianPolicyFeature(proposal),
		TargetObjs: append([]string(nil), proposal.TargetObjects...),
		IsReplica:  proposal.IsReplica,
		Deadline:   proposal.Deadline,
		Evidence:   cloneCustodianEvidence(proposal.Evidence),
		// Custodians remediate incident families, so the earned-autonomy
		// ledger governs them (M7).
		IncidentFamily:     custodianIncidentFamily(proposal.Feature),
		EvidenceObservedAt: proposal.ObservedAt,
	}
	if contract, feature, ok := contractForCustodianResponse(proposal.ResponseKind); ok {
		request.Contract, request.Feature = policyContract(contract), feature
		request.Evidence["action_type"] = contract.ActionType
	} else if contract, ok := contractForCustodianProposal(proposal.SQL); ok {
		request.Contract = policyContract(contract)
	}
	bindCustodianRequest(&request, proposal)
	return request
}

// Change classes of custodian plans: bloat plans, and escalations of the
// xmin holders pg_sage never signals.
const (
	bloatRemediationFeature = "bloat_remediation"
	freezeBlockerFeature    = "freeze_blocker"
)

// contractForCustodianResponse types a plan-only custodian response by its
// kind, never by Evidence["plan"] (every Plan response carries one): bloat
// plans are high-risk plan_bloat_remediation decisions, and holder
// escalations and catalog freeze plans high-risk diagnose_freeze_blockers
// ones, which the gate parks (a plan has no SQL to validate) and the
// ledger folds by fingerprint. A session signal keeps the contract of its SQL.
func contractForCustodianResponse(kind string) (ActionContract, string, bool) {
	switch freeze.ResponseKind(kind) {
	case freeze.ResponsePlanRepack, freeze.ResponseParkBloat:
		return planBloatRemediationContract(), bloatRemediationFeature, true
	case freeze.ResponseEscalateHolder:
		return freezeHolderEscalationContract(), freezeBlockerFeature, true
	case freeze.ResponsePlanCatalogFreeze:
		// A catalog VACUUM needs MAINTAIN or ownership: an operator plan.
		return freezeHolderEscalationContract(), string(policy.ChangeFreeze), true
	}
	return ActionContract{}, "", false
}

// freezeHolderEscalationContract is diagnose_freeze_blockers raised to a
// high-risk, review-only plan: resolving a prepared transaction or a slot
// is an operator's call. The incident diagnostic keeps its safe tier.
func freezeHolderEscalationContract() ActionContract {
	contract := diagnoseFreezeBlockersContract()
	contract.BaseRiskTier = "high"
	contract.Guardrails = []string{"direct execution disabled", "manual review required"}
	return contract
}
func cloneCustodianEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// SubmitCustodianProposal runs a custodian change through Apply and
// post-checks that it took effect.
func (e *Executor) SubmitCustodianProposal(
	ctx context.Context, proposal CustodianProposal,
) error {
	_, err := e.SubmitCustodianProposalDecision(ctx, proposal)
	return err
}

// custodianRun carries one custodian change from execution to its
// post-check.
type custodianRun struct {
	executor *Executor
	proposal CustodianProposal
	finding  analyzer.Finding
	baseline custodianBaseline
	managed  bool
	// decision is the gate's verdict the change ran under; executed
	// reports that the change itself succeeded.
	decision ActionPolicyDecision
	executed bool
}

// execute captures the verification baseline, applies the change (through
// the provider's adapter for managed configuration) and records it.
func (r *custodianRun) execute(
	ctx context.Context, decision ActionPolicyDecision,
) (int64, error) {
	e, decisionID := r.executor, decision.DecisionID
	r.decision = decision
	baseline, err := e.captureCustodianBaseline(ctx, r.proposal)
	if err != nil {
		return 0, fmt.Errorf("capture custodian verification baseline: %w", err)
	}
	r.baseline = baseline
	managedResult, managed, execErr := e.applyManagedCustodianConfig(ctx, r.proposal)
	r.managed = managed
	session := map[string]any{}
	if !managed {
		execErr = e.executeCustodianSQL(ctx, r.proposal.SQL, decision, session)
	}
	before := e.snapshotBeforeState(ctx, nil)
	maps.Copy(before, session)
	actionID := e.logActionWithDecision(ctx, r.finding, 0, before, decisionID, execErr)
	if execErr != nil {
		return actionID, fmt.Errorf("execute custodian proposal: %w", execErr)
	}
	r.executed = true
	if managed {
		_ = recordActionOutcome(ctx, e.pool, e.logFn, actionID,
			outcomeStatus(managedResult.InEffect), managedResult.Note)
	} else if isAlterSystem(r.proposal.SQL) {
		outcome := applyConfigChange(
			ctx, e.actionDB(), r.proposal.SQL, e.cfg.CloudEnvironment, e.logFn,
		)
		_ = recordActionOutcome(ctx, e.pool, e.logFn, actionID,
			outcomeStatus(outcome.InEffect), outcome.Note)
	}
	return actionID, nil
}

// verify post-checks the recorded change and finalizes its verification.
func (r *custodianRun) verify(ctx context.Context, actionID int64) error {
	e := r.executor
	criterion, verifyErr := "custodian_wal", error(nil)
	if !r.managed {
		criterion, verifyErr = e.verifyCustodianAction(ctx, r.proposal, r.baseline)
	}
	if verifyErr != nil {
		verificationID, _ := finalizeActionVerification(
			ctx, e.pool, actionID, "unverifiable", verifyErr.Error())
		setCustodianVerificationCriterion(ctx, e.pool, verificationID, criterion)
		_ = recordActionOutcome(ctx, e.pool, e.logFn, actionID, "failed",
			"custodian post-check failed: "+verifyErr.Error())
		return fmt.Errorf("verify custodian proposal: %w", verifyErr)
	}
	if actionID > 0 {
		updateActionSuccess(ctx, e.pool, actionID)
		setActionCustodianCriterion(ctx, e.pool, actionID, criterion)
		r.creditFreezeIncident(ctx, actionID)
		r.creditWALIncident(ctx, actionID)
	}
	return nil
}

type custodianBaseline struct {
	horizon *freezeHorizon
	disk    *runway.DiskMeasure // before a WAL bound, for incident credit
}

func (e *Executor) captureCustodianBaseline(
	ctx context.Context, proposal CustodianProposal,
) (custodianBaseline, error) {
	if isWALBound(proposal) {
		return custodianBaseline{disk: e.walDiskBaseline(ctx)}, nil
	}
	if proposal.Feature != "freeze" || len(proposal.TargetObjects) != 1 {
		return custodianBaseline{}, nil
	}
	horizon, err := e.readFreezeHorizon(ctx, proposal.TargetObjects[0])
	if err != nil {
		return custodianBaseline{}, err
	}
	return custodianBaseline{horizon: &horizon}, nil
}

func (e *Executor) verifyCustodianAction(
	ctx context.Context, proposal CustodianProposal, baseline custodianBaseline,
) (string, error) {
	if proposal.Feature == "freeze_blocker" {
		return e.verifyXminBlocker(ctx, proposal.Evidence)
	}
	if proposal.Feature == "freeze" && baseline.horizon != nil {
		after, err := e.readFreezeHorizon(ctx, proposal.TargetObjects[0])
		if err != nil || after.xidAge > baseline.horizon.xidAge {
			return "custodian_freeze", fmt.Errorf("freeze horizon did not improve")
		}
		return "custodian_freeze", nil
	}
	if isAlterSystem(proposal.SQL) {
		return "custodian_wal", e.verifyWALBound(ctx, proposal.SQL)
	}
	if isDDLMutation(proposal.SQL) && len(proposal.TargetObjects) == 1 {
		var exists bool
		err := e.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL",
			proposal.TargetObjects[0]).Scan(&exists)
		if err != nil || !exists {
			return "custodian_schema", fmt.Errorf("schema target post-check failed")
		}
		return "custodian_schema", nil
	}
	return "custodian_unknown", fmt.Errorf("no custodian post-check is available")
}

// verifyXminBlocker clears a holder once its PID is gone, its backend was
// restarted (another backend_start) or it holds neither a snapshot nor an
// XID: an idle writer holds back the horizon through its XID alone.
func (e *Executor) verifyXminBlocker(
	ctx context.Context, evidence map[string]any,
) (string, error) {
	const criterion = "custodian_xmin_blocker"
	pid, ok := evidence["pid"].(int)
	if !ok || pid <= 0 {
		return criterion, fmt.Errorf("blocker PID evidence is missing")
	}
	text, _ := evidence["backend_start"].(string)
	start, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return criterion, fmt.Errorf("blocker pid %d backend_start evidence %q is unusable: %w",
			pid, text, err)
	}
	var holds bool
	err = e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT EXISTS(SELECT 1 FROM pg_stat_activity
		WHERE pid = $1 AND backend_start = $2
		  AND (backend_xmin IS NOT NULL OR backend_xid IS NOT NULL))`, pid, start).Scan(&holds)
	if err != nil {
		return criterion, fmt.Errorf("read xmin blocker pid %d: %w", pid, err)
	}
	if holds {
		return criterion, fmt.Errorf("xmin blocker pid %d still holds a snapshot or an XID", pid)
	}
	return criterion, nil
}

func setActionCustodianCriterion(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, criterion string,
) {
	_, _ = pool.Exec(ctx, `UPDATE sage.verification
		SET criterion=jsonb_build_object('kind',$2::text), updated_at=now()
		WHERE action_log_id=$1`,
		actionID, criterion)
}

func setCustodianVerificationCriterion(
	ctx context.Context, pool *pgxpool.Pool, verificationID int64, criterion string,
) {
	if verificationID <= 0 {
		return
	}
	_, _ = pool.Exec(ctx, `UPDATE sage.verification
		SET criterion=jsonb_build_object('kind',$2), updated_at=now() WHERE id=$1`,
		verificationID, criterion)
}

// executeCustodianSQL runs a custodian statement under the decision's lock
// timeout (the policy lock ceiling caps in-transaction statements); a
// top-level statement records what its session changed in state.
func (e *Executor) executeCustodianSQL(
	ctx context.Context, sql string, decision ActionPolicyDecision, state map[string]any,
) error {
	if err := ValidateExecutorSQL(sql); err != nil {
		return err
	}
	if _, _, isSignal := parseBackendSignal(sql); isSignal {
		return ErrBackendApprovalRequired
	}
	if err := e.checkGUCValueSafety(ctx, sql); err != nil {
		return err
	}
	timeout := e.ddlTimeoutFor(sql)
	lockOpt := e.lockOption(sql, decision)
	var err error
	if statementRunsTopLevel(sql) {
		err = e.execConcurrentDDL(ctx, sql, state, lockOpt)
	} else {
		err = ExecInTransaction(ctx, e.actionDB(), sql, timeout, lockOpt)
	}
	if err == nil {
		e.notifyPostDDL(ctx, sql)
	}
	return err
}

func contractForCustodianProposal(sql string) (ActionContract, bool) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return ActionContract{}, false
	}
	return ContractForActionType(ActionTypeForSQL(sql))
}

func custodianPolicyFeature(proposal CustodianProposal) string {
	if proposal.Feature == "freeze_blocker" {
		return string(policy.ChangeFreeze)
	}
	if proposal.Feature == "wal" && isAlterSystem(proposal.SQL) {
		return string(policy.ChangeConfigGUC)
	}
	return proposal.Feature
}

func custodianFinding(proposal CustodianProposal) analyzer.Finding {
	target := strings.Join(proposal.TargetObjects, ",")
	return analyzer.Finding{
		Category: proposal.Feature, ObjectType: "custodian",
		ObjectIdentifier: target, Title: proposal.Feature + " custodian action",
		RecommendedSQL: proposal.SQL, ActionRisk: "safe",
	}
}
