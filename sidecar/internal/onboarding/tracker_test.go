package onboarding

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrackerFirstLookWithFindingsRecordsTTFF(t *testing.T) {
	tr := NewTracker()
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	tr.Start("app", t0)
	ttff, recorded := tr.FirstLook("app", t0.Add(9*time.Second), 3, 1500*time.Millisecond)
	if !recorded || ttff != 9*time.Second {
		t.Fatalf("ttff = %s recorded=%v, want 9s", ttff, recorded)
	}
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	m := snap[0]
	if m.Database != "app" || !m.FirstLookDone || m.FirstLookItems != 3 ||
		m.FirstLookDuration != 1500*time.Millisecond || !m.HasTTFF || m.TTFF != 9*time.Second {
		t.Fatalf("metrics = %+v", m)
	}
	// A later finding does not move the first one.
	if _, ok := tr.FirstFinding("app", t0.Add(time.Minute)); ok {
		t.Fatal("a second first finding was recorded")
	}
}

func TestTrackerEmptyFirstLookWaitsForAFinding(t *testing.T) {
	tr := NewTracker()
	t0 := time.Now()
	tr.Start("app", t0)
	if _, recorded := tr.FirstLook("app", t0.Add(time.Second), 0, time.Second); recorded {
		t.Fatal("an empty first look recorded a first finding")
	}
	if m := tr.Snapshot()[0]; m.HasTTFF || !m.FirstLookDone || m.FirstLookItems != 0 {
		t.Fatalf("metrics after an empty first look = %+v", m)
	}
	ttff, ok := tr.FirstFinding("app", t0.Add(4*time.Minute))
	if !ok || ttff != 4*time.Minute {
		t.Fatalf("analyzer finding ttff = %s ok=%v", ttff, ok)
	}
}

func TestTrackerUnknownDatabaseAndRestart(t *testing.T) {
	tr := NewTracker()
	if _, ok := tr.FirstLook("ghost", time.Now(), 2, time.Second); ok {
		t.Fatal("a database that never started recorded a finding")
	}
	if _, ok := tr.FirstFinding("ghost", time.Now()); ok {
		t.Fatal("a database that never started recorded a finding")
	}
	if len(tr.Snapshot()) != 0 {
		t.Fatalf("snapshot = %+v, want empty", tr.Snapshot())
	}
	t0 := time.Now()
	tr.Start("app", t0)
	tr.FirstLook("app", t0.Add(time.Second), 1, time.Second)
	// A new runtime generation (reload, restart) measures again.
	tr.Start("app", t0.Add(time.Hour))
	if m := tr.Snapshot()[0]; m.HasTTFF || m.FirstLookDone {
		t.Fatalf("restarted metrics = %+v, want a fresh measurement", m)
	}
	tr.Forget("app")
	if len(tr.Snapshot()) != 0 {
		t.Fatal("forgotten database still reported")
	}
	var nilTracker *Tracker
	nilTracker.Start("x", t0)
	if nilTracker.Snapshot() != nil {
		t.Fatal("nil tracker reported metrics")
	}
}

func TestTrackerSnapshotIsSorted(t *testing.T) {
	tr := NewTracker()
	for _, n := range []string{"c", "a", "b"} {
		tr.Start(n, time.Now())
	}
	s := tr.Snapshot()
	if len(s) != 3 || s[0].Database != "a" || s[1].Database != "b" || s[2].Database != "c" {
		t.Fatalf("snapshot order = %+v", s)
	}
}

func TestTrackerConcurrentFirstFindingRecordsOnce(t *testing.T) {
	tr := NewTracker()
	t0 := time.Now()
	tr.Start("app", t0)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, ok := tr.FirstFinding("app", t0.Add(time.Duration(i+1)*time.Second)); ok {
				wins.Add(1)
			}
			_ = tr.Snapshot()
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("first finding recorded %d times, want once", wins.Load())
	}
}
