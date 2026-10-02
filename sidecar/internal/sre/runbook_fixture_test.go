package sre

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Fixtures for typed runbooks (Sage SRE M6): a lock runbook that reads
// the lock chains and, when the graph's root cause is the idle holder,
// proposes its operator step; otherwise it escalates.

func idleRunbook() runbook.Definition {
	return runbook.Definition{Name: "Idle holder",
		Trigger: runbook.Trigger{Kinds: []string{string(TriggerLock)},
			Nodes: []string{"idle_in_tx_holder"}},
		Start: "read_chains",
		Nodes: []runbook.Node{
			{ID: "read_chains", Type: runbook.NodeProbe, Probe: "lock_chains",
				Next: "is_idle"},
			{ID: "is_idle", Type: runbook.NodeDecision, When: &runbook.Predicate{
				Op: runbook.OpHypothesis, Node: "idle_in_tx_holder",
				In: []string{"root_cause"}}, Then: "end_tx", Else: "escalate"},
			{ID: "end_tx", Type: runbook.NodeProposal, Proposal: &runbook.Proposal{
				Kind: runbook.ProposalOperatorStep, Node: "idle_in_tx_holder"}},
			{ID: "escalate", Type: runbook.NodeProposal, Proposal: &runbook.Proposal{
				Kind: runbook.ProposalEscalate}},
		}}
}

func hashOf(t *testing.T, d runbook.Definition) string {
	t.Helper()
	h, err := d.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

func draftOf(d runbook.Definition) RunbookDraft {
	return RunbookDraft{Definition: d, Actor: "user:2"}
}

// signedRunbook stores d and has an admin sign version 1.
func signedRunbook(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	d runbook.Definition) Runbook {
	t.Helper()
	rb, err := st.CreateRunbook(ctx, scope, draftOf(d))
	if err != nil {
		t.Fatalf("create runbook: %v", err)
	}
	rb, err = st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 1,
		ContentHash: hashOf(t, d), Signer: "user:1", Role: "admin"})
	if err != nil {
		t.Fatalf("sign runbook: %v", err)
	}
	return rb
}
