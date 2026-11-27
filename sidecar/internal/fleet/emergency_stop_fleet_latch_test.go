package fleet

import "testing"

// FleetStopped (review-04b) reports the fleet-wide stop latch: a stop of
// all databases sets it, a stop of one does not, and a resume of all
// lifts it. The fleet review tier halts its scheduling while it holds.
// No concurrent access tests here: the latch is read under the manager
// lock, which the stop and resume tests exercise.

func TestFleetStopped_FollowsTheFleetWideLatch(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	if h.mgr.FleetStopped() {
		t.Fatal("a fresh fleet is stopped")
	}
	if _, err := h.mgr.EmergencyStopStrict("alpha", "op"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	if h.mgr.FleetStopped() {
		t.Fatal("stopping one database set the fleet-wide latch")
	}
	if _, err := h.mgr.EmergencyStopStrict("", "op"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if !h.mgr.FleetStopped() {
		t.Fatal("a stop of all databases did not set the latch")
	}
	if _, err := h.mgr.ResumeStrict("", "op"); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	if h.mgr.FleetStopped() {
		t.Fatal("a resume of all databases left the latch")
	}
	var nilMgr *DatabaseManager
	if nilMgr.FleetStopped() {
		t.Fatal("a nil manager is stopped")
	}
}
