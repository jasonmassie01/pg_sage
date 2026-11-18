package trigger

import "testing"

// review-04b adds the fleet_outlier kind: the fleet triage schedules a
// member's deep review through the same queue, as an event.
// No concurrent access tests: kinds are constants.

func TestKindFleetOutlier_IsAValidEvent(t *testing.T) {
	if KindFleetOutlier != "fleet_outlier" || !KindFleetOutlier.Valid() ||
		!KindFleetOutlier.Event() {
		t.Fatalf("fleet_outlier: valid %v event %v", KindFleetOutlier.Valid(),
			KindFleetOutlier.Event())
	}
	got := Kinds()
	if len(got) != 8 || got[7] != KindFleetOutlier {
		t.Fatalf("Kinds() = %v, want the seven of review-04a then fleet_outlier", got)
	}
}
