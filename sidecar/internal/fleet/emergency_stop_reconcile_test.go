package fleet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// durableFlags is the per-database sage.config emergency_stop the
// reconcile reads; a missing entry reads as the bootstrap seed 'false'.
type durableFlags struct {
	mu      sync.Mutex
	state   map[string]executor.EmergencyStopState
	readErr map[string]error
}

func useDurableFlags(mgr *DatabaseManager) *durableFlags {
	d := &durableFlags{
		state:   map[string]executor.EmergencyStopState{},
		readErr: map[string]error{},
	}
	mgr.readStop = func(
		_ context.Context, inst *DatabaseInstance,
	) (executor.EmergencyStopState, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.readErr[inst.Name]; err != nil {
			return executor.EmergencyStopState{}, err
		}
		return d.state[inst.Name], nil
	}
	return d
}

func (d *durableFlags) set(name string, state executor.EmergencyStopState, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state[name] = state
	d.readErr[name] = err
}

func countCancels(mgr *DatabaseManager) *atomic.Int32 {
	var n atomic.Int32
	mgr.cancelInFlight = func(*DatabaseInstance) ([]int64, error) {
		n.Add(1)
		return nil, nil
	}
	return &n
}

// The fleet latch, not the seeded 'false' a new member reads back, decides
// whether a fleet-stopped member may run.
func TestReconcileKeepsFleetLatchedMemberStoppedThroughFailedPersist(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	useDurableFlags(h.mgr)
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mu.Lock()
	h.failing["late"] = true
	h.mu.Unlock()
	late := newFleetMember("late")
	h.mgr.RegisterInstance(late)
	ctx := context.Background()

	for tick := 0; tick < 3; tick++ {
		h.mgr.ReconcileStops(ctx)
		assertLatchedBy(t, h.mgr, late, "op@example.com")
	}
	h.mu.Lock()
	delete(h.failing, "late")
	h.mu.Unlock()
	h.mgr.ReconcileStops(ctx)

	assertLatchedBy(t, h.mgr, late, "op@example.com")
	if value, ok := h.persistedState("late"); !ok || !value {
		t.Fatalf("late durable flag = %v (written %v), want the retried stop saved", value, ok)
	}
	if _, err := h.mgr.ResumeStrict("", "op@example.com"); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	h.mgr.ReconcileStops(ctx)
	assertRunning(t, h.mgr, late)
}

func TestReconcileLatchesSQLStopWithinOneTick(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	flags := useDurableFlags(h.mgr)
	cancels := countCancels(h.mgr)
	at := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	flags.set("alpha",
		executor.EmergencyStopState{Stopped: true, UpdatedBy: "dba", UpdatedAt: at}, nil)

	h.mgr.ReconcileStops(context.Background())

	alpha := h.mgr.GetInstance("alpha")
	assertLatchedBy(t, h.mgr, alpha, "dba")
	if !alpha.StoppedAt.Equal(at) {
		t.Fatalf("stopped at %v, want the row's %v", alpha.StoppedAt, at)
	}
	if cancels.Load() != 1 {
		t.Fatalf("in-flight cancels = %d, want 1 (alpha only)", cancels.Load())
	}
	assertRunning(t, h.mgr, h.mgr.GetInstance("bravo"))
	if got := h.mgr.FleetStatus().Summary.EmergencyStoppedCount; got != 1 {
		t.Fatalf("stopped count = %d, want 1", got)
	}
	h.mgr.ReconcileStops(context.Background())
	if cancels.Load() != 1 {
		t.Fatalf("a second tick cancelled again: %d cancels", cancels.Load())
	}
}

func TestReconcileReadErrorLatches(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	flags := useDurableFlags(h.mgr)
	flags.set("alpha", executor.EmergencyStopState{}, errors.New("statement timeout"))

	h.mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), executor.EmergencyStopActorSystem)
	flags.set("alpha", executor.EmergencyStopState{}, nil)
	h.mgr.ReconcileStops(context.Background())
	assertRunning(t, h.mgr, h.mgr.GetInstance("alpha"))
}

func TestReconcileReleasesSQLResume(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	flags := useDurableFlags(h.mgr)
	if _, err := h.mgr.EmergencyStopStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	flags.set("alpha", executor.EmergencyStopState{Stopped: true, UpdatedBy: "op@example.com"}, nil)
	h.mgr.ReconcileStops(context.Background())
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")

	flags.set("alpha", executor.EmergencyStopState{UpdatedBy: "dba"}, nil)
	h.mgr.ReconcileStops(context.Background())

	assertRunning(t, h.mgr, h.mgr.GetInstance("alpha"))
}

