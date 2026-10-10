package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// An approved queue item an agent's request queued runs as that agent's
// request (§6.2.2): RunApprovedAction binds the item's principal, so the
// gate applies the D-steps and the principal hold, and the action keeps
// its provenance. pg_sage's own items bind none.

type principalRunner struct {
	ref   policy.PrincipalRef
	bound bool
}

func (r *principalRunner) Owns(store.QueuedAction) bool { return true }

func (r *principalRunner) RunApproved(ctx context.Context, _ store.QueuedAction,
	_ int) (ApprovedRun, error) {
	r.ref, r.bound = policy.PrincipalRefFromContext(ctx)
	return ApprovedRun{ActionLogID: 1}, nil
}

func TestRunApprovedActionBindsTheItemsPrincipal(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	r := &principalRunner{}
	exec.SetApprovedActionRunner(r)
	item := queuedSRE()
	item.PrincipalID = "agp_aaaaaaaaaaaaaaaaaaaa"
	if _, err := exec.RunApprovedAction(context.Background(), item, 12); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !r.bound || r.ref.ID != item.PrincipalID {
		t.Fatalf("principal not bound: %+v %v", r.ref, r.bound)
	}
	r = &principalRunner{}
	exec.SetApprovedActionRunner(r)
	if _, err := exec.RunApprovedAction(context.Background(), queuedSRE(), 12); err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.bound {
		t.Fatalf("pg_sage's own item bound a principal: %+v", r.ref)
	}
}

// An agent-bound context queues its proposal with the principal and
// proposed_via agent.
func TestProposalMetadataCarriesTheContextPrincipal(t *testing.T) {
	ctx := policy.WithPrincipalRef(context.Background(),
		policy.PrincipalRef{ID: "agp_bbbbbbbbbbbbbbbbbbbb"})
	meta := withAgentProvenance(ctx, store.ActionProposalMetadata{ActionType: "x"})
	if meta.PrincipalID != "agp_bbbbbbbbbbbbbbbbbbbb" || meta.ProposedVia != "agent" ||
		meta.ProposedBy != "agp_bbbbbbbbbbbbbbbbbbbb" || meta.ActionType != "x" {
		t.Fatalf("meta %+v", meta)
	}
	own := withAgentProvenance(context.Background(), store.ActionProposalMetadata{
		ProposedVia: "ask_sage", ProposedBy: "a@b"})
	if own.PrincipalID != "" || own.ProposedVia != "ask_sage" {
		t.Fatalf("own %+v", own)
	}
}
