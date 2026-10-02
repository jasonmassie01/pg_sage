package sre

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// M7 (coordinator, 2026-10-02): a runbook's typed action proposal names
// the incident family and the earned-autonomy class it would be judged
// under, so a request for it carries IncidentFamily and its outcome feeds
// that family's ledger. Operator steps and escalations name neither.

func TestRunbookActionProposalNamesFamilyAndAutonomyClass(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	d := idleRunbook()
	d.Nodes[1].When = &runbook.Predicate{Op: runbook.OpHypothesis,
		Node: string(causal.PreparedXactHolder), In: []string{"root_cause"}}
	d.Nodes[3].Proposal = &runbook.Proposal{Kind: runbook.ProposalAction,
		ActionType: "cancel_backend", Node: string(causal.IdleInTxHolder)}
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
		signedRunbook(t, ctx, st, s, d)
	})
	p := inv.Summary.Runbook.Proposal
	if p == nil || p.Family != "lock_blocking" || p.AutonomyClass != "backend_cancel" {
		t.Fatalf("runbook action proposal = %+v, want lock_blocking/backend_cancel", p)
	}
}

func TestRunbookOperatorStepNamesNoAutonomyClass(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
		signedRunbook(t, ctx, st, s, idleRunbook())
	})
	run := inv.Summary.Runbook
	if run == nil || run.Proposal == nil || run.Proposal.Kind != "operator_step" ||
		run.Proposal.AutonomyClass != "" || run.Proposal.Family != "" {
		t.Fatalf("operator step proposal = %+v", run)
	}
}
