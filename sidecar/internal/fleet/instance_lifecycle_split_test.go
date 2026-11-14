package fleet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Pins of the instance lifecycle before and after it moved out of
// types.go (fleet-30). Invalid input and integration do not apply: the
// move adds no input and the lifecycle code touches no database.

type teardownCounts struct{ cancel, shutdown, close atomic.Int32 }

func countedInstance(name string, id int, c *teardownCounts) *DatabaseInstance {
	inst := wave2LifecycleInstance(name, id)
	inst.Cancel = func() { c.cancel.Add(1) }
	inst.ExecutorShutdown = func(context.Context) error {
		c.shutdown.Add(1)
		return nil
	}
	inst.PoolClose = func() { c.close.Add(1) }
	return inst
}

func assertTornDownOnce(t *testing.T, label string, c *teardownCounts) {
	t.Helper()
	if c.cancel.Load() != 1 || c.shutdown.Load() != 1 || c.close.Load() != 1 {
		t.Fatalf("%s teardown cancel/shutdown/close = %d/%d/%d, want 1/1/1", label,
			c.cancel.Load(), c.shutdown.Load(), c.close.Load())
	}
}

func assertTeardownDone(t *testing.T, label string, inst *DatabaseInstance) {
	t.Helper()
	inst.lifecycleMu.Lock()
	started, done := inst.lifecycleStarted, inst.lifecycleDone
	inst.lifecycleMu.Unlock()
	if !started || done == nil {
		t.Fatalf("%s teardown never started", label)
	}
	waitWave2Signal(t, done, label+" teardown done")
}

func primaryOf(mgr *DatabaseManager) string {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return mgr.primaryName
}

func TestRemoveInstanceMissingReturnsFalse(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	if mgr.RemoveInstance("missing") {
		t.Fatal("RemoveInstance(missing) = true on an empty manager")
	}
	var c teardownCounts
	mgr.RegisterInstance(countedInstance("orders", 1, &c))
	if mgr.RemoveInstance("missing") {
		t.Fatal("RemoveInstance(missing) = true with another database registered")
	}
	if mgr.InstanceCount() != 1 || c.close.Load() != 0 {
		t.Fatalf("count=%d close=%d after a missing remove, want 1/0",
			mgr.InstanceCount(), c.close.Load())
	}
}

func TestShutdownInstanceNilIsNoop(t *testing.T) {
	//nolint:staticcheck // a nil context is the legacy caller's input
	if err := ShutdownInstance(nil, nil); err != nil {
		t.Fatalf("ShutdownInstance(nil, nil) = %v, want nil", err)
	}
	if err := cleanupRejectedCandidate(nil); err != nil {
		t.Fatalf("cleanupRejectedCandidate(nil) = %v, want nil", err)
	}
}

func TestRemoveInstanceContextCancelledKeepsInstance(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	var c teardownCounts
	mgr.RegisterInstance(countedInstance("orders", 1, &c))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mgr.RemoveInstanceContext(ctx, "orders")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("remove with a cancelled ctx = %v, want context.Canceled", err)
	}
	if mgr.GetInstance("orders") == nil || c.close.Load() != 0 {
		t.Fatal("a cancelled remove detached or tore down the instance")
	}
}

func TestLifecycleAddRejectReplaceRemove(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	var oldC, rejC, newC teardownCounts
	old := countedInstance("orders", 7, &oldC)
	mgr.RegisterInstance(old)
	if mgr.InstanceCount() != 1 || primaryOf(mgr) != "orders" {
		t.Fatalf("after add: count=%d primary=%q", mgr.InstanceCount(), primaryOf(mgr))
	}
	rejected := countedInstance("orders", 7, &rejC)
	healthErr := errors.New("identity mismatch")
	err := mgr.ReplaceInstance(context.Background(), "orders", rejected,
		func(context.Context, *DatabaseInstance) error { return healthErr })
	if !errors.Is(err, healthErr) {
		t.Fatalf("rejected replace = %v, want %v", err, healthErr)
	}
	assertTeardownDone(t, "rejected candidate", rejected)
	assertTornDownOnce(t, "rejected candidate", &rejC)
	if mgr.InstanceCount() != 1 || mgr.GetInstance("orders") != old || oldC.close.Load() != 0 {
		t.Fatal("a rejected candidate displaced or tore down the active generation")
	}
	next := countedInstance("orders", 7, &newC)
	err = mgr.ReplaceInstance(context.Background(), "orders", next,
		func(context.Context, *DatabaseInstance) error { return nil })
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	assertTeardownDone(t, "old generation", old)
	assertTornDownOnce(t, "old generation", &oldC)
	if mgr.InstanceCount() != 1 || mgr.GetInstance("orders") != next {
		t.Fatal("replacement did not publish the new generation")
	}
	if !mgr.RemoveInstance("orders") {
		t.Fatal("RemoveInstance(orders) = false")
	}
	assertTeardownDone(t, "new generation", next)
	assertTornDownOnce(t, "new generation", &newC)
	if mgr.InstanceCount() != 0 || primaryOf(mgr) != "" || mgr.RemoveInstance("orders") {
		t.Fatalf("after remove: count=%d primary=%q", mgr.InstanceCount(), primaryOf(mgr))
	}
}

// RemoveInstance racing a replacement commit and status writers: whichever
// wins, each generation is torn down exactly once and none is left behind.
func TestRemoveRacesReplaceAndUpdateStatus(t *testing.T) {
	for i := 0; i < 50; i++ {
		raceRemoveAgainstReplace(t)
	}
}

func raceRemoveAgainstReplace(t *testing.T) {
	t.Helper()
	mgr := NewManager(&config.Config{Mode: "fleet"})
	var oldC, newC teardownCounts
	old := countedInstance("orders", 7, &oldC)
	next := countedInstance("orders", 7, &newC)
	mgr.RegisterInstance(old)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); <-start; mgr.RemoveInstance("orders") }()
	go func() {
		defer wg.Done()
		<-start
		_ = mgr.ReplaceInstanceIfCurrent(context.Background(), "orders", old, next,
			func(context.Context, *DatabaseInstance) error { return nil })
	}()
	for _, inst := range []*DatabaseInstance{old, next} {
		go func(inst *DatabaseInstance) {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				inst.UpdateStatus(func(s *InstanceStatus) { s.FindingsOpen++ })
			}
		}(inst)
	}
	close(start)
	wg.Wait()
	if mgr.RemoveInstance("orders") {
		t.Fatal("a generation survived the remove/replace race")
	}
	assertTornDownOnce(t, "old generation", &oldC)
	assertTornDownOnce(t, "candidate", &newC)
	if mgr.InstanceCount() != 0 {
		t.Fatalf("instances left after remove/replace race: %d", mgr.InstanceCount())
	}
	if old.SnapshotStatus().FindingsOpen != 20 || next.SnapshotStatus().FindingsOpen != 20 {
		t.Fatal("concurrent UpdateStatus lost writes")
	}
}
