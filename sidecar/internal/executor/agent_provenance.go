package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// withAgentProvenance stamps a queue item an agent's request created
// (§6.2.2, §7): its principal, and proposed_via agent unless an origin is
// already recorded.
func withAgentProvenance(ctx context.Context,
	meta store.ActionProposalMetadata) store.ActionProposalMetadata {
	ref, ok := policy.PrincipalRefFromContext(ctx)
	if !ok || ref.ID == "" {
		return meta
	}
	meta.PrincipalID = ref.ID
	if meta.ProposedVia == "" {
		meta.ProposedVia, meta.ProposedBy = "agent", ref.ID
	}
	return meta
}

// withQueuedPrincipal binds an approved item's agent, so its run is that
// agent's request: the gate applies the D-steps and the principal hold,
// and the action keeps its provenance.
func withQueuedPrincipal(ctx context.Context, a store.QueuedAction) context.Context {
	if a.PrincipalID == "" {
		return ctx
	}
	return policy.WithPrincipalRef(ctx, policy.PrincipalRef{ID: a.PrincipalID})
}