func TestReconcileDoesNotReleasePendingAPIStop(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	useDurableFlags(h.mgr)
	inst := h.mgr.GetInstance("alpha")
	// The state an API stop is in after it latched memory and before it
	// took the transition lock to write the flag: the durable flag still
	// reads the old 'false'.
	h.mgr.mu.Lock()
	latchLocked(inst, "op@example.com", time.Now())
	inst.stopsPending++
	h.mgr.mu.Unlock()

	h.mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, h.mgr, inst, "op@example.com")
}

// End to end: a stop blocked writing its flag, with a reconcile queued
// behind it, still stands after both finish.
func TestReconcileQueuedBehindAPIStopKeepsIt(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	flags := useDurableFlags(h.mgr)
	writing := make(chan struct{})
	release := make(chan struct{})
	h.mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		close(writing)
		<-release
		flags.set(inst.Name, executor.EmergencyStopState{Stopped: stopped, UpdatedBy: actor}, nil)
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.mgr.EmergencyStopStrict("alpha", "op@example.com")
		done <- err
	}()
	<-writing
	reconciled := make(chan struct{})
	go func() { h.mgr.ReconcileStops(context.Background()); close(reconciled) }()

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}
	<-reconciled
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
}

// A stop that could not be saved is retried and never released by a
// read of the old 'false'.
func TestReconcileKeepsUnsavedStopAndRetries(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"}, "alpha")
	flags := useDurableFlags(h.mgr)
	harnessPersist := h.mgr.persistStop
	h.mgr.persistStop = func(
		ctx context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		if err := harnessPersist(ctx, inst, stopped, actor); err != nil {
			return err
		}
		flags.set(inst.Name, executor.EmergencyStopState{Stopped: stopped, UpdatedBy: actor}, nil)
		return nil
	}
	if _, err := h.mgr.EmergencyStopStrict("alpha", "op@example.com"); err == nil {
		t.Fatal("expected alpha's persist to fail")
	}

	h.mgr.ReconcileStops(context.Background())
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	if _, ok := h.persistedState("alpha"); ok {
		t.Fatal("the persist is still failing; nothing should be saved")
	}
	h.mu.Lock()
	delete(h.failing, "alpha")
	h.mu.Unlock()
	h.mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	if value, ok := h.persistedState("alpha"); !ok || !value {
		t.Fatalf("alpha durable flag = %v (written %v), want the retry saved", value, ok)
	}
}

// Nil/empty: instances without an executor or a pool have no durable flag
// to reconcile; a reconcile of an empty fleet is a no-op.
func TestReconcileSkipsInstancesWithoutDurableFlag(t *testing.T) {
	NewManager(nil).ReconcileStops(context.Background())
	h := newStopHarness(t, []string{"pooless"})
	agent := &DatabaseInstance{Name: "agent", Status: &InstanceStatus{}}
	h.mgr.RegisterInstance(agent)
	if _, err := h.mgr.EmergencyStopStrict("agent", "op@example.com"); err != nil {
		t.Fatalf("stop agent: %v", err)
	}

	h.mgr.ReconcileStops(context.Background())

	assertRunning(t, h.mgr, h.mgr.GetInstance("pooless"))
	assertLatchedBy(t, h.mgr, agent, "op@example.com")
}

func TestReconcileConcurrentWithStopAll(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo", "charlie"})
	useDurableFlags(h.mgr)
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			if g == 7 {
				_, _ = h.mgr.EmergencyStopStrict("", "op@example.com")
				return
			}
			h.mgr.ReconcileStops(context.Background())
		}(g)
	}
	wg.Wait()
	h.mgr.ReconcileStops(context.Background())

	for _, inst := range h.mgr.Instances() {
		assertLatchedBy(t, h.mgr, inst, "op@example.com")
	}
}

func TestRunStopReconcileTicksUntilCancelled(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	flags := useDurableFlags(h.mgr)
	flags.set("alpha", executor.EmergencyStopState{Stopped: true, UpdatedBy: "dba"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { h.mgr.RunStopReconcile(ctx, 5*time.Millisecond); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for !h.mgr.InstanceStopped(h.mgr.GetInstance("alpha")) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "dba")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunStopReconcile did not return after its context was cancelled")
	}
}

// A fleet-latched database whose flag reads back 'false' (a per-database
// SQL resume, or the bootstrap seed) is stopped again on disk too.
func TestReconcileRewritesFleetLatchedFalseFlag(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	useDurableFlags(h.mgr)
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mu.Lock()
	delete(h.persisted, "alpha")
	h.mu.Unlock()

	h.mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	if value, ok := h.persistedState("alpha"); !ok || !value {
		t.Fatalf("alpha durable flag = %v (written %v), want rewritten true", value, ok)
	}
}
