package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// fleet-09: a fleet-wide stop is a latch on the fleet, not only on the
// databases registered when it was pressed.

func newFleetMember(name string) *DatabaseInstance {
	return &DatabaseInstance{
		Name:     name,
		Config:   config.DatabaseConfig{Name: name},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true, LastSeen: time.Now()},
	}
}

func assertLatchedBy(t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance, actor string) {
	t.Helper()
	mgr.mu.RLock()
	stopped, by, at := inst.Stopped, inst.StoppedBy, inst.StoppedAt
	mgr.mu.RUnlock()
	if !stopped || by != actor || at.IsZero() {
		t.Fatalf("%s: stopped=%v by=%q at=%v, want stopped by %q",
			inst.Name, stopped, by, at, actor)
	}
	if inst.Executor != nil && inst.Executor.ExecutorEnabled() {
		t.Fatalf("%s: executor gate open while stopped", inst.Name)
	}
}

func assertRunning(t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance) {
	t.Helper()
	if mgr.InstanceStopped(inst) {
		t.Fatalf("%s: stopped, want running", inst.Name)
	}
	if inst.Executor != nil && !inst.Executor.ExecutorEnabled() {
		t.Fatalf("%s: executor gate closed while running", inst.Name)
	}
}

func TestFleetStopLatchesDatabaseRegisteredAfterStopAll(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	late := newFleetMember("late")

	h.mgr.RegisterInstance(late)

	assertLatchedBy(t, h.mgr, late, "op@example.com")
	if value, ok := h.persistedState("late"); !ok || !value {
		t.Fatalf("late durable flag = %v (written %v), want true", value, ok)
	}
}

func TestFleetStopLatchesPublishRegistration(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	late := newFleetMember("late")

	err := h.mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.PublishRegistration(late)
	})

	if err != nil {
		t.Fatalf("publish registration: %v", err)
	}
	assertLatchedBy(t, h.mgr, late, "op@example.com")
	if value, ok := h.persistedState("late"); !ok || !value {
		t.Fatalf("late durable flag = %v (written %v), want true", value, ok)
	}
}

// A resume-all that cannot clear every database keeps the fleet latch on,
// so a reconnect of a database it did release comes back stopped.
func TestFleetStopLatchesPublishReplacement(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mu.Lock()
	h.failing["bravo"] = true
	h.mu.Unlock()
	if _, err := h.mgr.ResumeStrict("", "op@example.com"); err == nil {
		t.Fatal("resume all must report bravo's failed write")
	}
	old := h.mgr.GetInstance("alpha")
	assertRunning(t, h.mgr, old)
	h.mu.Lock()
	delete(h.persisted, "alpha")
	h.mu.Unlock()
	candidate := newFleetMember("alpha")

	err := h.mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.PublishReplacement("alpha", old, candidate)
	})

	if err != nil {
		t.Fatalf("publish replacement: %v", err)
	}
	assertLatchedBy(t, h.mgr, candidate, "op@example.com")
	if value, ok := h.persistedState("alpha"); !ok || !value {
		t.Fatalf("alpha durable flag = %v (written %v), want true", value, ok)
	}
}

func TestFleetStopLatchesRegistrationWithoutExecutor(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	failed := &DatabaseInstance{Name: "unreachable", Status: &InstanceStatus{}}

	h.mgr.RegisterInstance(failed)

	assertLatchedBy(t, h.mgr, failed, "op@example.com")
	if _, ok := h.persistedState("unreachable"); ok {
		t.Fatal("an instance without an executor has no flag to write")
	}
	status := h.mgr.FleetStatus()
	if status.Summary.EmergencyStoppedCount != 2 {
		t.Fatalf("stopped count = %d, want 2", status.Summary.EmergencyStoppedCount)
	}
}

func TestFleetStopRefusesPerDatabaseResume(t *testing.T) {
	h := newStopHarness(t, []string{"db1", "db2"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}

	resumed, err := h.mgr.ResumeStrict("db1", "other@example.com")

	if !errors.Is(err, ErrFleetStopped) {
		t.Fatalf("error = %v, want ErrFleetStopped", err)
	}
	if resumed != 0 {
		t.Fatalf("resumed = %d, want 0", resumed)
	}
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("db1"), "op@example.com")
	if value, _ := h.persistedState("db1"); !value {
		t.Fatal("a refused resume must not clear db1's durable flag")
	}
}

func TestFleetStopResumeAllClearsLatch(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if !h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("summary must report the fleet stop")
	}
	if _, err := h.mgr.ResumeStrict("", "op@example.com"); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	late := newFleetMember("late")

	h.mgr.RegisterInstance(late)

	assertRunning(t, h.mgr, late)
	if _, ok := h.persistedState("late"); ok {
		t.Fatal("a registration after resume-all must not write a stop")
	}
	summary := h.mgr.FleetStatus().Summary
	if summary.FleetStopped || summary.FleetStoppedBy != "" || summary.FleetStoppedAt != nil {
		t.Fatalf("summary = %+v, want the fleet latch cleared", summary)
	}
	if _, err := h.mgr.ResumeStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("per-database resume after resume-all: %v", err)
	}
}

