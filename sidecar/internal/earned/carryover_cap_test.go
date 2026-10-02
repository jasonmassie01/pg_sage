package earned

import "testing"

// Post-test audit: no carry-over is defined for an irreversible class, so
// the seeding tests could not show that a carried row for one (written
// behind the service's back) is still held at L1 by the gate's cap.
func TestCarriedCapNeverLiftsAnIrreversibleClass(t *testing.T) {
	carried := State{Provenance: ProvenanceCarriedOver}
	for _, spec := range Classes() {
		got := effectiveCap(carried, spec.Class)
		if spec.Reversibility == Irreversible && got != L1 {
			t.Errorf("carried %s capped at %v, want L1", spec.Class, got)
		}
		if got > L3 {
			t.Errorf("carried %s capped at %v, above L3", spec.Class, got)
		}
	}
	if effectiveCap(carried, ActionClass("not_a_class")) != L1 {
		t.Error("an unknown class carried above L1")
	}
	if effectiveCap(carried, ClassWALBound) != L3 ||
		effectiveCap(State{Provenance: ProvenanceLedger}, ClassWALBound) != L2 {
		t.Error("wal_bound caps: carried L3, earned L2")
	}
}
