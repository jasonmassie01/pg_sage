package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// recordOperatorDecision writes the ledger decision for an operator-run
// action (manual "take action" or an approved queue item) before the action
// executes, so the action is auditable and can be verified and credited
// like autonomous work. A ledger failure is logged and leaves the action
// without a decision; the self-audit then reports it.
func (e *Executor) recordOperatorDecision(
	ctx context.Context, sql string, findingID int, approvedBy *int,
) int64 {
	if e.pool == nil {
		return 0
	}
	actionType := actionTypeForProposalSQL(sql)
	risk := "high"
	if contract, ok := ContractForActionType(actionType); ok {
		risk = contract.BaseRiskTier
	}
	feature := changeClassForActionType(actionType)
	if feature == "" {
		feature = "operator"
	}
	evidence := map[string]any{"finding_id": findingID, "source": "operator"}
	if approvedBy != nil {
		evidence["approved_by"] = *approvedBy
	}
	decision, err := ledger.NewService(ledger.NewPostgresRepository(e.pool)).RecordDecision(
		ctx, ledger.DecisionInput{
			DatabaseID: e.databaseID, Feature: feature, Intent: operatorDecisionIntent,
			Evidence: evidence, ProposedSQL: sql, Verdict: ledger.VerdictExecute,
			Reason: operatorDecisionIntent, RiskTier: risk,
			PolicyVersion: e.currentPolicyVersion(ctx),
		})
	if err != nil {
		e.logFn("executor", "record operator decision for finding %d: %v", findingID, err)
		return 0
	}
	return decision.ID
}

func (e *Executor) currentPolicyVersion(ctx context.Context) int {
	current, err := policy.NewStore(e.pool).Current(
		ctx, policy.Scope{DatabaseID: int64Pointer(e.databaseID)})
	if err != nil || current.Version <= 0 {
		return 1
	}
	return int(current.Version)
}

// databaseIDValue is the canonical database identity stamped on every
// action_log row (nil outside meta-db mode, where the pool is the identity).
func (e *Executor) databaseIDValue() *int64 {
	return int64Pointer(e.databaseID)
}
