package causal

import (
	"strings"
	"testing"
)

// CHECK-37: every hypothesis the graph can produce names a refutation
// probe or none_available, and an operator step (a manual next step,
// never an executed action).
func TestGraph_EveryNodeHasRefutationAndOperatorStep(t *testing.T) {
	for _, n := range Graph() {
		if n.Refutation == "" {
			t.Errorf("%s has no refutation probe (use %s)", n.ID, NoRefutation)
		}
		if strings.TrimSpace(n.OperatorStep) == "" {
			t.Errorf("%s has no operator step", n.ID)
		}
		h := newHypothesis(n.ID, "subject")
		if h.RefutationProbe != n.Refutation || h.OperatorStep != n.OperatorStep {
			t.Errorf("%s hypothesis does not carry the node's refutation and step", n.ID)
		}
	}
}

// CHECK-02: the idle-in-transaction holder is never presented as fixed by
// cancelling it: pg_cancel_backend has no running query to cancel.
func TestGraph_IdleHolderStepDoesNotOfferCancellation(t *testing.T) {
	n, _ := NodeByID(IdleInTxHolder)
	if !strings.Contains(n.OperatorStep,
		"pg_cancel_backend does not end an idle transaction") {
		t.Fatalf("idle holder step %q must say cancellation does not end it", n.OperatorStep)
	}
}

// CHECK-05: slot retention and write surge are separate mechanisms, and
// a write surge only amplifies slot retention.
func TestGraph_WriteSurgeOnlyAmplifiesSlotRetention(t *testing.T) {
	surge, _ := NodeByID(WriteSurge)
	want := map[NodeID]bool{InactiveSlot: true, SlowConsumer: true}
	if len(surge.Amplifies) != len(want) {
		t.Fatalf("write surge amplifies %v", surge.Amplifies)
	}
	for _, a := range surge.Amplifies {
		if !want[a] {
			t.Fatalf("write surge amplifies %s", a)
		}
	}
}
