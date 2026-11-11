package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06: a collector cycle that produced no snapshot reports
// an error to the loop tracker, so a loop that fails every cycle no
// longer looks healthy.

// No concurrent access tests here: the tracker's locking is tested in
// selfbudget, and a collector runs one cycle at a time.

// Error propagation: a skipped cycle (the load query failed) is a failed
// run of the collector loop and leaves its last success unset.
func TestTrackedCycle_LoadQueryFailureIsALoopError(t *testing.T) {
	c, _ := breakerCollector(t, &fakeLoadDB{err: context.DeadlineExceeded})
	c.loops = selfbudget.NewLoops()
	c.trackedCycle(context.Background(), &fakeTicker{})
	c.trackedCycle(context.Background(), &fakeTicker{})
	s := c.loops.Snapshot()["collector"]
	if s.Runs != 2 || s.Errors != 2 || !s.LastSuccess.IsZero() {
		t.Fatalf("collector loop = %+v, want 2 runs, 2 errors, no last success", s)
	}
}

// The cycle names why it produced nothing.
func TestCycle_ReturnsTheSkipCause(t *testing.T) {
	c, _ := breakerCollector(t, &fakeLoadDB{ratio: 0.97})
	err := c.cycle(context.Background(), &fakeTicker{})
	if !errors.Is(err, ErrCycleSkipped) {
		t.Fatalf("cycle under load = %v, want ErrCycleSkipped", err)
	}
}

// Integration and happy path: a successful cycle on a live database moves
// the loop's last success forward; a later failed one leaves it.
func TestTrackedCycle_SuccessMovesLastSuccess(t *testing.T) {
	pool := testPool(t)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err) // the cycle persists to sage.snapshots
	}
	rec := &logRecorder{}
	c := New(pool, testConfig(), serverVersion(t, pool), rec.log)
	c.loops = selfbudget.NewLoops()
	ticker := &fakeTicker{}
	ctx := context.Background()
	before := time.Now()
	c.trackedCycle(ctx, ticker)
	first := c.loops.Snapshot()["collector"]
	if first.Errors != 0 || first.Runs != 1 || first.LastSuccess.Before(before) {
		t.Fatalf("after a successful cycle = %+v, want 1 run, 0 errors, success now;"+
			" logs: %v", first, rec.lines)
	}
	time.Sleep(5 * time.Millisecond)
	c.trackedCycle(ctx, ticker)
	second := c.loops.Snapshot()["collector"]
	if !second.LastSuccess.After(first.LastSuccess) {
		t.Fatalf("second success %v did not move past %v", second.LastSuccess,
			first.LastSuccess)
	}
	c.loadDB = &fakeLoadDB{err: errors.New("permission denied for view pg_stat_activity")}
	c.trackedCycle(ctx, ticker)
	third := c.loops.Snapshot()["collector"]
	if third.Errors != 1 || !third.LastSuccess.Equal(second.LastSuccess) {
		t.Fatalf("after a failed cycle = %+v, want 1 error and last success kept", third)
	}
}
