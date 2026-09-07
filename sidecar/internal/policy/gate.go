package policy

import (
	"context"
	"fmt"
	"time"
)

type authorizationGate struct {
	config GateConfig
}

func NewGate(config GateConfig) Gate {
	return &authorizationGate{config: config}
}

func (gate *authorizationGate) Authorize(
	ctx context.Context,
	req ActionRequest,
) Decision {
	runtime, err := gate.runtime(ctx, req)
	if err != nil {
		return gate.finish(ctx, req, blocked(ReasonPolicyUnavailable, err.Error()))
	}
	if decision, stop := hardStop(runtime, req); stop {
		return gate.finish(ctx, req, decision)
	}
	if decision, stop := gate.validateRequest(req); stop {
		return gate.finish(ctx, req, decision)
	}
	if observeOnly(runtime) {
		return gate.finish(ctx, req, blockedAs(VerdictObserveOnly, ReasonObserveOnly))
	}
	doc, err := gate.policy(ctx, req)
	if err != nil || ValidateDocument(doc) != nil {
		return gate.finish(ctx, req, blocked(ReasonPolicyUnavailable, errorDetail(err)))
	}
	changeClass := ChangeClass(req.Feature)
	if !containsChangeClass(doc.AllowedChangeClasses, changeClass) {
		return gate.finish(ctx, req, gate.decision(
			req, VerdictBlocked, ReasonChangeClassNotAllowed))
	}
	if hasGuardrail(*req.Contract, GuardrailApprovalRequired) {
		return gate.finish(ctx, req, gate.decision(
			req, VerdictQueueApproval, ReasonApprovalRequired))
	}
	if containsChangeClass(doc.ApprovalRequiredClasses, changeClass) {
		return gate.finish(ctx, req, gate.decision(
			req, VerdictQueueApproval, ReasonApprovalRequired))
	}
	usage, err := gate.usage(ctx, req)
	if err != nil {
		return gate.finish(ctx, req, blocked(ReasonPolicyUnavailable, err.Error()))
	}
	if decision, stop := limitDecision(doc, usage); stop {
		return gate.finish(ctx, req, decisionForRequest(req, decision))
	}
	if decision, stop := gate.windowDecision(doc, req); stop {
		return gate.finish(ctx, req, decision)
	}
	return gate.finish(ctx, req, tierDecision(runtime, req))
}

func containsChangeClass(classes []ChangeClass, target ChangeClass) bool {
	for _, class := range classes {
		if class == target {
			return true
		}
	}
	return false
}

func (gate *authorizationGate) runtime(
	ctx context.Context,
	req ActionRequest,
) (RuntimeState, error) {
	if gate.config.Runtime == nil {
		return RuntimeState{}, fmt.Errorf("runtime state is unavailable")
	}
	return gate.config.Runtime(ctx, req)
}

func hardStop(runtime RuntimeState, req ActionRequest) (Decision, bool) {
	if !runtime.ExecutorEnabled {
		return blocked(ReasonExecutorDisabled, ""), true
	}
	if runtime.EmergencyStop {
		return blocked(ReasonEmergencyStop, ""), true
	}
	if runtime.IsReplica && !requestIsReadOnly(req) {
		return blocked(ReasonReplicaMutation, ""), true
	}
	return Decision{}, false
}

func requestIsReadOnly(req ActionRequest) bool {
	return req.Contract != nil && req.Contract.RiskTier == RiskReadOnly
}

func (gate *authorizationGate) validateRequest(req ActionRequest) (Decision, bool) {
	if req.Contract == nil || req.Contract.ActionType == "" {
		return blockedAs(VerdictPark, ReasonNoTypedContract), true
	}
	if guardrail, unknown := unknownGuardrail(*req.Contract); unknown {
		decision := gate.decision(req, VerdictBlocked, ReasonUnknownGuardrail)
		decision.Detail = string(guardrail)
		return decision, true
	}
	if trustedInternalControl(req) {
		return Decision{}, false
	}
	if gate.config.ValidateSQL == nil || gate.config.ValidateSQL(req.SQL) != nil {
		return gate.decision(req, VerdictPark, ReasonNoTypedContract), true
	}
	return Decision{}, false
}

func trustedInternalControl(req ActionRequest) bool {
	if !req.InternalControl || req.SQL != "" || req.Contract == nil {
		return false
	}
	switch req.Contract.ActionType {
	case "declare_table_contract", "register_consumer":
		return true
	default:
		return false
	}
}

func unknownGuardrail(contract ActionContract) (Guardrail, bool) {
	for _, guardrail := range contract.Guardrails {
		if guardrail != GuardrailApprovalRequired {
			return guardrail, true
		}
	}
	return "", false
}

func observeOnly(runtime RuntimeState) bool {
	return runtime.TrustLevel == TrustObservation ||
		runtime.ExecutionMode == ExecutionManual
}

func (gate *authorizationGate) policy(
	ctx context.Context,
	req ActionRequest,
) (Document, error) {
	if gate.config.Policy == nil {
		return Document{}, ErrPolicyNotFound
	}
	return gate.config.Policy(ctx, req)
}

func (gate *authorizationGate) usage(
	ctx context.Context,
	req ActionRequest,
) (LimitUsage, error) {
	if gate.config.Usage == nil {
		return LimitUsage{}, nil
	}
	return gate.config.Usage(ctx, req)
}