func TestFleetStopPartialResumeKeepsLatch(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mu.Lock()
	h.failing["bravo"] = true
	h.mu.Unlock()

	_, err := h.mgr.ResumeStrict("", "op@example.com")

	var stopErr *EmergencyStopError
	if !errors.As(err, &stopErr) || stopErr.Failed["bravo"] == nil {
		t.Fatalf("error = %v, want EmergencyStopError naming bravo", err)
	}
	summary := h.mgr.FleetStatus().Summary
	if !summary.FleetStopped || summary.FleetStoppedBy != "op@example.com" {
		t.Fatalf("summary = %+v, want the fleet latch kept", summary)
	}
	if _, err := h.mgr.ResumeStrict("bravo", "op@example.com"); !errors.Is(err, ErrFleetStopped) {
		t.Fatalf("per-database resume = %v, want ErrFleetStopped", err)
	}
}

func TestFleetStopSummaryCountsLaterRegistration(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	before := time.Now()
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mgr.RegisterInstance(newFleetMember("late"))

	summary := h.mgr.FleetStatus().Summary

	if !summary.FleetStopped || !summary.EmergencyStopped {
		t.Fatalf("summary = %+v, want fleet stopped", summary)
	}
	if summary.FleetStoppedBy != "op@example.com" {
		t.Fatalf("fleet stopped by %q, want op@example.com", summary.FleetStoppedBy)
	}
	if summary.FleetStoppedAt == nil || summary.FleetStoppedAt.Before(before) {
		t.Fatalf("fleet stopped at %v, want at or after %v", summary.FleetStoppedAt, before)
	}
	if summary.EmergencyStoppedCount != 3 || summary.TotalDatabases != 3 {
		t.Fatalf("stopped %d of %d, want 3 of 3",
			summary.EmergencyStoppedCount, summary.TotalDatabases)
	}
}

func TestPerDatabaseStopDoesNotLatchFleet(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	if _, err := h.mgr.EmergencyStopStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	late := newFleetMember("late")

	h.mgr.RegisterInstance(late)

	assertRunning(t, h.mgr, late)
	if h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("a single-database stop must not report a fleet stop")
	}
	if _, err := h.mgr.ResumeStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("per-database resume of a per-database stop: %v", err)
	}
}

// Concurrent access: stop-all races registrations; whichever wins the
// manager lock, every database ends stopped and saved as stopped.
func TestFleetStopRaceWithRegistrations(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		h := newStopHarness(t, []string{"seed"})
		var wg sync.WaitGroup
		for g := 0; g < 50; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				if g%10 == 0 {
					_, _ = h.mgr.EmergencyStopStrict("", "op@example.com")
					return
				}
				h.mgr.RegisterInstance(newFleetMember(fmt.Sprintf("db%02d", g)))
			}(g)
		}
		wg.Wait()
		for name, inst := range h.mgr.Instances() {
			assertLatchedBy(t, h.mgr, inst, "op@example.com")
			if value, ok := h.persistedState(name); !ok || !value {
				t.Fatalf("iter %d: %s durable flag = %v (written %v)", iter, name, value, ok)
			}
		}
		if got := len(h.mgr.Instances()); got != 46 {
			t.Fatalf("iter %d: %d instances, want 46", iter, got)
		}
	}
}

func TestEmergencyStopListsCancelledActions(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	h.mgr.cancelInFlight = func(inst *DatabaseInstance) ([]int64, error) {
		// The cancel waits on running statements: it must not hold m.mu.
		done := make(chan struct{})
		go func() { _ = h.mgr.FleetStatus(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("%s: in-flight cancel ran under the manager lock", inst.Name)
		}
		if inst.Name == "alpha" {
			return []int64{41, 42}, nil
		}
		return []int64{}, nil
	}

	result, err := h.mgr.EmergencyStopDetailed("", "op@example.com")

	if err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if result.Changed != 2 {
		t.Fatalf("changed = %d, want 2", result.Changed)
	}
	if fmt.Sprint(result.Cancelled["alpha"]) != "[41 42]" || len(result.Cancelled["bravo"]) != 0 {
		t.Fatalf("cancelled = %v, want alpha [41 42] and bravo none", result.Cancelled)
	}
	if len(result.CancelFailures) != 0 {
		t.Fatalf("cancel failures = %v, want none", result.CancelFailures)
	}
}

func TestEmergencyStopReportsCancelFailureButStaysStopped(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	h.mgr.cancelInFlight = func(*DatabaseInstance) ([]int64, error) {
		return nil, errors.New("cancel request refused")
	}

	result, err := h.mgr.EmergencyStopDetailed("alpha", "op@example.com")

	if err != nil {
		t.Fatalf("stop: %v (a cancel failure is reported, not a failed stop)", err)
	}
	if result.CancelFailures["alpha"] != "cancel request refused" {
		t.Fatalf("cancel failures = %v, want alpha's error", result.CancelFailures)
	}
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	if value, _ := h.persistedState("alpha"); !value {
		t.Fatal("the durable flag must be written despite the cancel failure")
	}
}

func TestEmergencyStopDetailedUnknownDatabase(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	called := false
	h.mgr.cancelInFlight = func(*DatabaseInstance) ([]int64, error) {
		called = true
		return nil, nil
	}

	result, err := h.mgr.EmergencyStopDetailed("missing", "op@example.com")

	if !errors.Is(err, ErrDatabaseNotFound) || result.Changed != 0 || called {
		t.Fatalf("result=%+v err=%v called=%v, want not found and no cancel",
			result, err, called)
	}
	if h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("an unknown target must not latch the fleet")
	}
}
