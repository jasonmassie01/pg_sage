package sre

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06 review: on a live store a tick that does its work is a
// successful run of the sre loop, and a failing trigger source is a
// failed run that still resumes pending work.

// No concurrent access tests here: a coordinator runs one tick at a time,
// and the tracker's locking is tested in selfbudget.

// Happy path, then error propagation: success moves last success; a
// trigger failure is a failed run that leaves it.
func TestTrackedTick_LiveStoreSuccessThenTriggerFailure(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	src := &staticTriggers{}
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), src)
	c.cfg.AutomaticStart = true
	c.loops = selfbudget.NewLoops()
	var last time.Time
	before := time.Now()
	c.trackedTick(ctx, &last)
	ok := c.loops.Snapshot()["sre"]
	if ok.Runs != 1 || ok.Errors != 0 || ok.LastSuccess.Before(before) {
		t.Fatalf("after a working tick = %+v, want 1 run, 0 errors, success now", ok)
	}
	src.mu.Lock()
	src.err = errors.New("permission denied for table incidents")
	src.mu.Unlock()
	queued, _, err := c.Start(ctx, lockTrigger("inc-obs06"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.tick(ctx, &last); err == nil ||
		!strings.Contains(err.Error(), "read investigation triggers: permission denied") {
		t.Fatalf("tick with a failing trigger source = %v, want the trigger error", err)
	}
	c.trackedTick(ctx, &last)
	failed := c.loops.Snapshot()["sre"]
	if failed.Runs != 2 || failed.Errors != 1 || !failed.LastSuccess.Equal(ok.LastSuccess) {
		t.Fatalf("after a failed tick = %+v, want 2 runs, 1 error, last success kept",
			failed)
	}
	got, err := st.Get(ctx, queued.Scope, queued.ID)
	if err != nil || got.State != StateConcluded {
		t.Fatalf("queued investigation = %+v, %v; want it resumed to concluded", got, err)
	}
}
