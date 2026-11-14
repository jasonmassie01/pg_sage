package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// config-04c: the builder's retry loop, with fake build and catalog
// reads. The loop runs in one goroutine; the stop watcher is the only
// concurrent part and is covered by the stop tests.

type fakeBuilderRun struct {
	mu       sync.Mutex
	counts   []DeferredIndexCounts // states returned in order; the last repeats
	builds   []error               // build results in order; the last repeats
	nBuilds  int
	sleeps   []time.Duration
	observed []DeferredIndexCounts
	warns    []string
	infos    []string
	cancel   context.CancelFunc
	maxSleep int // cancel the run after this many sleeps
}

func (f *fakeBuilderRun) builder() *DeferredIndexBuilder {
	nStates := 0
	return &DeferredIndexBuilder{
		Database: "db1",
		Observe: func(c DeferredIndexCounts) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.observed = append(f.observed, c)
		},
		states: func(context.Context) (DeferredIndexCounts, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			c := f.counts[min(nStates, len(f.counts)-1)]
			nStates++
			return c, nil
		},
		build: func(context.Context) (BuildReport, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			err := f.builds[min(f.nBuilds, len(f.builds)-1)]
			f.nBuilds++
			return BuildReport{}, err
		},
		sleep: func(ctx context.Context, d time.Duration) bool {
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			n := len(f.sleeps)
			f.mu.Unlock()
			if f.maxSleep > 0 && n >= f.maxSleep {
				f.cancel()
			}
			return ctx.Err() == nil
		},
		warn: func(msg string, _ ...any) { f.warns = append(f.warns, msg) },
		info: func(msg string, _ ...any) { f.infos = append(f.infos, msg) },
	}
}

func runFake(t *testing.T, f *fakeBuilderRun, b *DeferredIndexBuilder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.cancel = cancel
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

var pending1 = DeferredIndexCounts{Missing: 1}

// Nil/empty: nothing missing at start builds nothing and reports zero.
func TestDeferredBuilder_NothingPendingReturnsAtOnce(t *testing.T) {
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{{}}, builds: []error{nil}}
	runFake(t, f, f.builder())
	if f.nBuilds != 0 || len(f.sleeps) != 0 {
		t.Fatalf("builds %d, sleeps %v; want none", f.nBuilds, f.sleeps)
	}
	if len(f.observed) != 1 || f.observed[0] != (DeferredIndexCounts{}) {
		t.Fatalf("observed %v, want one zero sample", f.observed)
	}
}

// Happy path: one build, the gauge goes to zero, the loop ends.
func TestDeferredBuilder_BuildsThenStops(t *testing.T) {
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1, {}}, builds: []error{nil}}
	runFake(t, f, f.builder())
	if f.nBuilds != 1 || len(f.sleeps) != 0 {
		t.Fatalf("builds %d, sleeps %v; want one build, no sleep", f.nBuilds, f.sleeps)
	}
	if last := f.observed[len(f.observed)-1]; last != (DeferredIndexCounts{}) {
		t.Fatalf("last observed %v, want zero", last)
	}
	if f.observed[0] != pending1 {
		t.Fatalf("first observed %v, want %v", f.observed[0], pending1)
	}
}

// Error propagation and boundary: failures back off 1 min, 2 min, 4 min
// ... capped at 1 h; one WARN per failure streak.
func TestDeferredBuilder_BacksOffAndWarnsOncePerStreak(t *testing.T) {
	failure := errors.New(`build sage.idx_decision_created on sage.decision: lock timeout`)
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1},
		builds: []error{failure, failure, failure, nil, failure, nil}}
	f.counts = []DeferredIndexCounts{pending1, pending1, pending1, pending1, pending1,
		pending1, pending1, pending1, pending1, pending1, pending1, {}}
	runFake(t, f, f.builder())
	// A build that succeeded with an index still missing is a retry too.
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 16 * time.Minute}
	if fmt.Sprint(f.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps %v, want %v", f.sleeps, want)
	}
	if len(f.warns) != 2 {
		t.Fatalf("warns %d (%v), want one per failure streak (2)", len(f.warns), f.warns)
	}
}

func TestDeferredBuilderBackoff_Caps(t *testing.T) {
	cases := map[int]time.Duration{0: time.Minute, 1: 2 * time.Minute, 5: 32 * time.Minute,
		6: time.Hour, 7: time.Hour, 60: time.Hour, -1: time.Minute}
	for n, want := range cases {
		if got := deferredBuilderBackoff(n); got != want {
			t.Errorf("deferredBuilderBackoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// Concurrency: another sidecar building is not a failure: no WARN, retry.
func TestDeferredBuilder_BusyRetriesWithoutWarning(t *testing.T) {
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1, pending1, pending1, {}},
		builds: []error{fmt.Errorf("db1: %w", ErrDeferredBuildBusy)}}
	runFake(t, f, f.builder())
	if len(f.warns) != 0 {
		t.Fatalf("busy builder warned: %v", f.warns)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] != time.Minute {
		t.Fatalf("sleeps %v, want one 1 min retry", f.sleeps)
	}
}

// State transition: ctx cancelled while waiting to retry ends the loop.
func TestDeferredBuilder_CancelEndsTheLoop(t *testing.T) {
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1},
		builds: []error{errors.New("boom")}, maxSleep: 1}
	runFake(t, f, f.builder())
	if f.nBuilds != 1 || len(f.sleeps) != 1 {
		t.Fatalf("builds %d, sleeps %v; want one each before the cancel", f.nBuilds, f.sleeps)
	}
}

// State transition: an emergency stop cancels a running build, and no
// build starts while the stop holds.
func TestDeferredBuilder_EmergencyStopCancelsTheBuild(t *testing.T) {
	var stopped atomic.Bool
	started := make(chan struct{})
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1}, maxSleep: 2}
	b := f.builder()
	b.Stopped = stopped.Load
	b.stopPoll = 5 * time.Millisecond
	var buildErr error
	b.build = func(ctx context.Context) (BuildReport, error) {
		f.nBuilds++
		close(started)
		<-ctx.Done()
		buildErr = ctx.Err()
		return BuildReport{}, ctx.Err()
	}
	go func() { <-started; stopped.Store(true) }()
	runFake(t, f, b)
	if f.nBuilds != 1 {
		t.Fatalf("builds %d, want 1 (none while stopped)", f.nBuilds)
	}
	if !errors.Is(buildErr, context.Canceled) {
		t.Fatalf("build ended with %v, want context.Canceled", buildErr)
	}
	if len(f.warns) != 0 {
		t.Fatalf("a stop-cancelled build warned: %v", f.warns)
	}
}

func TestDeferredBuilder_NoBuildWhileStopped(t *testing.T) {
	f := &fakeBuilderRun{counts: []DeferredIndexCounts{pending1}, builds: []error{nil},
		maxSleep: 3}
	b := f.builder()
	b.Stopped = func() bool { return true }
	runFake(t, f, b)
	if f.nBuilds != 0 {
		t.Fatalf("built %d times during an emergency stop", f.nBuilds)
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("an elapsed sleep reported a cancel")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("a cancelled sleep reported it elapsed")
	}
	if time.Since(start) > time.Second {
		t.Fatal("a cancelled sleep waited")
	}
}
