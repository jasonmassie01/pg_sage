package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// fakeFleetStore is the meta database's fleet_emergency_stop row.
type fakeFleetStore struct {
	mu       sync.Mutex
	state    executor.EmergencyStopState
	readErr  error
	writeErr error
	writes   []string
}

func (s *fakeFleetStore) ReadFleetStop(context.Context) (executor.EmergencyStopState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.readErr
}

func (s *fakeFleetStore) WriteFleetStop(_ context.Context, stopped bool, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.writes = append(s.writes, fmt.Sprintf("%v by %s", stopped, actor))
	s.state = executor.EmergencyStopState{Stopped: stopped, UpdatedBy: actor, UpdatedAt: time.Now()}
	return nil
}

func (s *fakeFleetStore) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// scopeLog records the emergency_stop_scope writes per database.
type scopeLog struct {
	mu      sync.Mutex
	scope   map[string]bool
	failing map[string]bool
}

func useScopeLog(mgr *DatabaseManager) *scopeLog {
	l := &scopeLog{scope: map[string]bool{}, failing: map[string]bool{}}
	mgr.persistScope = func(_ context.Context, inst *DatabaseInstance, fleet bool, _ string) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.failing[inst.Name] {
			return fmt.Errorf("emergency_stop_scope unwritable on %s", inst.Name)
		}
		l.scope[inst.Name] = fleet
		return nil
	}
	return l
}

func (l *scopeLog) get(name string) (bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	value, ok := l.scope[name]
	return value, ok
}

func TestFleetStopMetaReadErrorRestoresEveryInstanceStopped(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	store := &fakeFleetStore{readErr: errors.New("meta db: connection refused")}

	h.mgr.UseFleetStopStore(store)
	h.mgr.RegisterInstance(newFleetMember("bravo"))

	for _, name := range []string{"alpha", "bravo"} {
		assertLatchedBy(t, h.mgr, h.mgr.GetInstance(name), executor.EmergencyStopActorSystem)
		if value, ok := h.persistedState(name); !ok || !value {
			t.Fatalf("%s durable flag = %v (written %v), want true", name, value, ok)
		}
	}
	summary := h.mgr.FleetStatus().Summary
	if !summary.FleetStopped || summary.FleetStoppedBy != executor.EmergencyStopActorSystem {
		t.Fatalf("summary = %+v, want fleet stopped by system", summary)
	}
}

func TestFleetStopMetaStoredStopRestores(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	h := newStopHarness(t, nil)
	h.mgr.UseFleetStopStore(&fakeFleetStore{state: executor.EmergencyStopState{
		Stopped: true, UpdatedBy: "op@example.com", UpdatedAt: at}})

	h.mgr.RegisterInstance(newFleetMember("orders"))

	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("orders"), "op@example.com")
	summary := h.mgr.FleetStatus().Summary
	if summary.FleetStoppedAt == nil || !summary.FleetStoppedAt.Equal(at) {
		t.Fatalf("fleet stopped at %v, want %v", summary.FleetStoppedAt, at)
	}
}

func TestFleetStopMetaRunningStoreDoesNotLatch(t *testing.T) {
	h := newStopHarness(t, nil)
	h.mgr.UseFleetStopStore(&fakeFleetStore{})

	h.mgr.RegisterInstance(newFleetMember("orders"))

	assertRunning(t, h.mgr, h.mgr.GetInstance("orders"))
	if h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("a stored 'false' must not latch the fleet")
	}
}

func TestFleetStopWritesMetaKeyOnStopAndResume(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	store := &fakeFleetStore{}
	h.mgr.UseFleetStopStore(store)

	if _, err := h.mgr.EmergencyStopStrict("alpha", "single@example.com"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	if _, err := h.mgr.ResumeStrict("alpha", "single@example.com"); err != nil {
		t.Fatalf("resume alpha: %v", err)
	}
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if _, err := h.mgr.ResumeStrict("", "admin@example.com"); err != nil {
		t.Fatalf("resume all: %v", err)
	}

	want := "[true by op@example.com false by admin@example.com]"
	if got := fmt.Sprint(store.written()); got != want {
		t.Fatalf("meta writes = %s, want %s (single-database stops write none)", got, want)
	}
}

func TestFleetStopMetaWriteFailureFailsClosed(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})
	store := &fakeFleetStore{writeErr: errors.New("meta db read-only")}
	h.mgr.UseFleetStopStore(store)

	_, err := h.mgr.EmergencyStopStrict("", "op@example.com")

	var stopErr *EmergencyStopError
	if !errors.As(err, &stopErr) || stopErr.Failed[FleetStopKey] == nil {
		t.Fatalf("stop error = %v, want EmergencyStopError naming %s", err, FleetStopKey)
	}
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	_, err = h.mgr.ResumeStrict("", "op@example.com")
	if !errors.As(err, &stopErr) || stopErr.Failed[FleetStopKey] == nil {
		t.Fatalf("resume error = %v, want EmergencyStopError naming %s", err, FleetStopKey)
	}
	if !h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("an unsaved resume-all must keep the fleet latch")
	}
	late := newFleetMember("late")
	h.mgr.RegisterInstance(late)
	assertLatchedBy(t, h.mgr, late, "op@example.com")
}

