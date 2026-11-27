package executor

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// AutonomyCeiling is this executor's gate inputs as the Autonomy page's
// sentence reads them (ux-06b): the same runtime snapshot the standing
// gate authorizes against, so a lowered trust level or ceiling shows on
// the next read. A nil executor, or one without a standing gate, is the
// zero ceiling, which watches only. The policy set's per-class trust
// ceiling is left out: the gate does not enforce it yet.
func (e *Executor) AutonomyCeiling(ctx context.Context, isReplica bool) earned.Ceiling {
	if e == nil {
		return earned.Ceiling{}
	}
	state := e.standingRuntimeState(ctx, policy.ActionRequest{IsReplica: isReplica})
	return earned.Ceiling{
		GateAttached: e.StandingPolicyGate() != nil, ExecutorEnabled: state.ExecutorEnabled,
		EmergencyStop: state.EmergencyStop, Replica: state.IsReplica,
		TrustLevel: state.TrustLevel, ExecutionMode: state.ExecutionMode,
		Tier3Safe: state.Tier3Safe, Tier3Moderate: state.Tier3Moderate,
		WindowConfigured:      state.WindowConfigured,
		SQLValidationDegraded: state.SQLValidationDegraded,
		LedgerEnforced:        e.autonomyLimiter() != nil,
		RampStart:             state.RampStart, SafeRampAge: state.SafeRampAge,
		ModerateRampAge: state.ModerateRampAge, Now: time.Now(),
		Contracts: AutonomyClassContracts(),
	}
}

// classContracts is computed once from the contract registry.
var classContracts = buildClassContracts()

// AutonomyClassContracts is, per ledger class, what the gate reads of
// its action types' contracts: the highest risk tier, the least
// reversible rollback class and whether any needs approval. A class with
// no action type has no entry (it watches only). The map is a copy.
func AutonomyClassContracts() map[earned.ActionClass]earned.ClassContract {
	return maps.Clone(classContracts)
}

func buildClassContracts() map[earned.ActionClass]earned.ClassContract {
	out := map[earned.ActionClass]earned.ClassContract{}
	for _, spec := range earned.Classes() {
		var merged earned.ClassContract
		found := false
		for _, actionType := range spec.ActionTypes {
			contract, ok := ContractForActionType(actionType)
			if !ok {
				continue
			}
			merged = mergeClassContract(merged, found, contract)
			found = true
		}
		if found {
			out[spec.Class] = merged
		}
	}
	return out
}

func mergeClassContract(into earned.ClassContract, seen bool,
	contract ActionContract) earned.ClassContract {
	pc := policyContract(contract)
	next := earned.ClassContract{RiskTier: pc.RiskTier, Rollback: pc.RollbackClass,
		ChangeClass:      policy.ChangeClass(changeClassForActionType(contract.ActionType)),
		ApprovalRequired: len(pc.Guardrails) > 0 || isBackendSignalType(contract.ActionType)}
	if !seen {
		return next
	}
	if riskRank(next.RiskTier) > riskRank(into.RiskTier) {
		into.RiskTier = next.RiskTier
	}
	if earned.RollbackCap(next.Rollback) < earned.RollbackCap(into.Rollback) {
		into.Rollback = next.Rollback
	}
	into.ApprovalRequired = into.ApprovalRequired || next.ApprovalRequired
	if into.ChangeClass == "" {
		into.ChangeClass = next.ChangeClass
	}
	return into
}

// riskRank orders tiers; an unknown tier ranks as high (fail closed).
func riskRank(tier policy.RiskTier) int {
	switch tier {
	case policy.RiskReadOnly:
		return 0
	case policy.RiskSafe:
		return 1
	case policy.RiskModerate:
		return 2
	}
	return 3
}

// isBackendSignalType mirrors the gate: a session cancel or terminate
// always needs a human.
func isBackendSignalType(actionType string) bool {
	switch strings.TrimSpace(actionType) {
	case "cancel_backend", "terminate_backend":
		return true
	}
	return false
}
