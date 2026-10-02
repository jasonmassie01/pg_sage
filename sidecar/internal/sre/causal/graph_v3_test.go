package causal

import (
	"strings"
	"testing"
)

// Graph v3 (M6) adds the runway families. An XID consumption surge only
// shortens a runway another mechanism holds back, so it amplifies every
// other wraparound mechanism; database growth and the sequence limits
// amplify nothing.
func TestGraphV3_RunwayAmplification(t *testing.T) {
	if GraphVersion != "causal-v4" {
		t.Fatalf("graph version = %q, want causal-v4 (runway nodes added)", GraphVersion)
	}
	surge, ok := NodeByID(XIDConsumptionSurge)
	if !ok {
		t.Fatal("no XID consumption surge node")
	}
	want := map[NodeID]bool{XminHeldBySession: true, XminHeldByPreparedXact: true,
		XminHeldByReplication: true, AutovacuumSaturated: true, AutovacuumDisabled: true,
		AutovacuumCancelled: true}
	if len(surge.Amplifies) != len(want) {
		t.Fatalf("surge amplifies %v", surge.Amplifies)
	}
	for _, a := range surge.Amplifies {
		if !want[a] {
			t.Fatalf("surge amplifies %s", a)
		}
	}
	for _, id := range []NodeID{DatabaseGrowth, SequenceTypeLimit,
		ColumnNarrowerThanSequence, ExplicitMaxvalueLimit, XminHeldBySession} {
		n, ok := NodeByID(id)
		if !ok || len(n.Amplifies) != 0 {
			t.Fatalf("%s = %+v (%v), want a node that amplifies nothing", id, n, ok)
		}
	}
}

// The sequence fixes are manual and reviewed: changing a column type is
// a table rewrite pg_sage never executes.
func TestGraphV3_SequenceStepsAreReviewedManualChanges(t *testing.T) {
	for _, id := range []NodeID{SequenceTypeLimit, ColumnNarrowerThanSequence} {
		n, _ := NodeByID(id)
		if !strings.Contains(n.OperatorStep, "rewrite") ||
			!strings.Contains(n.OperatorStep, "reviewed") {
			t.Errorf("%s step %q must say it is a reviewed table rewrite", id, n.OperatorStep)
		}
	}
}
