package main

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// T5 round 2 (ci-19 -race, PG15): two fleet inits in one process each
// started a roll-up loop with no way to stop it, and both loops drove the
// same briefing workers, racing on their last-run state. One loop per
// process now owns the workers. No integration case: the loop's tick
// (RunRollup) is covered by the briefing package; this is its lifecycle.

// rollupFixture sets a fleet-mode config with the given briefing.fleet_mode
// and the shutdown context, restoring both (and stopping the loop) after t.
func rollupFixture(t *testing.T, mode, fleetMode string, shutdown context.Context) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldShutdown := shutdownCtx
	t.Cleanup(func() { shutdownCtx = oldShutdown })
	cfg = config.DefaultConfig()
	cfg.Mode = mode
	cfg.Briefing.FleetMode = fleetMode
	shutdownCtx = shutdown
}

func rollupDone() chan struct{} {
	briefingRollupLoop.mu.Lock()
	defer briefingRollupLoop.mu.Unlock()
	return briefingRollupLoop.done
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// liveRollupLoops counts the goroutines inside runBriefingRollup, waiting
// up to 2 s for a stopped loop's goroutine to leave its deferred close.
func liveRollupLoops(t *testing.T, want int) int {
	t.Helper()
	buf := make([]byte, 1<<22)
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.Stack(buf, true)
		got := strings.Count(string(buf[:n]), ".runBriefingRollup(")
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// State transition: a second start stops the first loop before its own.
func TestBriefingRollupStartReplacesTheRunningLoop(t *testing.T) {
	rollupFixture(t, "fleet", config.BriefingFleetRollup, nil)
	startBriefingRollup()
	first := rollupDone()
	if first == nil || isClosed(first) {
		t.Fatal("the first start did not run a roll-up loop")
	}
	startBriefingRollup()
	second := rollupDone()
	if !isClosed(first) {
		t.Fatal("the first roll-up loop still runs after the second start")
	}
	if second == nil || second == first || isClosed(second) {
		t.Fatal("the second start did not run its own roll-up loop")
	}
	if got := liveRollupLoops(t, 1); got != 1 {
		t.Fatalf("%d roll-up loops run, want 1", got)
	}
	stopBriefingRollup()
	if !isClosed(second) || rollupDone() != nil {
		t.Fatal("stop left the roll-up loop running")
	}
	stopBriefingRollup() // nil state: a second stop is a no-op
	if got := liveRollupLoops(t, 0); got != 0 {
		t.Fatalf("%d roll-up loops run after stop, want 0", got)
	}
}

// Invalid input for the loop: standalone mode and per-database briefings
// start none.
func TestBriefingRollupOffOutsideRollupMode(t *testing.T) {
	for _, tc := range []struct{ mode, fleetMode string }{
		{"standalone", config.BriefingFleetRollup},
		{"fleet", config.BriefingFleetPerDatabase},
	} {
		rollupFixture(t, tc.mode, tc.fleetMode, nil)
		startBriefingRollup()
		if done := rollupDone(); done != nil {
			t.Fatalf("%s/%s started a roll-up loop", tc.mode, tc.fleetMode)
		}
	}
}

// Error propagation: the shutdown context ends the loop by itself.
func TestBriefingRollupEndsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rollupFixture(t, "fleet", config.BriefingFleetRollup, ctx)
	startBriefingRollup()
	done := rollupDone()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the roll-up loop outlived the shutdown context by 5 s")
	}
	stopBriefingRollup()
	if rollupDone() != nil {
		t.Fatal("stop after shutdown left loop state behind")
	}
}

// Concurrent access: starts racing each other leave exactly one loop.
func TestBriefingRollupConcurrentStartsLeaveOneLoop(t *testing.T) {
	rollupFixture(t, "fleet", config.BriefingFleetRollup, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startBriefingRollup()
		}()
	}
	wg.Wait()
	if got := liveRollupLoops(t, 1); got != 1 {
		t.Fatalf("%d roll-up loops run after 8 concurrent starts, want 1", got)
	}
	stopBriefingRollup()
	if got := liveRollupLoops(t, 0); got != 0 {
		t.Fatalf("%d roll-up loops run after stop, want 0", got)
	}
}
