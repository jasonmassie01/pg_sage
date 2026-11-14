package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleet-31: retryFailedInstances against a real meta database. The
// unreachable members point at 127.0.0.1:1, which refuses at once.

// shortStartupBackoff makes a refused connect fail in milliseconds.
func shortStartupBackoff(t *testing.T, d time.Duration) {
	t.Helper()
	old := startupBackoff
	startupBackoff = d
	t.Cleanup(func() { startupBackoff = old })
}

// registerFailedRecord stores input and publishes it as a failed instance.
func registerFailedRecord(
	t *testing.T, state *metaDBState, input store.DatabaseInput, userID int,
) *fleet.DatabaseInstance {
	t.Helper()
	ctx := context.Background()
	id, err := state.Store.Create(ctx, input, userID)
	if err != nil {
		t.Fatalf("create %s: %v", input.Name, err)
	}
	deleteDatabaseOnCleanup(t, state, id)
	rec, err := state.Store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", input.Name, err)
	}
	registerFailedInstance(*rec, "fixture disconnected")
	return fleetMgr.GetInstance(input.Name)
}

func unreachableInput(input store.DatabaseInput, name string) store.DatabaseInput {
	input.Name, input.Host, input.Port = name, "127.0.0.1", 1
	return input
}

// Error propagation and state transitions: the unreachable member stays
// failed with today's log line; the reachable one goes failed -> active,
// and an active member is not retried on the next tick.
func TestRetryFailedInstancesIsolatesUnreachableMember(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	shortStartupBackoff(t, time.Millisecond)
	good := registerFailedRecord(t, state, input, userID)
	bad := registerFailedRecord(t, state, unreachableInput(input, "unreachable-b"), userID)

	out := captureStderr(t, func() { retryFailedInstances(state) })
	for _, want := range []string{
		`[INFO] [reconnect] attempting reconnect for "managed-a"`,
		`[INFO] [reconnect] attempting reconnect for "unreachable-b"`,
		`[WARN] [reconnect] db "unreachable-b": still unreachable:`,
		`[INFO] [reconnect] db "managed-a": reconnected successfully`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	active := fleetMgr.GetInstance("managed-a")
	if active == good || active.SnapshotStatus().Error != "" {
		t.Fatalf("reachable member not reactivated: error %q", active.SnapshotStatus().Error)
	}
	assertManagedRuntime(t, active, good.DatabaseID, "managed-a")
	if fleetMgr.GetInstance("unreachable-b") != bad || bad.Pool != nil ||
		bad.SnapshotStatus().Error != "fixture disconnected" {
		t.Fatalf("unreachable member left the retry set: %+v", bad.SnapshotStatus())
	}

	out = captureStderr(t, func() { retryFailedInstances(state) })
	if strings.Contains(out, `attempting reconnect for "managed-a"`) {
		t.Fatalf("an active member was retried:\n%s", out)
	}
	if !strings.Contains(out, `attempting reconnect for "unreachable-b"`) {
		t.Fatalf("the failed member left the retry set:\n%s", out)
	}
	if fleetMgr.GetInstance("managed-a") != active {
		t.Fatal("the active member was replaced on the next tick")
	}
}

// Boundary: N failed members get exactly N attempts per tick.
func TestRetryFailedInstancesAttemptsEachFailedMemberOnce(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	shortStartupBackoff(t, time.Millisecond)
	const n = 3
	for i := range n {
		registerFailedRecord(t, state, unreachableInput(input, fmt.Sprintf("down-%d", i)),
			userID)
	}
	for tick := 1; tick <= 2; tick++ {
		out := captureStderr(t, func() { retryFailedInstances(state) })
		if got := strings.Count(out, "attempting reconnect for"); got != n {
			t.Fatalf("tick %d: %d attempts, want %d:\n%s", tick, got, n, out)
		}
		for i := range n {
			name := fmt.Sprintf("down-%d", i)
			line := fmt.Sprintf(`db %q: still unreachable:`, name)
			if strings.Count(out, line) != 1 {
				t.Fatalf("tick %d: %s not attempted once:\n%s", tick, name, out)
			}
		}
	}
	if fleetMgr.InstanceCount() != n {
		t.Fatalf("instances = %d, want %d", fleetMgr.InstanceCount(), n)
	}
}

// Concurrency: cancelling the loop while a retry is in flight lets that
// retry finish, then the loop returns and no goroutine is left behind.
// CI runs this under -race.
func TestFleetReconnectLoopCancelledMidRetry(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	// Four backoffs of 50, 100, 200, 400 ms: the retry is still running
	// when the test cancels at 150 ms.
	shortStartupBackoff(t, 50*time.Millisecond)
	oldInterval := fleetReconnectInterval
	fleetReconnectInterval = 20 * time.Millisecond
	t.Cleanup(func() { fleetReconnectInterval = oldInterval })
	registerFailedRecord(t, state, unreachableInput(input, "unreachable-c"), userID)
	baseline := runtime.NumGoroutine()
	out := captureStderr(t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			fleetReconnectLoop(ctx, state)
			close(done)
		}()
		time.Sleep(150 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * metaDBTimeout):
			t.Error("fleetReconnectLoop did not return after cancel")
		}
	})
	if !strings.Contains(out, `attempting reconnect for "unreachable-c"`) {
		t.Fatalf("no retry was in flight:\n%s", out)
	}
	if n := settledGoroutines(baseline, 5*time.Second); n > baseline {
		t.Fatalf("goroutines %d after cancel, want at most %d", n, baseline)
	}
}
