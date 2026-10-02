package executor

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// proposeForApproval queues f for an operator, pinned to the exact
// recommendation revision when there is one (C04).
func (e *Executor) proposeForApproval(
	ctx context.Context,
	findingID int,
	f analyzer.Finding,
	cand *recommendation.Candidate,
) (int, error) {
	if proposer, ok := e.actionStore.(ActionMetadataProposer); ok {
		meta := e.buildApprovalProposalMetadata(f, time.Now().UTC())
		if cand != nil {
			meta.RecommendationID, meta.RecommendationRevision = cand.ID, cand.Revision
			meta.ContentHash = cand.ContentHash
		}
		return proposer.ProposeWithMetadata(
			ctx, nil, findingID,
			f.RecommendedSQL, f.RollbackSQL, f.ActionRisk, meta,
		)
	}
	return e.actionStore.Propose(
		ctx, nil, findingID,
		f.RecommendedSQL, f.RollbackSQL, f.ActionRisk,
	)
}

func (e *Executor) buildApprovalProposalMetadata(
	f analyzer.Finding,
	now time.Time,
) store.ActionProposalMetadata {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	actionType := actionTypeForProposalSQL(f.RecommendedSQL)
	metadata := store.ActionProposalMetadata{
		ActionType:         actionType,
		IdentityKey:        actionIdentityKey(f, actionType),
		PolicyDecision:     PolicyDecisionQueueApproval,
		VerificationStatus: "not_started",
		ShadowToilMinutes:  estimatedToilForActionType(actionType),
	}
	expiresAt := now.Add(24 * time.Hour)
	metadata.ExpiresAt = &expiresAt
	if contract, ok := ContractForActionType(actionType); ok {
		decision := e.ExplainAction(context.Background(), contract,
			f.RecommendedSQL, strings.TrimSpace(f.ObjectIdentifier))
		metadata.PolicyDecision = decision.Decision
		metadata.Guardrails = decision.Guardrails
	}
	return metadata
}

func actionIdentityKey(f analyzer.Finding, actionType string) string {
	parts := []string{f.Category, f.ObjectIdentifier, actionType}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ":")
}

func estimatedToilForActionType(actionType string) int {
	if actionType == "analyze_table" {
		return 15
	}
	return 30
}
