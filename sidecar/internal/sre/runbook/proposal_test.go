package runbook

import (
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// A runbook ends in a proposal: the graph node's operator step, a typed
// action for the policy gate to judge later, or an escalation. Nothing a
// runbook proposes is executed by the runbook.

func TestActionTypes_SortedUniqueAndIncidentScoped(t *testing.T) {
	types := ActionTypes()
	if len(types) == 0 || !sort.StringsAreSorted(types) {
		t.Fatalf("ActionTypes = %v, want a sorted, non-empty list", types)
	}
	seen := map[string]bool{}
	for _, a := range types {
		if seen[a] {
			t.Fatalf("duplicate action type %s", a)
		}
		seen[a] = true
	}
	for _, want := range []string{"cancel_backend", "terminate_backend",
		"diagnose_lock_blockers"} {
		if !seen[want] {
			t.Errorf("ActionTypes lacks %s", want)
		}
	}
	for _, never := range []string{"alter_system_guc", "retention_delete",
		"reindex_concurrently"} {
		if seen[never] {
			t.Errorf("ActionTypes offers %s, which no incident runbook may propose", never)
		}
	}
	types[0] = "mutated"
	if ActionTypes()[0] == "mutated" {
		t.Fatal("ActionTypes returns its internal slice")
	}
}

func TestProposalText(t *testing.T) {
	n, _ := causal.NodeByID(causal.IdleInTxHolder)
	step := Proposal{Kind: ProposalOperatorStep, Node: string(causal.IdleInTxHolder)}
	if got := step.Text(); got != n.OperatorStep {
		t.Fatalf("operator step text = %q, want the node's step %q", got, n.OperatorStep)
	}
	action := Proposal{Kind: ProposalAction, ActionType: "cancel_backend",
		Node: string(causal.IdleInTxHolder)}
	text := action.Text()
	for _, want := range []string{"cancel_backend", "idle_in_tx_holder", "not executed",
		"policy gate"} {
		if !strings.Contains(text, want) {
			t.Errorf("action text %q lacks %q", text, want)
		}
	}
	esc := Proposal{Kind: ProposalEscalate}
	if !strings.Contains(strings.ToLower(esc.Text()), "escalate") {
		t.Errorf("escalation text %q does not say escalate", esc.Text())
	}
	if (Proposal{Kind: "other"}).Text() != "" {
		t.Error("an unknown proposal kind must have no text")
	}
}