func TestFleetStopPersistsScopeOnStopAllAndClearsOnResumeAll(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	scopes := useScopeLog(h.mgr)

	if _, err := h.mgr.EmergencyStopStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	if _, ok := scopes.get("alpha"); ok {
		t.Fatal("a single-database stop must not write a fleet scope")
	}
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	h.mgr.RegisterInstance(newFleetMember("late"))
	for _, name := range []string{"alpha", "bravo", "late"} {
		if fleet, ok := scopes.get(name); !ok || !fleet {
			t.Fatalf("%s scope = fleet:%v (written %v), want fleet", name, fleet, ok)
		}
	}
	if _, err := h.mgr.ResumeStrict("", "op@example.com"); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	for _, name := range []string{"alpha", "bravo", "late"} {
		if fleet, _ := scopes.get(name); fleet {
			t.Fatalf("%s scope still fleet after resume-all", name)
		}
		assertRunning(t, h.mgr, h.mgr.GetInstance(name))
	}
}

// A scope that cannot be cleared keeps that database stopped, and with it
// the fleet latch: its durable state still says fleet-stopped.
func TestFleetStopScopeClearFailureKeepsDatabaseStopped(t *testing.T) {
	h := newStopHarness(t, []string{"alpha", "bravo"})
	scopes := useScopeLog(h.mgr)
	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	scopes.mu.Lock()
	scopes.failing["bravo"] = true
	scopes.mu.Unlock()

	_, err := h.mgr.ResumeStrict("", "op@example.com")

	var stopErr *EmergencyStopError
	if !errors.As(err, &stopErr) || stopErr.Failed["bravo"] == nil {
		t.Fatalf("error = %v, want EmergencyStopError naming bravo", err)
	}
	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("bravo"), "op@example.com")
	if value, _ := h.persistedState("bravo"); !value {
		t.Fatal("bravo's flag must stay true while its fleet scope is set")
	}
	assertRunning(t, h.mgr, h.mgr.GetInstance("alpha"))
	if !h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("the fleet latch must stay on")
	}
}

// YAML fleet has no control database: a member that restarts with its
// own flag set and scope 'fleet' re-latches the whole fleet.
func TestFleetStopScopeRestoresFleetLatchFromMember(t *testing.T) {
	at := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	h := newStopHarness(t, nil)
	h.mgr.readStop = func(
		_ context.Context, inst *DatabaseInstance,
	) (executor.EmergencyStopState, error) {
		if inst.Name == "alpha" {
			return executor.EmergencyStopState{Stopped: true, UpdatedBy: "op@example.com",
				UpdatedAt: at}, nil
		}
		return executor.EmergencyStopState{}, nil
	}
	h.mgr.readScope = func(_ context.Context, inst *DatabaseInstance) (bool, error) {
		return inst.Name == "alpha", nil
	}
	bravo := newFleetMember("bravo")
	h.mgr.RegisterInstance(bravo)
	assertRunning(t, h.mgr, bravo)

	h.mgr.RegisterInstance(newFleetMember("alpha"))
	charlie := newFleetMember("charlie")
	h.mgr.RegisterInstance(charlie)

	for _, inst := range []*DatabaseInstance{bravo, charlie} {
		assertLatchedBy(t, h.mgr, inst, "op@example.com")
		if value, ok := h.persistedState(inst.Name); !ok || !value {
			t.Fatalf("%s durable flag = %v (written %v), want true", inst.Name, value, ok)
		}
	}
	summary := h.mgr.FleetStatus().Summary
	if !summary.FleetStopped || summary.FleetStoppedBy != "op@example.com" {
		t.Fatalf("summary = %+v, want fleet stopped by op@example.com", summary)
	}
}

// A fleet scope next to a 'false' flag is not a stop: the flag is what
// every executor reads, and resume-all clears the scope before the flag.
func TestFleetScopeWithoutStoppedFlagDoesNotLatch(t *testing.T) {
	h := newStopHarness(t, nil)
	h.mgr.readScope = func(context.Context, *DatabaseInstance) (bool, error) {
		return true, nil
	}

	h.mgr.RegisterInstance(newFleetMember("alpha"))

	assertRunning(t, h.mgr, h.mgr.GetInstance("alpha"))
	if h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("scope 'fleet' with emergency_stop 'false' must not latch the fleet")
	}
}

func TestFleetScopeReadErrorStopsOnlyThatDatabase(t *testing.T) {
	h := newStopHarness(t, []string{"bravo"})
	h.mgr.readStop = func(context.Context, *DatabaseInstance) (executor.EmergencyStopState, error) {
		return executor.EmergencyStopState{Stopped: true, UpdatedBy: "op@example.com",
			UpdatedAt: time.Now()}, nil
	}
	h.mgr.readScope = func(context.Context, *DatabaseInstance) (bool, error) {
		return false, errors.New("permission denied for table config")
	}

	h.mgr.RegisterInstance(newFleetMember("alpha"))

	assertLatchedBy(t, h.mgr, h.mgr.GetInstance("alpha"), "op@example.com")
	assertRunning(t, h.mgr, h.mgr.GetInstance("bravo"))
}

func TestUseFleetStopStoreNilIsNoop(t *testing.T) {
	h := newStopHarness(t, []string{"alpha"})

	h.mgr.UseFleetStopStore(nil)

	if _, err := h.mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all without a meta store: %v", err)
	}
	if !h.mgr.FleetStatus().Summary.FleetStopped {
		t.Fatal("the in-memory fleet latch works without a durable store")
	}
}
