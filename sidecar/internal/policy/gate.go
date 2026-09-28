package policy

import (
	"context"
	"fmt"
	"strings"
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
	req.ExplainFamily = false // only Explain may skip SQL validation
	return gate.finish(ctx, req, gate.evaluate(ctx, req))
}

// Explain runs the same evaluation as Authorize and records nothing.
func (gate *authorizationGate) Explain(ctx context.Context, req ActionRequest) Decision {
	return decisionForRequest(req, gate.evaluate(ctx, req))
}

func (gate *authorizationGate) evaluate(ctx context.Context, req ActionRequest) Decision {
	runtime, err := gate.runtime(ctx, req)
	if err != nil {
		return blocked(ReasonPolicyUnavailable, err.Error())
	}
	if decision, stop := hardStop(runtime, req); stop {
		return decision
	}
	if decision, stop := gate.validateRequest(req); stop {
		return decision
	}
	if decision, stop := providerDecision(runtime, req); stop {
		return decision
	}
	if req.OperatorApproved {
		return gate.operatorDecision(ctx, runtime, req)
	}
	if observeOnly(runtime) {
		return blockedAs(VerdictObserveOnly, ReasonObserveOnly)
	}
	if runtime.TrustLevel != TrustAdvisory && runtime.TrustLevel != TrustAutonomous {
		return blocked(ReasonUnknownTrustLevel, runtime.TrustLevel)
	}
	doc, decision, stop := gate.documentDecision(ctx, req)
	if stop {
		return decision
	}
	// Trust, mode, tier flags and ramp decide first; a window (or a deadline
	// override of it) can only restrict an otherwise-execute verdict.
	tier := tierDecision(runtime, req, gate.now())
	if tier.Verdict != VerdictExecute {
		return tier
	}
	if runtime.SQLValidationDegraded && req.Contract.RiskTier != RiskReadOnly {
		return decisionForRequest(req, degradedValidationDecision())
	}
	if decision, stop := gate.windowDecision(doc, runtime, req); stop {
		return decision
	}
	return tier
}

// providerDecision blocks actions whose contract excludes the target's
// provider (formerly checked only by the legacy executor engine).
func providerDecision(runtime RuntimeState, req ActionRequest) (Decision, bool) {
	support := req.Contract.ProviderSupport
	if len(support) == 0 {
		return Decision{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(runtime.Provider))
	if provider == "" || provider == "self-managed" {
		provider = "postgres"
	}
	for _, item := range support {
		if strings.EqualFold(provider, item) {
			return Decision{}, false
		}
	}
	return blocked(ReasonProviderUnsupported, "provider "+provider), true
}

// documentDecision applies the standing policy document: change class,
// approval requirements and usage limits.
func (gate *authorizationGate) documentDecision(
	ctx context.Context, req ActionRequest,
) (Document, Decision, bool) {
	doc, err := gate.policy(ctx, req)
	if err != nil || ValidateDocument(doc) != nil {
		return doc, blocked(ReasonPolicyUnavailable, errorDetail(err)), true
	}
	if req.Contract.RiskTier == RiskReadOnly {
		return doc, Decision{}, false // diagnostics mutate nothing
	}
	changeClass := ChangeClass(req.Feature)
	if !containsChangeClass(doc.AllowedChangeClasses, changeClass) {
		return doc, gate.decision(req, VerdictBlocked, ReasonChangeClassNotAllowed), true
	}
	if hasGuardrail(*req.Contract, GuardrailApprovalRequired) ||
		isBackendSignal(req.Contract.ActionType) ||
		containsChangeClass(doc.ApprovalRequiredClasses, changeClass) {
		return doc, gate.decision(req, VerdictQueueApproval, ReasonApprovalRequired), true
	}
	usage, err := gate.usage(ctx, req)
	if err != nil {
		return doc, blocked(ReasonPolicyUnavailable, err.Error()), true
	}
	if decision, stop := limitDecision(doc, usage); stop {
		return doc, decisionForRequest(req, decision), true
	}
	return doc, Decision{}, false
}

// isBackendSignal reports action types that cancel or terminate a session.
// They always require a human, whatever the trust level or tier.
func isBackendSignal(actionType string) bool {
	return actionType == "cancel_backend" || actionType == "terminate_backend"
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
	if trustedInternalControl(req) || (req.ExplainFamily && req.SQL == "") {
		return Decision{}, false
	}
	if gate.config.ValidateSQL == nil {
		return gate.decision(req, VerdictPark, ReasonNoTypedContract), true
	}
	if err := gate.config.ValidateSQL(req.SQL); err != nil {
		decision := gate.decision(req, VerdictPark, ReasonNoTypedContract)
		decision.Detail = err.Error()
		return decision, true
	}
	return Decision{}, false
}

func trustedInternalControl(req ActionRequest) bool {
	if !req.InternalControl || req.SQL != "" || req.Contract == nil {
		return false
	}
	switch req.Contract.ActionType {
	case "declare_table_contract", "register_consumer", "retention_delete":
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
	runtime RuntimeState,
	req ActionRequest,
) (Decision, bool) {
	if req.Contract.RiskTier != RiskModerate && req.Contract.RiskTier != RiskHigh {
		return Decision{}, false
	}
	if gate.config.WindowObserved != nil {
		gate.config.WindowObserved()
	}
	now := gate.now()
	configured := req.Contract.RiskTier != RiskModerate || runtime.InConfiguredWindow
	if configured && inAnyWindow(doc.MaintenanceWindows, now) {
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

const (
	safeRampAge     = 8 * 24 * time.Hour
	moderateRampAge = 31 * 24 * time.Hour
)

func rampSatisfied(runtime RuntimeState, minimum time.Duration, now time.Time) bool {
	return !runtime.RampStart.IsZero() && now.Sub(runtime.RampStart) >= minimum
}

func tierDecision(runtime RuntimeState, req ActionRequest, now time.Time) Decision {
	if runtime.ExecutionMode == ExecutionApproval {
		return decisionForRequest(
			req, blockedAs(VerdictQueueApproval, ReasonApprovalRequired))
	}
	trusted := runtime.TrustLevel == TrustAdvisory || runtime.TrustLevel == TrustAutonomous
	switch req.Contract.RiskTier {
	case RiskReadOnly:
		if trusted {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
	case RiskSafe:
		if trusted && (!runtime.Tier3Safe || !rampSatisfied(runtime, safeRampAge, now)) {
			return decisionForRequest(req, blocked(ReasonTrustRampNotSatisfied, ""))
		}
		if trusted {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
	case RiskModerate:
		if runtime.TrustLevel == TrustAutonomous &&
			(!runtime.Tier3Moderate || !rampSatisfied(runtime, moderateRampAge, now)) {
			return decisionForRequest(req, blocked(ReasonTrustRampNotSatisfied, ""))
		}
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

// degradedValidationDecision sends an unattended mutation to a human when
// the build lacks parse-tree SQL validation.
func degradedValidationDecision() Decision {
	return Decision{
		Verdict: VerdictQueueApproval, Reason: ReasonSQLValidationDegraded,
		Detail: "built without cgo: parse-tree SQL validation is unavailable, " +
			"so unattended changes need operator approval",
	}
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
