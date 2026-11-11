package analyzer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06 review: on a live database a cycle over a fresh
// snapshot is a successful run; the same snapshot again is a failed run.

// No concurrent access tests here: an analyzer runs one cycle at a time,
// and the tracker's locking is tested in selfbudget.

// Integration, happy path and state transition: fresh, then stale.
func TestTrackedCycle_FreshThenStaleSnapshot(t *testing.T) {
	p, ctx := preflightPool(t)
	cfg := testConfigLive()
	c := collector.New(p, cfg, 170000, noopLog)
	stopAfterOneSnapshot(t, c)
	a := New(p, cfg, c, nil, nil, nil, nil, noopLog)
	a.loops = selfbudget.NewLoops()
	before := time.Now()
	a.trackedCycle(ctx)
	ok := a.loops.Snapshot()["analyzer"]
	if ok.Runs != 1 || ok.Errors != 0 || ok.LastSuccess.Before(before) {
		t.Fatalf("after a fresh cycle = %+v, want 1 run, 0 errors, success now", ok)
	}
	if err := a.cycle(ctx); !errors.Is(err, errStaleSnapshot) {
		t.Fatalf("cycle over the analyzed snapshot = %v, want errStaleSnapshot", err)
	}
	a.trackedCycle(ctx)
	stale := a.loops.Snapshot()["analyzer"]
	if stale.Runs != 2 || stale.Errors != 1 || !stale.LastSuccess.Equal(ok.LastSuccess) {
		t.Fatalf("after a stale cycle = %+v, want 2 runs, 1 error, last success kept",
			stale)
	}
}

// testConfigLive is the preflight fixture's settings without its log tap.
func testConfigLive() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 1
	cfg.Advisor.Enabled = false
	cfg.Safety.QueryTimeoutMs = 5000
	return cfg
}

// stopAfterOneSnapshot runs c until it has a snapshot, then stops it, so
// its latest snapshot no longer changes.
func stopAfterOneSnapshot(t *testing.T, c *collector.Collector) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); c.LatestSnapshot() == nil &&
		time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if c.LatestSnapshot() == nil {
		t.Fatal("collector produced no snapshot")
	}
}
