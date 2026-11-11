package sre

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06: a coordinator tick that cannot reach its store is a
// failed run of the sre loop.

// No concurrent access tests here: the tracker's locking is tested in
// selfbudget, and a coordinator runs one tick at a time.

func unreachableCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(CoordinatorDeps{Store: unreachableStore(t),
		Runner: newScriptedRunner(), Config: DefaultCoordinatorConfig("loop:health"),
		LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return c
}

// Error propagation: the bind failure reaches the tick's caller.
func TestTick_StoreOutageReturnsTheError(t *testing.T) {
	c := unreachableCoordinator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var last time.Time
	err := c.tick(ctx, &last)
	if !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("tick during an outage = %v, want ErrMetadataUnavailable", err)
	}
	// Still degraded: the next tick fails on the store check, not silently.
	if err := c.tick(ctx, &last); err == nil {
		t.Fatal("a tick against a dead store returned no error")
	}
}

// A cancelled tick did nothing and is no error.
func TestTick_CancelledIsNoError(t *testing.T) {
	c := unreachableCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var last time.Time
	if err := c.tick(ctx, &last); err != nil {
		t.Fatalf("cancelled tick = %v, want nil", err)
	}
}

// The coordinator loop records each tick in its tracker.
func TestRun_TracksFailedTicks(t *testing.T) {
	c := unreachableCoordinator(t)
	c.loops = selfbudget.NewLoops()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Run(ctx)
	s := c.loops.Snapshot()["sre"]
	if s.Runs < 1 || s.Errors < 1 || s.Errors > s.Runs || !s.LastSuccess.IsZero() {
		t.Fatalf("sre loop = %+v, want failed runs and no last success", s)
	}
}
