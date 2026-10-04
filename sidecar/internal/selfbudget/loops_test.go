package selfbudget

import (
	"sync"
	"testing"
	"time"
)

func TestLoops_TrackAccumulatesBusyTimeAndRuns(t *testing.T) {
	l := NewLoops()
	l.Add("collector", 300*time.Millisecond)
	l.Add("collector", 200*time.Millisecond)
	l.Add("analyzer", 100*time.Millisecond)
	got := l.Snapshot()
	if c := got["collector"]; c.Busy != 500*time.Millisecond || c.Runs != 2 {
		t.Fatalf("collector = %+v, want 500ms over 2 runs", c)
	}
	if a := got["analyzer"]; a.Busy != 100*time.Millisecond || a.Runs != 1 {
		t.Fatalf("analyzer = %+v", a)
	}
}

func TestLoops_TrackMeasuresSinceStart(t *testing.T) {
	l := NewLoops()
	done := l.Track("advisor")
	time.Sleep(20 * time.Millisecond)
	done()
	s := l.Snapshot()["advisor"]
	if s.Runs != 1 || s.Busy < 20*time.Millisecond || s.Busy > 5*time.Second {
		t.Fatalf("advisor = %+v, want one run of at least 20ms", s)
	}
}

// Snapshot is a copy: later work does not change an earlier snapshot.
func TestLoops_SnapshotIsACopy(t *testing.T) {
	l := NewLoops()
	l.Add("collector", time.Second)
	before := l.Snapshot()
	l.Add("collector", time.Second)
	if before["collector"].Busy != time.Second {
		t.Fatalf("snapshot changed after it was taken: %+v", before["collector"])
	}
}

func TestTopLoops_DeltaSortedAndCapped(t *testing.T) {
	prev := map[string]LoopStat{
		"collector": {Busy: 10 * time.Second, Runs: 10},
		"analyzer":  {Busy: 5 * time.Second, Runs: 5},
		"idle":      {Busy: time.Second, Runs: 1},
	}
	cur := map[string]LoopStat{
		"collector": {Busy: 11 * time.Second, Runs: 11}, // +1s
		"analyzer":  {Busy: 8 * time.Second, Runs: 6},   // +3s
		"idle":      {Busy: time.Second, Runs: 1},       // no work: left out
		"runway":    {Busy: 2 * time.Second, Runs: 1},   // new: counts in full
	}
	got := TopLoops(prev, cur, 2)
	if len(got) != 2 {
		t.Fatalf("top = %+v, want 2", got)
	}
	if got[0].Name != "analyzer" || got[0].BusyMs != 3000 || got[0].Runs != 1 {
		t.Fatalf("first = %+v, want analyzer 3000ms 1 run", got[0])
	}
	if got[1].Name != "runway" || got[1].BusyMs != 2000 {
		t.Fatalf("second = %+v, want runway 2000ms", got[1])
	}
	all := TopLoops(prev, cur, 10)
	if len(all) != 3 {
		t.Fatalf("all = %+v, want 3 loops that worked", all)
	}
}

func TestTopLoops_TiesByNameAndResets(t *testing.T) {
	prev := map[string]LoopStat{"b": {Busy: 5 * time.Second, Runs: 5}}
	cur := map[string]LoopStat{
		"b": {Busy: time.Second, Runs: 1}, // went back: restarted, counts from zero
		"a": {Busy: time.Second, Runs: 1},
	}
	got := TopLoops(prev, cur, 5)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" || got[1].BusyMs != 1000 {
		t.Fatalf("top = %+v, want a then b, b counted from zero", got)
	}
}

func TestTopLoops_EmptyAndZero(t *testing.T) {
	if got := TopLoops(nil, nil, 5); len(got) != 0 {
		t.Fatalf("nil maps: %+v", got)
	}
	cur := map[string]LoopStat{"a": {Busy: time.Second, Runs: 1}}
	if got := TopLoops(nil, cur, 0); len(got) != 0 {
		t.Fatalf("n=0: %+v", got)
	}
	if got := TopLoops(nil, cur, -1); len(got) != 0 {
		t.Fatalf("n<0: %+v", got)
	}
}

func TestLoops_NilIsSafe(t *testing.T) {
	var l *Loops
	l.Add("x", time.Second)
	l.Track("x")()
	if s := l.Snapshot(); len(s) != 0 {
		t.Fatalf("nil loops snapshot = %+v", s)
	}
}

func TestLoops_ConcurrentTracking(t *testing.T) {
	l := NewLoops()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				l.Add("collector", time.Millisecond)
				_ = l.Snapshot()
			}
		}()
	}
	wg.Wait()
	if s := l.Snapshot()["collector"]; s.Runs != 4000 || s.Busy != 4*time.Second {
		t.Fatalf("collector = %+v, want 4000 runs and 4s", s)
	}
}

func TestProcessLoopsIsShared(t *testing.T) {
	if Process() == nil || Process() != Process() {
		t.Fatal("Process() must return one shared tracker")
	}
}
