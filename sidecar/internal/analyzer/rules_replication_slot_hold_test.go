package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// No concurrent-access tests: runSnapshotRules runs on the analyzer's own
// goroutine; the pending flag lives in that analyzer's RuleExtras.

// slotHoldAnalyzer is an analyzer whose rule clock the test moves.
func slotHoldAnalyzer(clk *slotTestClock) *Analyzer {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.extras.Now = func() time.Time { return clk.now }
	return a
}

// slotCycle runs one cycle's snapshot rules and reports whether open
// inactive_slot findings may resolve, and how many the cycle emitted.
func slotCycle(a *Analyzer, snap *collector.Snapshot) (bool, int) {
	a.eval = newCycleEval()
	out := a.runSnapshotRules(snap, nil, false)
	n := 0
	for _, f := range out {
		if f.Category == "inactive_slot" {
			n++
		}
	}
	return a.eval.resolvable(out)["inactive_slot"], n
}

// rules-09: before PG17 the inactive clock lives in memory. After a
// restart a slot that was already reported is below the threshold on the
// new clock; its absence is not proof of clearance, so the open finding
// must stay open (not resolve and reopen as a new row) until the clock
// decides.
func TestInactiveSlot_RestartedClockHoldsResolution(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	a := slotHoldAnalyzer(clk)
	idle := slotsSnap(deadSlot("cdc"))
	if ok, n := slotCycle(a, idle); ok || n != 0 {
		t.Fatalf("first sighting: resolvable=%v findings=%d, want held and none", ok, n)
	}
	clk.now = slotT0.Add(4 * time.Minute)
	if ok, n := slotCycle(a, idle); ok || n != 0 {
		t.Fatalf("at +4m: resolvable=%v findings=%d, want held and none", ok, n)
	}
	clk.now = slotT0.Add(6 * time.Minute)
	if ok, n := slotCycle(a, idle); !ok || n != 1 {
		t.Fatalf("at +6m: resolvable=%v findings=%d, want resolvable and one", ok, n)
	}
	live := deadSlot("cdc")
	live.Active = true
	clk.now = slotT0.Add(7 * time.Minute)
	if ok, n := slotCycle(a, slotsSnap(live)); !ok || n != 0 {
		t.Fatalf("slot active again: resolvable=%v findings=%d, want resolvable", ok, n)
	}
}

// The server's inactive_since (PG17+) is the truth: a slot it shows
// inactive for less than the threshold resolves its finding at once.
func TestInactiveSlot_InactiveSinceBelowThresholdResolves(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	a := slotHoldAnalyzer(clk)
	s := deadSlot("cdc")
	s.InactiveSince = timePtr(slotT0.Add(-time.Minute))
	if ok, n := slotCycle(a, slotsSnap(s)); !ok || n != 0 {
		t.Fatalf("inactive_since 1m ago: resolvable=%v findings=%d, want resolvable", ok, n)
	}
}

// Skipped slots (temporary, synced on a standby) never hold resolution.
func TestInactiveSlot_SkippedSlotsDoNotHold(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	a := slotHoldAnalyzer(clk)
	tmp := deadSlot("tmp")
	tmp.Temporary = true
	synced := deadSlot("failover")
	synced.Synced = true
	snap := slotsSnap(tmp, synced)
	snap.System.IsReplica = true
	if ok, n := slotCycle(a, snap); !ok || n != 0 {
		t.Fatalf("skipped slots: resolvable=%v findings=%d, want resolvable", ok, n)
	}
}
