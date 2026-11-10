package executor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// vacuum-05: the watchdog's pure decisions — when a VACUUM is stalled, and
// what a running vacuum on the target means for admission.

// No concurrent access tests: stallTracker belongs to one watchdog
// goroutine, and vacuumAdmission is a pure function of its rows.

func heapProgress(scanned int64) vacuumProgress {
	return vacuumProgress{Phase: "scanning heap", HeapBlksTotal: 100, HeapBlksScanned: scanned}
}

func TestStallTrackerHeapPhaseStallsAtExactlyTheStallWindow(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tracker := stallTracker{stall: 15 * time.Minute}
	if tracker.observe(heapProgress(10), start) {
		t.Fatal("first sample reported a stall")
	}
	if tracker.observe(heapProgress(10), start.Add(15*time.Minute-time.Nanosecond)) {
		t.Fatal("unchanged for one nanosecond under the window reported a stall")
	}
	if !tracker.observe(heapProgress(10), start.Add(15*time.Minute)) {
		t.Fatal("unchanged for exactly the window did not report a stall")
	}
}

func TestStallTrackerAnyCounterChangeResetsTheClock(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	five := int64(5)
	six := int64(6)
	changes := []func(*vacuumProgress){
		func(p *vacuumProgress) { p.HeapBlksScanned++ },
		func(p *vacuumProgress) { p.HeapBlksVacuumed++ },
		func(p *vacuumProgress) { p.IndexVacuumCount++ },
		func(p *vacuumProgress) { p.Phase = "vacuuming heap" },
		func(p *vacuumProgress) { p.IndexesProcessed = &six },
	}
	for i, change := range changes {
		tracker := stallTracker{stall: time.Minute}
		base := heapProgress(10)
		base.IndexesProcessed = &five
		tracker.observe(base, start)
		next := base
		change(&next)
		if tracker.observe(next, start.Add(59*time.Second)) {
			t.Fatalf("change %d: reported a stall on a changed tuple", i)
		}
		if tracker.observe(next, start.Add(90*time.Second)) {
			t.Fatalf("change %d: the clock did not restart at the change", i)
		}
		if !tracker.observe(next, start.Add(119*time.Second)) {
			t.Fatalf("change %d: unchanged for a full window since the change, no stall", i)
		}
	}
}

func TestStallTrackerIndexPhaseNeverStalls(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, phase := range []string{"vacuuming indexes", "cleaning up indexes",
		"initializing", "truncating heap", "performing final cleanup"} {
		tracker := stallTracker{stall: time.Minute}
		p := vacuumProgress{Phase: phase, HeapBlksTotal: 100, HeapBlksScanned: 100}
		tracker.observe(p, start)
		if tracker.observe(p, start.Add(time.Hour)) {
			t.Fatalf("phase %q: unchanged for an hour reported a stall; only the cap "+
				"bounds a phase without a heap counter", phase)
		}
	}
}

func TestStallTrackerResetForgetsTheLastSample(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tracker := stallTracker{stall: time.Minute}
	tracker.observe(heapProgress(10), start)
	tracker.reset()
	if tracker.observe(heapProgress(10), start.Add(2*time.Minute)) {
		t.Fatal("first sample after a reset reported a stall")
	}
}

func TestStallTrackerZeroWindowNeverStalls(t *testing.T) {
	// A zero window is never configured (VacuumStall defaults it); the
	// tracker must still not cancel on the first repeated sample.
	tracker := stallTracker{}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tracker.observe(heapProgress(1), now)
	if tracker.observe(heapProgress(1), now.Add(time.Hour)) {
		t.Fatal("zero stall window reported a stall")
	}
}

func TestVacuumAdmissionEmptyViewProceeds(t *testing.T) {
	for name, rows := range map[string][]runningVacuum{"nil": nil, "empty": {}} {
		verdict, running := vacuumAdmission(rows)
		if verdict != "" || running != nil {
			t.Fatalf("%s rows: verdict %q running %+v, want proceed", name, verdict, running)
		}
	}
}

func TestVacuumAdmissionVisibleVacuumWithholds(t *testing.T) {
	rows := []runningVacuum{
		{PID: 7, RelIDKnown: false},
		{PID: 42, RelIDKnown: true, Phase: "scanning heap", HeapBlksScanned: 120,
			HeapBlksTotal: 1000, Autovacuum: true, Wraparound: true},
	}
	verdict, running := vacuumAdmission(rows)
	if verdict != reasonVacuumInProgress || running == nil || running.PID != 42 {
		t.Fatalf("verdict %q running %+v, want vacuum_in_progress for pid 42",
			verdict, running)
	}
	detail := running.detail("public.orders")
	for _, want := range []string{"pid 42", "public.orders", "scanning heap", "120/1000",
		"to prevent wraparound", "autovacuum"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	evidence := running.evidence()
	if evidence["pid"] != 42 || evidence["phase"] != "scanning heap" ||
		evidence["heap_blks_scanned"] != int64(120) ||
		evidence["heap_blks_total"] != int64(1000) || evidence["is_wraparound"] != true {
		t.Fatalf("evidence = %v", evidence)
	}
}

func TestVacuumAdmissionUnknownRelIDProceedsAsProgressUnknown(t *testing.T) {
	verdict, running := vacuumAdmission([]runningVacuum{{PID: 9, RelIDKnown: false}})
	if verdict != vacuumProgressUnknown || running == nil || running.PID != 9 {
		t.Fatalf("verdict %q running %+v, want progress_unknown for pid 9", verdict, running)
	}
}

func TestVacuumWatchErrorNamesOutcomeAndWrapsCauses(t *testing.T) {
	execErr := errors.New("canceling statement due to user request")
	cancelErr := errors.New("permission denied to cancel")
	err := &VacuumWatchError{Outcome: VacuumStalled, PID: 77,
		Progress: heapProgress(10), Err: execErr, CancelErr: cancelErr}
	text := err.Error()
	for _, want := range []string{"vacuum_stalled", "77", "scanning heap", "10/100",
		"permission denied to cancel", "canceling statement"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q lacks %q", text, want)
		}
	}
	if !errors.Is(err, execErr) || !errors.Is(err, cancelErr) {
		t.Fatalf("error does not wrap its causes: %v", err)
	}
	if errors.Is(err, ErrEmergencyStop) {
		t.Fatal("a stall is not an emergency stop")
	}
	stop := &VacuumWatchError{Outcome: VacuumEmergencyStop, PID: 77, Err: execErr}
	if !errors.Is(stop, ErrEmergencyStop) {
		t.Fatal("an emergency_stop outcome must match ErrEmergencyStop")
	}
}
