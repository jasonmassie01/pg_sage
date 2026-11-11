package rca

import (
	"testing"
	"time"
)

// rca-07 review: one drain can carry the first signal of a new window
// together with the pending follow-up of the window it replaced (the
// classifier queues that follow-up when the new window opens). Both
// windows must count, in either order, and later reports of either
// window add only their growth.
func TestMergeLogOccurrences_BatchSpanningTwoWindows(t *testing.T) {
	t2 := occT0.Add(30 * time.Minute)
	w1First := occSignal(1, []int{101}, occT0, occT0)
	w1Up := occSignal(3, []int{101, 102}, occT0, occT0.Add(29*time.Minute))
	w2First := occSignal(1, []int{103}, t2, t2)
	for name, batch := range map[string][]*Signal{
		"new window first":    {w2First, w1Up},
		"closed window first": {w1Up, w2First},
	} {
		eng := newTestEngine()
		eng.mu.Lock()
		inc := eng.runLogDecisionTrees([]*Signal{w1First})[0]
		eng.dedup(&inc)
		next := eng.runLogDecisionTrees(batch)[0]
		eng.dedup(&next)
		got := eng.incidents[0].LogOccurrences
		eng.mu.Unlock()
		if got == nil || got.Count != 4 {
			t.Fatalf("%s: count = %+v, want 4 (3 in the first window, 1 in the second)",
				name, got)
		}
		w2Up := logOccurrencesFromSignal(occSignal(5, []int{103}, t2, t2.Add(time.Minute)))
		if m := mergeLogOccurrences(got, w2Up); m.Count != 8 {
			t.Errorf("%s: after the second window's follow-up count = %d, want 8",
				name, m.Count)
		}
		if m := mergeLogOccurrences(got, logOccurrencesFromSignal(w1Up)); m.Count != 4 {
			t.Errorf("%s: re-reporting the first window counted it again: %d", name, m.Count)
		}
	}
}

// Boundary: the windows an incident remembers are bounded; a report of a
// window older than the ones kept is counted as new, never panics.
func TestMergeLogOccurrences_WindowMemoryBounded(t *testing.T) {
	var occ *LogOccurrences
	for i := 0; i < 10; i++ {
		at := occT0.Add(time.Duration(i) * time.Hour)
		occ = mergeLogOccurrences(occ, logOccurrencesFromSignal(occSignal(2, []int{i + 1}, at, at)))
	}
	if occ.Count != 20 {
		t.Fatalf("ten windows of 2 = %d, want 20", occ.Count)
	}
	if len(occ.windows) != maxLogOccurrenceWindows {
		t.Errorf("windows kept = %d, want %d", len(occ.windows), maxLogOccurrenceWindows)
	}
	last := occT0.Add(9 * time.Hour)
	if m := mergeLogOccurrences(occ, logOccurrencesFromSignal(
		occSignal(3, []int{10}, last, last))); m.Count != 21 {
		t.Errorf("growth of the newest window = %d, want 21", m.Count)
	}
}
