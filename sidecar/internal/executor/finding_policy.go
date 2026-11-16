package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// evaluateFindingPolicy is the single background-action authorization path.
// It reloads runtime mode, trust, enabled state, and emergency stop for every
// candidate so a safety downgrade takes effect before the next action.
func (e *Executor) evaluateFindingPolicy(
	ctx context.Context,
	f analyzer.Finding,
	isReplica bool,
) ActionPolicyDecision {
	e.policyMu.RLock()
	gate := e.policyGate
	e.policyMu.RUnlock()
	if gate == nil {
		// The standing gate is the only authority (G4-I01); production
		// installs it before anything runs, so its absence fails closed.
		contract, _ := contractForFinding(f)
		return noStandingPolicyDecision(contract)
	}
	return e.evaluateStandingPolicy(ctx, gate, f, isReplica)
}

func (e *Executor) evaluateStandingPolicy(
	ctx context.Context, gate policy.Gate, finding analyzer.Finding, isReplica bool,
) ActionPolicyDecision {
	return standingPolicyDecision(gate.Authorize(ctx, findingRequest(finding, isReplica)))
}

// evaluateRollbackPolicy authorizes the rollback of a finding's executed
// change. It is marked a rollback: undoing pg_sage's own change is never
// withheld by the trust ledger (roadmap 1.2) nor parked by the kind budgets
// (policy.BudgetBypassFor); the rest of the gate binds. actionID (0:
// unknown) is the reverted action, named in the decision (policy-05).
func (e *Executor) evaluateRollbackPolicy(
	ctx context.Context, f analyzer.Finding, actionID int64,
) ActionPolicyDecision {
	gate := e.StandingPolicyGate()
	if gate == nil {
		contract, _ := contractForFinding(f)
		return noStandingPolicyDecision(contract)
	}
	// A rollback re-runs pg_sage's recorded inverse, which names another
	// object than the forward finding (a dropped index's table): it is not
	// target-bound.
	request := unboundFindingRequest(f, false)
	if request.Contract == nil && isExposureInverseSQL(f.RecommendedSQL) {
		// An exposure rollback (re-GRANT, RLS disable) has a contract only
		// as a rollback: the forward contract it reverts (fleetpol-02b).
		if c, ok := ContractForActionType(exposureActionType(f.RecommendedSQL)); ok {
			request.Contract = policyContract(c)
		}
	}
	request.Rollback = true
	request.Evidence = map[string]any{"source": rollbackSource(false)}
	if actionID > 0 {
		request.Evidence["rolled_back_action_id"] = actionID
	}
	return standingPolicyDecision(gate.Authorize(ctx, request))
}

// findingRequest is the standing-gate request for a background finding,
// its statement bound to the object the finding names (executor-08).
func findingRequest(finding analyzer.Finding, isReplica bool) policy.ActionRequest {
	request := unboundFindingRequest(finding, isReplica)
	bindFindingRequest(&request, finding)
	return request
}

func unboundFindingRequest(finding analyzer.Finding, isReplica bool) policy.ActionRequest {
	var contract *policy.ActionContract
	if found, ok := contractForFinding(finding); ok {
		contract = policyContract(found)
	}
	return contractFindingRequest(finding, contract, isReplica)
}

// contractFindingRequest is the gate request for finding under contract
// (nil when no contract runs it), before its statement is bound: the
// request the executor authorizes and the Cases view explains.
func contractFindingRequest(
	finding analyzer.Finding, contract *policy.ActionContract, isReplica bool,
) policy.ActionRequest {
	request := policy.ActionRequest{
		SQL: finding.RecommendedSQL, Feature: featureForFinding(finding),
		TargetObjs:            targetObjectsForFinding(finding),
		TargetsNameNoRelation: targetsNameNoRelation(finding),
		IsReplica:             isReplica,
		Contract:              contract,
	}
	if contract != nil {
		requireApprovalWithoutWhatIf(finding, contract)
	}
	if gateSQL, targets, ok := replaceGateView(finding.RecommendedSQL); ok {
		request.SQL, request.TargetObjs = gateSQL, targets
		request.TargetsNameNoRelation = false
	}
	requireApproval(&request, finding)
	return request
}
func policyContract(contract ActionContract) *policy.ActionContract {
	result := &policy.ActionContract{
		ActionType: contract.ActionType, RiskTier: policy.RiskTier(contract.BaseRiskTier),
		ProviderSupport: append([]string(nil), contract.ProviderSupport...),
		RollbackClass:   policy.RollbackClass(contract.RollbackClass),
		DropKind:        dropKindForActionType(contract.ActionType),
		Narrowing:       contract.Narrowing,
	}
	for _, guardrail := range contract.Guardrails {
		if isApprovalRequiredGuardrail(guardrail) {
			result.Guardrails = []policy.Guardrail{policy.GuardrailApprovalRequired}
		}
	}
	return result
}

