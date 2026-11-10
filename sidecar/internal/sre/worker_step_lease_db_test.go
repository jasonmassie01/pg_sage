package sre

import (
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06 review: a plan step heartbeats its lease once, then runs its
// probes, and each probe may now wait for its slots twice (retryQueued).
// A step of four probes can therefore outlast the lease TTL; the step
// must hold its lease while its probes run, or its commit is refused as
// a stale worker's and the investigation loses its claim.
// No concurrency test beyond the heartbeat goroutine itself: the step's
// probes run in order.

func TestCollect_SlowRetriedStepOutlastingTheLeaseTTLStillCommits(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 2 * time.Second
	st, _, ctx := liveStore(t, limits)
	runner := idleChainRunner()
	graph := runner.scripts[probes.LockGraph][0]
	runner.script(probes.LockGraph, queueRefusal(probes.LockGraph), graph)
	// Five runs (four plan probes and one retry) of 600 ms each: 3 s, well
	// past the 2 s lease the step heartbeat once before it began.
	runner.hook = func(probes.ID, int) { time.Sleep(600 * time.Millisecond) }
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, lockTrigger("slow-step"))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.ProbeCount != 4 {
		t.Fatalf("investigation = %s root %q with %d probes, want concluded "+
			"idle_in_tx_holder from 4", inv.State, inv.Summary.Root, inv.ProbeCount)
	}
	if runner.total() != 5 {
		t.Fatalf("probe runs = %d, want 5 (4 plus one retry)", runner.total())
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatalf("event chain: %v", err)
	}
}

func TestRunStepHeld_LostLeaseIsReturnedAndStopsTheStep(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = time.Second
	st, _, ctx := liveStore(t, limits)
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv, _, err := c.Start(ctx, lockTrigger("lost-step"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, c.worker)
	if err != nil {
		t.Fatal(err)
	}
	// Another worker's fence: every heartbeat of this lease is refused.
	stale := lease
	stale.Fence++
	runner.hook = func(probes.ID, int) { time.Sleep(700 * time.Millisecond) }
	plan, _ := planFor(TriggerLock, time.Hour)
	results, _, err := c.runStepHeld(ctx, stale, plan[0])
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("runStepHeld on a lost lease: err = %v, want ErrLeaseLost", err)
	}
	if len(results) != len(plan[0].calls) {
		t.Fatalf("results = %d, want one per call (%d)", len(results), len(plan[0].calls))
	}
}
