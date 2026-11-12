package gameday

import (
	"context"
	"testing"
	"time"
)

// benches-03 (review): another process abandons a run still running
// MaxRunTime after it started and destroys its clone, so a live run must
// never get that old. Each run's clone and fault programs are bounded by
// runBudget, which leaves an hour for the recovery, destroy and result
// that still run after the budget ends.

// checkBudget asserts that the fault programs ran under a deadline of
// runBudget from (about) when the run started.
func checkBudget(t *testing.T, f *fakeFaults, started time.Time) {
	t.Helper()
	if len(f.deadlines) != 1 || f.deadlines[0].IsZero() {
		t.Fatalf("fault program deadlines = %v, want one deadline", f.deadlines)
	}
	left := f.deadlines[0].Sub(started)
	if left > runBudget || left < runBudget-time.Minute {
		t.Fatalf("fault programs had %s, want the %s run budget", left, runBudget)
	}
}

func TestRunBudgetEndsBeforeMaxRunTime(t *testing.T) {
	// Boundary: the budget plus its margin is below the abandon age.
	if runBudget <= 0 || runBudget > MaxRunTime-time.Hour {
		t.Fatalf("runBudget = %s, want at least an hour below MaxRunTime %s", runBudget,
			MaxRunTime)
	}
}

func TestLocalBenchRunIsBoundedByTheRunBudget(t *testing.T) {
	p, f := &fakeProvider{}, &fakeFaults{report: []byte(scoredReport)}
	b := newLocalBench(t, p, f, &fakeBenchLedger{})
	started := time.Now()
	if _, err := b.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, f, started)
}

// A background local run (Start) outlives its request's context, but not
// the run budget.
func TestStartedLocalBenchRunIsBoundedByTheRunBudget(t *testing.T) {
	p, f := &fakeProvider{}, &fakeFaults{report: []byte(scoredReport)}
	b := newLocalBench(t, p, f, &fakeBenchLedger{})
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	if _, err := b.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitLocalIdle(t, b)
	checkBudget(t, f, started)
	if len(p.destroyed) != 1 {
		t.Fatalf("destroyed %d clones, want 1", len(p.destroyed))
	}
}

func TestGameDayIsBoundedByTheRunBudget(t *testing.T) {
	f := newGDFixture(t)
	started := time.Now()
	if _, err := f.runner().Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, f.faults, started)
}

func waitLocalIdle(t *testing.T, b *LocalBench) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the local run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
