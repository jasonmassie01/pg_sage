package analyzer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// fleetpol-02a: a producer that runs between cycles (the agent posture
// exposure sweep) publishes through RunOutOfCycle, which never runs while
// a cycle does.

func TestRunOutOfCycle_NilRunIsRefused(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	if err := a.RunOutOfCycle(context.Background(), nil); !errors.Is(err, ErrNoOutOfCycleRun) {
		t.Fatalf("err = %v, want ErrNoOutOfCycleRun", err)
	}
}

// A run that had nothing to do publishes nothing: with no pool, a
// publish would fail, so a nil error proves nothing was written.
func TestRunOutOfCycle_NothingToDoPublishesNothing(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	calls := 0
	err := a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle, bool,
		error) {
		calls++
		return OutOfCycle{Findings: []Finding{lifecycleFinding("x", "y", "info")}}, false, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want nil and 1", err, calls)
	}
	if got := a.LatestFindings(); len(got) != 0 {
		t.Fatalf("latest findings = %+v, want none", got)
	}
}

// Error propagation: the run's error is returned wrapped, with nothing
// published.
func TestRunOutOfCycle_RunErrorIsReturned(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	boom := errors.New("catalog read failed")
	err := a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle, bool,
		error) {
		return OutOfCycle{}, true, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

// Without a pool there is nowhere to persist: a run with findings fails
// with ErrNoPool instead of dropping them silently.
func TestRunOutOfCycle_NoPoolFails(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	err := a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle, bool,
		error) {
		return OutOfCycle{Findings: []Finding{lifecycleFinding("x", "y", "info")}}, true, nil
	})
	if !errors.Is(err, ErrOutOfCycleNoPool) {
		t.Fatalf("err = %v, want ErrOutOfCycleNoPool", err)
	}
}

// Concurrency: while a cycle holds the cycle lock, an out-of-cycle run
// waits; it starts only once the cycle is done.
func TestRunOutOfCycle_WaitsForARunningCycle(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.cycleMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle,
			bool, error) {
			close(started)
			return OutOfCycle{}, false, nil
		})
	}()
	select {
	case <-started:
		t.Fatal("the out-of-cycle run started while a cycle held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	a.cycleMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the out-of-cycle run never ran after the cycle released the lock")
	}
}

// A context that ends while waiting for a cycle returns its error and
// never runs the producer.
func TestRunOutOfCycle_ContextEndsWhileWaiting(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.cycleMu.Lock()
	defer a.cycleMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ran := false
	err := a.RunOutOfCycle(ctx, func(context.Context) (OutOfCycle, bool, error) {
		ran = true
		return OutOfCycle{}, false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("err = %v, ran = %v; want the deadline and no run", err, ran)
	}
}

// mergeOutOfCycle replaces the latest findings of the run's categories
// and keeps every other category's.
func TestMergeOutOfCycle_ReplacesOnlyTheRunsCategories(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	keep := lifecycleFinding("other", "public.a", "warning")
	old := lifecycleFinding("posture", "public.gone", "critical")
	a.SetFindings([]Finding{keep, old})
	fresh := lifecycleFinding("posture", "public.new", "critical")
	a.mergeOutOfCycle([]Finding{fresh}, map[string]bool{"posture": true})
	got := a.LatestFindings()
	if len(got) != 2 || got[0].ObjectIdentifier != "public.a" ||
		got[1].ObjectIdentifier != "public.new" {
		t.Fatalf("latest = %+v, want public.a then public.new", got)
	}
}

// Concurrency: the cycle itself holds the cycle lock, so an out-of-cycle
// run started while a cycle runs returns only after the cycle is done.
func TestRunOutOfCycle_WaitsForTheCycleItself(t *testing.T) {
	cfg := phase2Config()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	logFn := func(_, msg string, _ ...any) {
		if strings.Contains(msg, "no snapshot yet, skipping") {
			once.Do(func() { close(entered); <-release })
		}
	}
	a := New(nil, cfg, collector.New(nil, cfg, 170000, noopLog), nil, nil, nil, nil, logFn)
	cycleDone := make(chan error, 1)
	go func() { cycleDone <- a.cycle(context.Background()) }()
	<-entered
	runDone := make(chan error, 1)
	go func() {
		runDone <- a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle,
			bool, error) {
			return OutOfCycle{}, false, nil
		})
	}()
	select {
	case err := <-runDone:
		t.Fatalf("the out-of-cycle run returned (%v) while a cycle ran", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	<-cycleDone
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the out-of-cycle run never returned after the cycle ended")
	}
}

// Before the collector's first snapshot there is nothing to derive the
// run's findings against: the run is not started (the first cycle, which
// waits for that snapshot, runs every detector) and nothing is published.
func TestRunOutOfCycle_WaitsForTheFirstSnapshot(t *testing.T) {
	cfg := phase2Config()
	a := New(nil, cfg, collector.New(nil, cfg, 170000, noopLog), nil, nil, nil, nil, noopLog)
	calls := 0
	err := a.RunOutOfCycle(context.Background(), func(context.Context) (OutOfCycle, bool,
		error) {
		calls++
		return OutOfCycle{Findings: []Finding{lifecycleFinding("x", "y", "info")}}, true, nil
	})
	if err != nil || calls != 0 {
		t.Fatalf("err = %v, calls = %d; want nil and no run", err, calls)
	}
	if got := a.LatestFindings(); len(got) != 0 {
		t.Fatalf("latest findings = %+v, want none", got)
	}
}
