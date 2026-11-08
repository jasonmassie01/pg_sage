package api

import (
	"context"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

const reasonUnknownActionType = "unknown action type"

// enrichCasesActionPolicies shows each candidate's standing-policy verdict
// for one database's cases. The distinct action types of every candidate
// go to the database's executor gate in one call (one policy snapshot per
// database, so the reads do not grow with the number of cases), and each
// verdict is applied by type. Unknown types are blocked without being
// sent. No instance or executor, a stopped instance, or a cancelled ctx
// fails closed. Provider applicability is applied per case afterwards.
func enrichCasesActionPolicies(
	ctx context.Context, all []cases.Case, inst *fleet.DatabaseInstance,
) {
	isReplica, provider := false, ""
	if inst != nil {
		capabilities := inst.SnapshotStatus().Capabilities
		isReplica, provider = capabilities.IsReplica, capabilities.Provider
	}
	contracts, slotByType := distinctCaseContracts(all)
	var decisions []executor.ActionPolicyDecision
	if len(contracts) > 0 {
		decisions = fleet.InstanceFamilyExplainer(inst)(ctx, contracts, isReplica)
	}
	for i := range all {
		for j := range all[i].ActionCandidates {
			candidate := &all[i].ActionCandidates[j]
			slot, ok := slotByType[candidate.ActionType]
			if !ok {
				candidate.BlockedReason = reasonUnknownActionType
				continue
			}
			applyCandidatePolicy(candidate, decisions[slot])
		}
		cases.ApplyProviderApplicability(&all[i], provider)
	}
}

// distinctCaseContracts returns the contract of each known action type, in
// first-seen order, and the slot of each type in that list.
func distinctCaseContracts(all []cases.Case) ([]executor.ActionContract, map[string]int) {
	var contracts []executor.ActionContract
	slotByType := map[string]int{}
	for i := range all {
		for _, candidate := range all[i].ActionCandidates {
			if _, seen := slotByType[candidate.ActionType]; seen {
				continue
			}
			contract, ok := executor.ContractForActionType(candidate.ActionType)
			if !ok {
				continue
			}
			slotByType[candidate.ActionType] = len(contracts)
			contracts = append(contracts, contract)
		}
	}
	return contracts, slotByType
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
