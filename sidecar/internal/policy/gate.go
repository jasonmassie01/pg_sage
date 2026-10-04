package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type authorizationGate struct {
	config GateConfig
	// budgetMu serializes the budget section of budget-spending requests in
	// this process (and keeps at most one connection waiting on the
	// cross-process lock, GateConfig.Serialize): the recorded execute
	// decision holds its slot, so a concurrent candidate reads it.
	budgetMu sync.Mutex
}

func NewGate(config GateConfig) Gate {
	return &authorizationGate{config: config}
}

func (gate *authorizationGate) Authorize(
	ctx context.Context,
	req ActionRequest,
) Decision {
	req.ExplainFamily = false // only Explain may skip SQL validation
	if !spendsBudget(req) {
		return gate.finish(ctx, req, gate.evaluate(ctx, req))
	}
	gate.budgetMu.Lock()
	defer gate.budgetMu.Unlock()
	if gate.config.Serialize != nil {
		return gate.authorizeSerialized(ctx, req)
	}
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
	if decision, stop := gate.factDecision(ctx, req); stop {
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
	doc, decision, stop, note := gate.documentDecision(ctx, req)
	if !stop {
		decision = withDocumentBounds(doc, gate.selfInitiatedDecision(doc, runtime, req))
	}
	return note.stamp(gate.restrictAutonomy(ctx, doc, runtime, req, decision))
}

// selfInitiatedDecision lets trust, mode, tier flags and ramp decide first.
// The refusal set, degraded SQL validation and the windows (or a deadline
// override of them) can only restrict an otherwise-execute verdict.
func (gate *authorizationGate) selfInitiatedDecision(
	doc Document, runtime RuntimeState, req ActionRequest,
) Decision {
	tier := tierDecision(runtime, req, gate.now(), gate.selfGoverned(req))
	if tier.Verdict != VerdictExecute {
		return tier
	}
	if decision, refused := refusalDecision(doc, req); refused {
		return decision
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
// approval requirements and the usage limits of the request's budget kind.
func (gate *authorizationGate) documentDecision(
	ctx context.Context, req ActionRequest,
) (Document, Decision, bool, budgetNote) {
	doc, err := gate.policy(ctx, req)
	if err != nil || ValidateDocument(doc) != nil {
		return doc, blocked(ReasonPolicyUnavailable, errorDetail(err)), true, budgetNote{}
	}
	if req.Contract.RiskTier == RiskReadOnly {
		return doc, Decision{}, false, budgetNote{} // diagnostics mutate nothing
	}
	changeClass := ChangeClass(req.Feature)
	if !containsChangeClass(doc.AllowedChangeClasses, changeClass) {
		decision := gate.decision(req, VerdictBlocked, ReasonChangeClassNotAllowed)
		return doc, decision, true, budgetNote{}
	}
	if hasGuardrail(*req.Contract, GuardrailApprovalRequired) ||
		isBackendSignal(req.Contract.ActionType) ||
		containsChangeClass(doc.ApprovalRequiredClasses, changeClass) {
		decision := gate.decision(req, VerdictQueueApproval, ReasonApprovalRequired)
		return doc, decision, true, budgetNote{}
	}
	usage, err := gate.usage(ctx, req)
	if err != nil {
		return doc, blocked(ReasonPolicyUnavailable, err.Error()), true, budgetNote{}
	}
	kind := BudgetKindFor(req)
	note := budgetNote{read: true, kind: kind, rows: usage.RequestRowsRewritten,
		bypass: BudgetBypassFor(req, gate.now())}
	if decision, stop := limitDecision(doc, kind, usage, note.bypass != ""); stop {
		return doc, decisionForRequest(req, decision), true, note
	}
	return doc, Decision{}, false, note
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

// The trust ramp (AI-SRE-SPEC trust model): unattended SAFE actions wait
// SpecSafeRampAge after the ramp start and MODERATE ones
// SpecModerateRampAge, unless trust.ramp_*_hours configure another age.
const (
	SpecSafeRampAge     = 8 * 24 * time.Hour
	SpecModerateRampAge = 31 * 24 * time.Hour
	// MinRampAge is the shortest configured ramp the gate honours.
	MinRampAge = time.Hour
)

// rampAge is the ramp an action waits for. Zero or negative configured
// ages take the spec ramp; a configured ramp never drops below
// MinRampAge; and an action that cannot be rolled back (anything the
// earned-autonomy reversibility cap holds below L3) never waits less than
// the spec ramp, so fast elevation only shortens trust for reversible
// actions.
func rampAge(configured, spec time.Duration, rollback RollbackClass) time.Duration {
	if configured <= 0 {
		return spec
	}
	if reversibilityCap(rollback) < autonomyExecuteLevel {
		return max(configured, spec)
	}
	return max(configured, MinRampAge)
}

// RampAge is the trust ramp an action of rollback class waits for, by the
// gate's own rule (rampAge). The earned ledger uses it as the minimum
// observation time before it may propose a promotion (roadmap 1.2).
func RampAge(configured, spec time.Duration, rollback RollbackClass) time.Duration {
	return rampAge(configured, spec, rollback)
}

// rampSatisfied reports an elapsed ramp. A self-initiated class the
// ledger governs (ledgerDecides) is never held or granted by the clock:
// its earned level decides, and the ramp only floors promotions.
func rampSatisfied(runtime RuntimeState, minimum time.Duration, now time.Time,
	ledgerDecides bool) bool {
	if ledgerDecides {
		return true
	}
	return !runtime.RampStart.IsZero() && now.Sub(runtime.RampStart) >= minimum
}

func tierDecision(runtime RuntimeState, req ActionRequest, now time.Time,
	ledgerDecides bool) Decision {
	if runtime.ExecutionMode == ExecutionApproval {
		return decisionForRequest(
			req, blockedAs(VerdictQueueApproval, ReasonApprovalRequired))
	}
	trusted := runtime.TrustLevel == TrustAdvisory || runtime.TrustLevel == TrustAutonomous
	rollback := req.Contract.RollbackClass
	safeRampAge := rampAge(runtime.SafeRampAge, SpecSafeRampAge, rollback)
	moderateRampAge := rampAge(runtime.ModerateRampAge, SpecModerateRampAge, rollback)
	switch req.Contract.RiskTier {
	case RiskReadOnly:
		if trusted {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
	case RiskSafe:
		if trusted && (!runtime.Tier3Safe ||
			!rampSatisfied(runtime, safeRampAge, now, ledgerDecides)) {
			return decisionForRequest(req, blocked(ReasonTrustRampNotSatisfied, ""))
		}
		if trusted {
			return decisionForRequest(req, blockedAs(VerdictExecute, ReasonAuthorized))
		}
	case RiskModerate:
		if runtime.TrustLevel == TrustAutonomous &&
			(!runtime.Tier3Moderate ||
				!rampSatisfied(runtime, moderateRampAge, now, ledgerDecides)) {
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
	recorded, _ := gate.record(ctx, req, decision) // a failed record is a blocked verdict
	return recorded
}

// record stamps and records the decision. A failed record returns a
// blocked policy_unavailable decision and the error.
func (gate *authorizationGate) record(
	ctx context.Context, req ActionRequest, decision Decision,
) (Decision, error) {
	decision = decisionForRequest(req, decision)
	if gate.config.RecordDecisionDetailed != nil {
		evidenceID, decisionID, err := gate.config.RecordDecisionDetailed(ctx, req, decision)
		if err != nil {
			return blocked(ReasonPolicyUnavailable, err.Error()), err
		}
		decision.EvidenceID, decision.DecisionID = evidenceID, decisionID
		return decision, nil
	}
	if gate.config.RecordDecision == nil {
		return decision, nil
	}
	evidenceID, err := gate.config.RecordDecision(ctx, req, decision)
	if err != nil {
		return blocked(ReasonPolicyUnavailable, err.Error()), err
	}
	decision.EvidenceID = evidenceID
	return decision, nil
}

func errorDetail(err error) string {
	if err == nil {
		return "invalid policy"
	}
	return err.Error()
}
