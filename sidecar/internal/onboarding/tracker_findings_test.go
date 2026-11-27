package onboarding

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Invalid input for the metric: a first look of setup notes only (items,
// no findings) records its items but not a first finding. Fails before
// firstlook-07, when any item set the time to first finding.
func TestTrackerSetupOnlyFirstLookLeavesTTFFUnset(t *testing.T) {
	tr := NewTracker()
	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tr.Start("app", t0)
	if _, recorded := tr.FirstLook("app", t0.Add(2*time.Second), 4, 0,
		time.Second); recorded {
		t.Fatal("a first look with only setup notes recorded a first finding")
	}
	m := tr.Snapshot()[0]
	if m.HasTTFF || m.TTFF != 0 || !m.FirstLookDone || m.FirstLookItems != 4 ||
		m.FirstLookFindings != 0 || m.FirstLookDuration != time.Second {
		t.Fatalf("metrics = %+v, want 4 items, 0 findings and no ttff", m)
	}
}

func TestTrackerFirstLookFindingSetsTTFF(t *testing.T) {
	tr := NewTracker()
	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tr.Start("app", t0)
	ttff, recorded := tr.FirstLook("app", t0.Add(7*time.Second), 5, 1, time.Second)
	if !recorded || ttff != 7*time.Second {
		t.Fatalf("ttff = %s recorded=%v, want 7s", ttff, recorded)
	}
	m := tr.Snapshot()[0]
	if !m.HasTTFF || m.TTFF != 7*time.Second || m.FirstLookItems != 5 ||
		m.FirstLookFindings != 1 {
		t.Fatalf("metrics = %+v, want 5 items, 1 finding and a 7s ttff", m)
	}
}

// State transition: no qualifying finding in the first look, then the
// analyzer's first finding sets the time to first finding.
func TestTrackerSetupOnlyThenAnalyzerFinding(t *testing.T) {
	tr := NewTracker()
	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	tr.Start("app", t0)
	tr.FirstLook("app", t0.Add(time.Second), 3, 0, time.Second)
	ttff, ok := tr.FirstFinding("app", t0.Add(90*time.Second))
	if !ok || ttff != 90*time.Second {
		t.Fatalf("analyzer ttff = %s ok=%v, want 90s", ttff, ok)
	}
	// A retry that then finds something does not move it.
	if _, ok := tr.FirstLook("app", t0.Add(2*time.Minute), 4, 1, time.Second); ok {
		t.Fatal("a later first look moved the first finding")
	}
	m := tr.Snapshot()[0]
	if m.TTFF != 90*time.Second || m.FirstLookItems != 4 || m.FirstLookFindings != 1 {
		t.Fatalf("metrics = %+v, want the 90s ttff kept and the retry's counts", m)
	}
}

// Boundary: findings is what decides, not items; one finding is enough.
func TestTrackerFindingsBoundary(t *testing.T) {
	for _, c := range []struct {
		items, findings int
		want            bool
	}{{0, 0, false}, {1, 0, false}, {1, 1, true}, {50, 0, false}} {
		tr := NewTracker()
		tr.Start("app", time.Now())
		if _, got := tr.FirstLook("app", time.Now(), c.items, c.findings,
			time.Millisecond); got != c.want {
			t.Errorf("items %d findings %d: recorded=%v, want %v", c.items, c.findings,
				got, c.want)
		}
	}
}

// Concurrent access (run with -race where cgo is available): FirstLook and
// FirstFinding racing on one database record exactly one first finding.
func TestTrackerFirstLookRacesFirstFinding(t *testing.T) {
	for round := 0; round < 20; round++ {
		tr := NewTracker()
		t0 := time.Now()
		tr.Start("app", t0)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func(i int) {
				defer wg.Done()
				at := t0.Add(time.Duration(i+1) * time.Second)
				if _, ok := tr.FirstLook("app", at, 3, 1, time.Second); ok {
					wins.Add(1)
				}
			}(i)
			go func(i int) {
				defer wg.Done()
				if _, ok := tr.FirstFinding("app", t0.Add(time.Duration(i+1)*
					time.Second)); ok {
					wins.Add(1)
				}
				_ = tr.Snapshot()
			}(i)
		}
		wg.Wait()
		m := tr.Snapshot()[0]
		if wins.Load() != 1 || !m.HasTTFF || m.FirstLookFindings != 1 {
			t.Fatalf("round %d: %d first findings recorded, metrics %+v; want exactly one",
				round, wins.Load(), m)
		}
	}
}