// nonRelationObjectTypes are finding object types whose identifier names
// no relation ("instance", a database name, a query id): a schema-wide
// fact must not catch them (facts-01).
var nonRelationObjectTypes = map[string]bool{"configuration": true, "setting": true,
	"database": true, "instance": true, "query": true}

// targetsNameNoRelation reports a finding whose target the facts binder
// matches only as a slot (policy.ActionRequest.TargetsNameNoRelation).
func targetsNameNoRelation(finding analyzer.Finding) bool {
	return nonRelationObjectTypes[strings.ToLower(strings.TrimSpace(finding.ObjectType))]
}

func targetObjectsForFinding(finding analyzer.Finding) []string {
	target := strings.TrimSpace(finding.ObjectIdentifier)
	if target == "" {
		return nil
	}
	return []string{target}
}

func featureForFinding(finding analyzer.Finding) string {
	return changeClassForActionType(ActionTypeForSQL(finding.RecommendedSQL))
}

func standingPolicyDecision(decision policy.Decision) ActionPolicyDecision {
	result := ActionPolicyDecision{
		RiskTier: string(decision.RiskTier), BlockedReason: string(decision.Reason),
		Detail:           decision.Detail,
		RequiresApproval: decision.Verdict == policy.VerdictQueueApproval,
		RequiresMaintenanceWindow: decision.RiskTier == policy.RiskModerate ||
			decision.RiskTier == policy.RiskHigh,
		EvidenceID: decision.EvidenceID, DecisionID: decision.DecisionID,
		LockCeilingMS: decision.LockCeilingMS, SerializeMode: decision.SerializeMode,
		WindowOnly: decision.WindowOnly,
	}
	switch decision.Verdict {
	case policy.VerdictExecute:
		result.Decision = PolicyDecisionExecute
	case policy.VerdictQueueApproval:
		result.Decision = PolicyDecisionQueueApproval
	case policy.VerdictPark:
		result.Decision = PolicyDecisionParked
	case policy.VerdictObserveOnly:
		result.Decision = PolicyDecisionObserveOnly
	default:
		result.Decision = PolicyDecisionBlocked
	}
	for _, guardrail := range decision.Guardrails {
		result.Guardrails = append(result.Guardrails, string(guardrail))
	}
	if t := decision.Trusted; t != nil {
		result.TrustedVerdict, result.TrustedReason = string(t.Verdict), string(t.Reason)
		result.TrustedDetail = t.Detail
	}
	// A plain authorization is not a blocked reason; informative execute
	// reasons such as deadline_override are kept.
	if decision.Reason == policy.ReasonAuthorized ||
		decision.Reason == policy.ReasonOperatorApproved {
		result.BlockedReason = ""
	}
	return result
}

func (e *Executor) checkEmergencyStop(ctx context.Context) bool {
	e.policyMu.RLock()
	check := e.emergencyStopFn
	e.policyMu.RUnlock()
	if check != nil {
		return check(ctx)
	}
	return CheckEmergencyStop(ctx, e.pool)
}

func contractForFinding(f analyzer.Finding) (ActionContract, bool) {
	if IsIndexReplaceSQL(f.RecommendedSQL) {
		if _, err := ParseIndexReplace(f.RecommendedSQL, f.RollbackSQL); err != nil {
			return ActionContract{}, false
		}
		return replaceIndexContract(), true
	}
	if err := ValidateExecutorSQL(f.RecommendedSQL); err != nil {
		return ActionContract{}, false
	}
	if isExposureInverseSQL(f.RecommendedSQL) {
		return ActionContract{}, false // a rollback form: rollbackContractFor only
	}
	actionType := ActionTypeForSQL(f.RecommendedSQL)
	return ContractForActionType(actionType)
}