func limitDecision(doc Document, usage LimitUsage) (Decision, bool) {
	if budgetExceeded(doc.Budgets.StorageBytes, usage.StorageBytes) {
		return blockedAs(VerdictPark, ReasonBudgetExceeded), true
	}
	if positiveExceeded(doc.BlastRadius.MaxRowsRewritten, usage.RowsRewritten, false) ||
		positiveExceeded(doc.BlastRadius.MaxTablesPerWindow, usage.TablesInWindow, false) {
		return blockedAs(VerdictPark, ReasonBlastRadiusExceeded), true
	}
	limit := doc.RateLimits.MaxSelfInitiatedChangesPerWindow
	if positiveExceeded(limit, usage.SelfInitiatedChangesInWindow, true) {
		return blockedAs(VerdictPark, ReasonRateLimitExceeded), true
	}
	return Decision{}, false
}

func budgetExceeded(limit BudgetLimit, usage int64) bool {
	if limit.NoCap() {
		return false
	}
	value, ok := limit.Value()
	if !ok {
		return true
	}
	return value == 0 || usage > value
}

func positiveExceeded(limit, usage int64, includeEqual bool) bool {
	if limit == 0 {
		return usage > 0
	}
	if includeEqual {
		return usage >= limit
	}
	return usage > limit
}

func (gate *authorizationGate) windowDecision(
	doc Document,
	req ActionRequest,
) (Decision, bool) {
	if req.Contract.RiskTier != RiskModerate && req.Contract.RiskTier != RiskHigh {
		return Decision{}, false
	}
	if gate.config.WindowObserved != nil {
		gate.config.WindowObserved()
	}
	now := gate.now()
	if inAnyWindow(doc.MaintenanceWindows, now) {
		return Decision{}, false
	}
	if validDeadlineOverride(doc, req.Deadline, now) {
		decision := gate.decision(req, VerdictExecute, ReasonDeadlineOverride)
		decision.OffWindowOK = true
		return decision, true
	}
	return gate.decision(
		req, VerdictBlocked, ReasonOutsideMaintenanceWindow), true
}

func inAnyWindow(expressions []string, now time.Time) bool {
	for _, expression := range expressions {
		window, err := ParseWindow(expression)
		if err == nil && window.Contains(now) {
			return true
		}
	}
	return false
}

func validDeadlineOverride(
	doc Document,
	deadline *DeadlineContext,
	now time.Time,
) bool {
	if deadline == nil || !deadline.HardAt.After(now) {
		return false
	}
	if deadline.Kind != DeadlineXID && deadline.Kind != DeadlineDisk {
		return false
	}
	return doc.DeadlineOverrides[deadline.Kind]
}

func tierDecision(runtime RuntimeState, req ActionRequest) Decision {
	if runtime.ExecutionMode == ExecutionApproval {
		return decisionForRequest(
			req, blockedAs(VerdictQueueApproval, ReasonApprovalRequired))
	}
	switch req.Contract.RiskTier {
	case RiskReadOnly, RiskSafe:
		if runtime.TrustLevel == TrustAdvisory ||
			runtime.TrustLevel == TrustAutonomous {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
	case RiskModerate:
		if runtime.TrustLevel == TrustAutonomous {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
		if runtime.TrustLevel == TrustAdvisory {
			return decisionForRequest(
				req, blockedAs(VerdictQueueApproval, ReasonApprovalRequired))
		}
	case RiskHigh:
		return decisionForRequest(
			req, blockedAs(VerdictQueueApproval, ReasonApprovalRequired))
	}
	return decisionForRequest(req, blocked(ReasonUnknownRiskTier, ""))
}

func (gate *authorizationGate) decision(
	req ActionRequest,
	verdict Verdict,
	reason Reason,
) Decision {
	return decisionForRequest(req, blockedAs(verdict, reason))
}

func decisionForRequest(req ActionRequest, decision Decision) Decision {
	if req.Contract == nil {
		return decision
	}
	decision.RiskTier = req.Contract.RiskTier
	decision.Guardrails = append([]Guardrail(nil), req.Contract.Guardrails...)
	return decision
}

func blocked(reason Reason, detail string) Decision {
	return Decision{Verdict: VerdictBlocked, Reason: reason, Detail: detail}
}

func blockedAs(verdict Verdict, reason Reason) Decision {
	return Decision{Verdict: verdict, Reason: reason}
}

func hasGuardrail(contract ActionContract, target Guardrail) bool {
	for _, guardrail := range contract.Guardrails {
		if guardrail == target {
			return true
		}
	}
	return false
}

func (gate *authorizationGate) now() time.Time {
	if gate.config.Now != nil {
		return gate.config.Now()
	}
	return time.Now()
}

func (gate *authorizationGate) finish(
	ctx context.Context,
	req ActionRequest,
	decision Decision,
) Decision {
	decision = decisionForRequest(req, decision)
	if gate.config.RecordDecisionDetailed != nil {
		evidenceID, decisionID, err := gate.config.RecordDecisionDetailed(
			ctx, req, decision,
		)
		if err != nil {
			return blocked(ReasonPolicyUnavailable, err.Error())
		}
		decision.EvidenceID = evidenceID
		decision.DecisionID = decisionID
		return decision
	}
	if gate.config.RecordDecision == nil {
		return decision
	}
	evidenceID, err := gate.config.RecordDecision(ctx, req, decision)
	if err != nil {
		return blocked(ReasonPolicyUnavailable, err.Error())
	}
	decision.EvidenceID = evidenceID
	return decision
}

func errorDetail(err error) string {
	if err == nil {
		return "invalid policy"
	}
	return err.Error()
}
