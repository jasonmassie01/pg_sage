package api

import (
	"context"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

const reasonUnknownActionType = "unknown action type"

// enrichCasesActionPolicies shows each candidate's standing-policy verdict
// for one database's cases, as the executor would decide it (cases-04): a
// candidate whose statement the executor runs under its type is explained
// with that statement and its finding (targets, facts, per-table usage);
// any other by its family, once per type. Everything goes to the gate in
// one call, against one policy snapshot per database. Unknown types are
// blocked without being sent. No instance or executor, a stopped instance,
// or a cancelled ctx fails closed. Provider applicability is applied per
// case afterwards. It returns each candidate's gate detail (details[i][j]
// for candidate j of case i; "" for an unknown type), which the case view
// does not carry and the policy explain route shows (policy-07).
func enrichCasesActionPolicies(
	ctx context.Context, all []cases.Case, inst *fleet.DatabaseInstance,
) (details [][]string) {
	isReplica, provider := false, ""
	if inst != nil {
		capabilities := inst.SnapshotStatus().Capabilities
		isReplica, provider = capabilities.IsReplica, capabilities.Provider
	}
	requests, slots := caseExplainRequests(all)
	var decisions []executor.ActionPolicyDecision
	if len(requests) > 0 {
		decisions = fleet.InstanceActionExplainer(inst)(ctx, requests, isReplica)
	}
	details = make([][]string, len(all))
	for i := range all {
		details[i] = make([]string, len(all[i].ActionCandidates))
		for j := range all[i].ActionCandidates {
			candidate := &all[i].ActionCandidates[j]
			slot := slots[i][j]
			if slot < 0 {
				candidate.BlockedReason = reasonUnknownActionType
				continue
			}
			applyCandidatePolicy(candidate, decisions[slot])
			details[i][j] = decisions[slot].Detail
		}
		cases.ApplyProviderApplicability(&all[i], provider)
	}
	return details
}

// caseExplainRequests lists the explanations the cases need: one per
// concrete candidate, one per family type otherwise, in first-seen order.
// slots[i][j] is candidate j of case i's request, or -1 for an unknown
// type.
func caseExplainRequests(all []cases.Case) ([]executor.ExplainActionRequest, [][]int) {
	var requests []executor.ExplainActionRequest
	familySlot := map[string]int{}
	slots := make([][]int, len(all))
	for i := range all {
		slots[i] = make([]int, len(all[i].ActionCandidates))
		for j, candidate := range all[i].ActionCandidates {
			contract, ok := executor.ContractForActionType(candidate.ActionType)
			switch {
			case !ok:
				slots[i][j] = -1
			case executorRunsCandidate(candidate):
				slots[i][j] = len(requests)
				requests = append(requests, executor.ExplainActionRequest{
					Contract: contract, Finding: candidateFinding(candidate),
				})
			default:
				slot, seen := familySlot[candidate.ActionType]
				if !seen {
					slot = len(requests)
					familySlot[candidate.ActionType] = slot
					requests = append(requests, executor.ExplainActionRequest{Contract: contract})
				}
				slots[i][j] = slot
			}
		}
	}
	return requests, slots
}

// executorRunsCandidate reports a candidate whose statement the executor
// would run under the candidate's own contract. Overlays (script-only
// preflights, diagnostics, prepared rewrites) are explained by family.
func executorRunsCandidate(candidate cases.ActionCandidate) bool {
	return candidate.ProposedSQL != "" &&
		executor.ActionTypeForSQL(candidate.ProposedSQL) == candidate.ActionType
}

// candidateFinding is the finding the executor would authorize for the
// candidate: its statement and, when projected from a finding, that
// finding's object, category, detail and undo.
func candidateFinding(candidate cases.ActionCandidate) analyzer.Finding {
	finding := analyzer.Finding{RecommendedSQL: candidate.ProposedSQL}
	if src := candidate.Source; src != nil {
		finding.Category, finding.Severity = src.Category, string(src.Severity)
		finding.ObjectType, finding.ObjectIdentifier = src.ObjectType, src.ObjectIdentifier
		finding.Title, finding.Detail = src.Title, src.Detail
		finding.RollbackSQL, finding.DatabaseName = src.RollbackSQL, src.DatabaseName
	}
	return finding
}

// applyCandidatePolicy copies a verdict onto one candidate. Guardrails are
// copied per candidate, since one verdict is shared by every candidate of
// its type.
func applyCandidatePolicy(
	candidate *cases.ActionCandidate, decision executor.ActionPolicyDecision,
) {
	candidate.PolicyDecision = &cases.ActionPolicyDecision{
		Decision:                  decision.Decision,
		RiskTier:                  decision.RiskTier,
		RequiresApproval:          decision.RequiresApproval,
		RequiresMaintenanceWindow: decision.RequiresMaintenanceWindow,
		BlockedReason:             decision.BlockedReason,
		Guardrails:                cloneGuardrails(decision.Guardrails),
		Provider:                  decision.Provider,
	}
	candidate.Guardrails = cloneGuardrails(decision.Guardrails)
	candidate.RequiresApproval = decision.RequiresApproval
	candidate.RequiresMaintenanceWindow = decision.RequiresMaintenanceWindow
	if decision.BlockedReason != "" {
		candidate.BlockedReason = decision.BlockedReason
	}
}

// cloneGuardrails copies guardrails, keeping nil as nil so the JSON shape
// (omitted or present) is unchanged.
func cloneGuardrails(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}
