package fleet

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// stopHarness builds a manager whose instances own real executors (nil
// pools) and whose persistence hook fails for the named databases.
type stopHarness struct {
	mgr       *DatabaseManager
	mu        sync.Mutex
	persisted map[string]bool
	failing   map[string]bool
}

func newStopHarness(t *testing.T, names []string, failing ...string) *stopHarness {
	t.Helper()
	h := &stopHarness{
		mgr:       NewManager(&config.Config{Mode: "fleet"}),
		persisted: map[string]bool{},
		failing:   map[string]bool{},
	}
	for _, name := range failing {
		h.failing[name] = true
	}
	h.mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, _ string,
	) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.failing[inst.Name] {
			return fmt.Errorf("sage.config unwritable on %s", inst.Name)
		}
		h.persisted[inst.Name] = stopped
		return nil
	}
	for _, name := range names {
		h.mgr.RegisterInstance(&DatabaseInstance{
			Name:     name,
			Config:   config.DatabaseConfig{Name: name},
			Executor: newGateExecutor(),
			Status:   &InstanceStatus{Connected: true, LastSeen: time.Now()},
		})
	}
	return h
}

func newGateExecutor() *executor.Executor {
	cfg := &config.Config{Trust: config.TrustConfig{Level: "autonomous"}}
	return executor.New(nil, cfg, time.Now(), func(string, string, ...any) {})
}

func (h *stopHarness) persistedState(name string) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	value, ok := h.persisted[name]
	return value, ok
}

func TestEmergencyStopAll_FailingDatabaseDoesNotAbortLoop(t *testing.T) {
	for i := 0; i < 100; i++ {
		names := []string{"alpha", "bravo", "charlie"}
		rand.Shuffle(len(names), func(a, b int) { names[a], names[b] = names[b], names[a] })
		h := newStopHarness(t, names, "bravo")

		changed, err := h.mgr.EmergencyStopStrict("", "test")

		if changed != 3 {
			t.Fatalf("iteration %d: changed = %d, want 3", i, changed)
		}
		var stopErr *EmergencyStopError
		if !errors.As(err, &stopErr) {
			t.Fatalf("iteration %d: error %v is not *EmergencyStopError", i, err)
		}
		if len(stopErr.Failed) != 1 || stopErr.Failed["bravo"] == nil {
			t.Fatalf("iteration %d: failed = %v, want only bravo", i, stopErr.Failed)
		}
		for _, name := range names {
			inst := h.mgr.GetInstance(name)
			if !h.mgr.InstanceStopped(inst) {
				t.Fatalf("iteration %d: %s not stopped in memory", i, name)
			}
			if inst.Executor.ExecutorEnabled() {
				t.Fatalf("iteration %d: %s executor still enabled", i, name)
			}
		}
		for _, name := range []string{"alpha", "charlie"} {
			if value, ok := h.persistedState(name); !ok || !value {
				t.Fatalf("iteration %d: %s persisted=%v ok=%v", i, name, value, ok)
			}
		}
		if _, ok := h.persistedState("bravo"); ok {
			t.Fatalf("iteration %d: bravo recorded as persisted", i)
		}
	}
}

func TestEmergencyStopAll_AgentDatabaseWithoutExecutorIsSkipped(t *testing.T) {
	h := newStopHarness(t, []string{"prod"})
	h.mgr.RegisterInstance(&DatabaseInstance{
		Name:   "agentdb:dep-1",
		Config: config.DatabaseConfig{Name: "agentdb:dep-1"},
		Status: &InstanceStatus{Connected: true},
	})

	changed, err := h.mgr.EmergencyStopStrict("", "test")

	if err != nil {
		t.Fatalf("agent DB without executor must not fail the stop: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	if !h.mgr.InstanceStopped(h.mgr.GetInstance("agentdb:dep-1")) {
		t.Fatal("agent DB must still be stopped in memory")
	}
	if _, ok := h.persistedState("agentdb:dep-1"); ok {
		t.Fatal("agent DB has no executor and must not be persisted")
	}
	if value, ok := h.persistedState("prod"); !ok || !value {
		t.Fatalf("prod persisted=%v ok=%v, want true", value, ok)
	}
}

func TestResume_FailedPersistenceKeepsDatabaseStopped(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	if _, err := h.mgr.EmergencyStopStrict("", "test"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	h.mu.Lock()
	h.failing["bravo"] = true
	h.mu.Unlock()

	resumed, err := h.mgr.ResumeStrict("", "test")

	if resumed != 1 {
		t.Fatalf("resumed = %d, want 1", resumed)
	}
	var stopErr *EmergencyStopError
	if !errors.As(err, &stopErr) || stopErr.Failed["bravo"] == nil {
		t.Fatalf("error = %v, want EmergencyStopError naming bravo", err)
	}
	alpha := h.mgr.GetInstance("alpha")
	bravo := h.mgr.GetInstance("bravo")
	if h.mgr.InstanceStopped(alpha) || !alpha.Executor.ExecutorEnabled() {
		t.Fatal("alpha should be resumed and its executor re-enabled")
	}
	if !h.mgr.InstanceStopped(bravo) || bravo.Executor.ExecutorEnabled() {
		t.Fatal("bravo must stay stopped when its resume could not persist")
	}
}

func TestResume_RestoresConfiguredExecutorGate(t *testing.T) {
	disabled := false
	h := newStopHarness(t, nil)
	h.mgr.RegisterInstance(&DatabaseInstance{
		Name:     "readonly",
		Config:   config.DatabaseConfig{Name: "readonly", ExecutorEnabled: &disabled},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true},
	})
	h.mgr.GetInstance("readonly").Executor.SetExecutorEnabled(false)

	if _, err := h.mgr.EmergencyStopStrict("readonly", "test"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := h.mgr.ResumeStrict("readonly", "test"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if h.mgr.GetInstance("readonly").Executor.ExecutorEnabled() {
		t.Fatal("resume must not enable an executor configured as disabled")
	}
}

func TestReplacementInheritsEmergencyStop(t *testing.T) {
	h := newStopHarness(t, []string{"orders"}, "orders")
	if _, err := h.mgr.EmergencyStopStrict("orders", "test"); err == nil {
		t.Fatal("expected persistence failure for orders")
	}
	old := h.mgr.GetInstance("orders")
	candidate := &DatabaseInstance{
		Name:     "orders",
		Config:   config.DatabaseConfig{Name: "orders"},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true},
	}

	err := h.mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.PublishReplacement("orders", old, candidate)
	})

	if err != nil {
		t.Fatalf("publish replacement: %v", err)
	}
	if !h.mgr.InstanceStopped(candidate) {
		t.Fatal("replacement runtime must inherit the in-memory emergency stop")
	}
	if candidate.Executor.ExecutorEnabled() {
		t.Fatal("replacement executor must be gated while stopped")
	}
}

func TestEmergencyStopError_MessageListsFailedDatabasesSorted(t *testing.T) {
	err := &EmergencyStopError{Stopped: true, Failed: map[string]error{
		"zeta": errors.New("boom"), "alpha": errors.New("down"),
	}}
	want := "emergency stop persistence failed for 2 database(s): " +
		"alpha: down; zeta: boom"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, err.Failed["zeta"]) {
		t.Fatal("EmergencyStopError must unwrap per-database errors")
	}
}
