package selfbudget

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// observability-06: a loop's failed runs and its last successful run.

var errCycle = errors.New("collection failed: connection refused")

// Error propagation: a failed run counts as a run and an error, and leaves
// the last success where it was.
func TestLoops_TrackErrCountsErrorsAndKeepsLastSuccess(t *testing.T) {
	l := NewLoops()
	before := time.Now()
	l.TrackErr("collector")(nil)
	ok := l.Snapshot()["collector"]
	if ok.Runs != 1 || ok.Errors != 0 || ok.LastSuccess.Before(before) ||
		ok.LastSuccess.After(time.Now()) {
		t.Fatalf("after a success = %+v, want 1 run, 0 errors, last success now", ok)
	}
	l.TrackErr("collector")(errCycle)
	failed := l.Snapshot()["collector"]
	if failed.Runs != 2 || failed.Errors != 1 {
		t.Fatalf("after a failure runs/errors = %d/%d, want 2/1", failed.Runs, failed.Errors)
	}
	if !failed.LastSuccess.Equal(ok.LastSuccess) {
		t.Fatalf("a failed run moved last success from %v to %v", ok.LastSuccess,
			failed.LastSuccess)
	}
}

// Happy path: each success moves the last success forward; busy time is
// still recorded for failed and successful runs alike.
func TestLoops_TrackErrSuccessMovesLastSuccessForward(t *testing.T) {
	l := NewLoops()
	l.TrackErr("analyzer")(nil)
	first := l.Snapshot()["analyzer"].LastSuccess
	time.Sleep(5 * time.Millisecond)
	done := l.TrackErr("analyzer")
	time.Sleep(10 * time.Millisecond)
	done(nil)
	s := l.Snapshot()["analyzer"]
	if !s.LastSuccess.After(first) {
		t.Fatalf("last success %v did not move past %v", s.LastSuccess, first)
	}
	if s.Busy < 10*time.Millisecond || s.Runs != 2 {
		t.Fatalf("analyzer = %+v, want 2 runs and at least 10ms busy", s)
	}
}

// Nil/empty: a loop that never succeeded has no last success; a nil
// tracker records nothing and does not panic.
func TestLoops_TrackErrNeverSucceededAndNilTracker(t *testing.T) {
	l := NewLoops()
	l.TrackErr("sre")(errCycle)
	l.TrackErr("sre")(errCycle)
	s := l.Snapshot()["sre"]
	if s.Runs != 2 || s.Errors != 2 || !s.LastSuccess.IsZero() {
		t.Fatalf("sre = %+v, want 2 runs, 2 errors, no last success", s)
	}
	var none *Loops
	none.TrackErr("sre")(errCycle)
	none.Track("sre")()
	if got := none.Snapshot(); len(got) != 0 {
		t.Fatalf("nil tracker snapshot = %v, want empty", got)
	}
}

// Track is TrackErr with no error: a run that reports nothing succeeded.
func TestLoops_TrackIsASuccessfulRun(t *testing.T) {
	l := NewLoops()
	l.Track("executor")()
	l.Add("executor", time.Millisecond)
	s := l.Snapshot()["executor"]
	if s.Runs != 2 || s.Errors != 0 || s.LastSuccess.IsZero() {
		t.Fatalf("executor = %+v, want 2 successful runs", s)
	}
}

// Boundary: errors do not change what the budget breach finding reads:
// TopLoops ranks by busy time and runs only.
func TestTopLoops_IgnoresErrorsAndLastSuccess(t *testing.T) {
	prev := map[string]LoopStat{"collector": {Busy: time.Second, Runs: 1}}
	cur := map[string]LoopStat{
		"collector": {Busy: 3 * time.Second, Runs: 2, Errors: 9,
			LastSuccess: time.Unix(1, 0)},
		"analyzer": {Busy: time.Second, Runs: 1, Errors: 1},
	}
	got := TopLoops(prev, cur, 5)
	if len(got) != 2 || got[0].Name != "collector" || got[0].BusyMs != 2000 ||
		got[0].Runs != 1 || got[1].Name != "analyzer" {
		t.Fatalf("top = %+v, want collector 2000ms/1 run then analyzer", got)
	}
}

// Concurrency: 8 goroutines tracking mixed results lose no run or error.
func TestLoops_TrackErrConcurrent(t *testing.T) {
	const workers, n = 8, 200
	l := NewLoops()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				var err error
				if (w+i)%3 == 0 {
					err = errCycle
				}
				l.TrackErr("collector")(err)
				_ = l.Snapshot()
			}
		}(w)
	}
	wg.Wait()
	wantErrs := int64(0)
	for w := 0; w < workers; w++ {
		for i := 0; i < n; i++ {
			if (w+i)%3 == 0 {
				wantErrs++
			}
		}
	}
	s := l.Snapshot()["collector"]
	if s.Runs != workers*n || s.Errors != wantErrs || s.LastSuccess.IsZero() {
		t.Fatalf("collector = %+v, want %d runs and %d errors", s, workers*n, wantErrs)
	}
}
