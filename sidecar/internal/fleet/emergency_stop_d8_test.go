package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// transitionLog records every persisted emergency-stop transition, in
// order, as the durable audit trail would see it.
type transitionLog struct {
	mu      sync.Mutex
	entries []transition
	state   map[string]bool
}

type transition struct {
	db      string
	stopped bool
	actor   string
}

func newTransitionLog() *transitionLog {
	return &transitionLog{state: map[string]bool{}}
}

func (l *transitionLog) record(db string, stopped bool, actor string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, transition{db: db, stopped: stopped, actor: actor})
	l.state[db] = stopped
}

func (l *transitionLog) snapshot() ([]transition, map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := make(map[string]bool, len(l.state))
	for k, v := range l.state {
		state[k] = v
	}
	return append([]transition(nil), l.entries...), state
}

func newD8Manager(names ...string) *DatabaseManager {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	for _, name := range names {
		mgr.RegisterInstance(&DatabaseInstance{
			Name:     name,
			Config:   config.DatabaseConfig{Name: name},
			Executor: newGateExecutor(),
			Status:   &InstanceStatus{Connected: true, LastSeen: time.Now()},
		})
	}
	return mgr
}

func TestEmergencyStop_FleetPersistFailureStopsAllAndAttributes(t *testing.T) {
	mgr := newD8Manager("alpha", "bravo", "charlie")
	log := newTransitionLog()
	mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		if inst.Name == "bravo" {
			return errors.New("sage.config unwritable on bravo")
		}
		log.record(inst.Name, stopped, actor)
		return nil
	}

	changed, err := mgr.EmergencyStopStrict("", "op@example.com")

	if changed != 3 {
		t.Fatalf("changed = %d, want 3", changed)
	}
	var stopErr *EmergencyStopError
	if !errors.As(err, &stopErr) || len(stopErr.Failed) != 1 ||
		stopErr.Failed["bravo"] == nil {
		t.Fatalf("error = %v, want EmergencyStopError naming only bravo", err)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		inst := mgr.GetInstance(name)
		if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
			t.Fatalf("%s must be stopped in memory with its executor gated", name)
		}
		if inst.StoppedBy != "op@example.com" || inst.StoppedAt.IsZero() {
			t.Fatalf("%s attribution = %q at %v, want op@example.com with a time",
				name, inst.StoppedBy, inst.StoppedAt)
		}
	}
	entries, _ := log.snapshot()
	if len(entries) != 2 {
		t.Fatalf("persisted transitions = %v, want alpha and charlie", entries)
	}
	for _, e := range entries {
		if e.actor != "op@example.com" || !e.stopped {
			t.Fatalf("persisted %+v, want stopped by op@example.com", e)
		}
	}
}

func TestResume_RecordsActorAndClearsAttribution(t *testing.T) {
	mgr := newD8Manager("alpha")
	log := newTransitionLog()
	mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		log.record(inst.Name, stopped, actor)
		return nil
	}
	if _, err := mgr.EmergencyStopStrict("alpha", "op@example.com"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	resumed, err := mgr.ResumeStrict("alpha", "admin@example.com")

	if err != nil || resumed != 1 {
		t.Fatalf("resume = %d, %v; want 1, nil", resumed, err)
	}
	inst := mgr.GetInstance("alpha")
	if mgr.InstanceStopped(inst) || inst.StoppedBy != "" || !inst.StoppedAt.IsZero() {
		t.Fatalf("resumed instance kept stop attribution %q/%v", inst.StoppedBy, inst.StoppedAt)
	}
	entries, _ := log.snapshot()
	want := []transition{
		{db: "alpha", stopped: true, actor: "op@example.com"},
		{db: "alpha", stopped: false, actor: "admin@example.com"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("audited transitions = %v, want %v", entries, want)
	}
}

func TestFleetOverviewExposesPerDatabaseEmergencyStop(t *testing.T) {
	mgr := newD8Manager("db1", "db2")
	mgr.persistStop = func(context.Context, *DatabaseInstance, bool, string) error {
		return nil
	}
	before := time.Now()
	if _, err := mgr.EmergencyStopStrict("db1", "op@example.com"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	raw, err := json.Marshal(mgr.FleetStatus())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Summary struct {
			EmergencyStopped      bool `json:"emergency_stopped"`
			EmergencyStoppedCount int  `json:"emergency_stopped_count"`
		} `json:"summary"`
		Databases []struct {
			Name               string     `json:"name"`
			EmergencyStopped   bool       `json:"emergency_stopped"`
			EmergencyStoppedBy string     `json:"emergency_stopped_by"`
			EmergencyStoppedAt *time.Time `json:"emergency_stopped_at"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Summary.EmergencyStopped || got.Summary.EmergencyStoppedCount != 1 {
		t.Fatalf("summary = %+v, want stopped with count 1", got.Summary)
	}
	for _, db := range got.Databases {
		switch db.Name {
		case "db1":
			if !db.EmergencyStopped || db.EmergencyStoppedBy != "op@example.com" ||
				db.EmergencyStoppedAt == nil || db.EmergencyStoppedAt.Before(before) {
				t.Fatalf("db1 = %+v, want stopped by op@example.com after %v", db, before)
			}
		case "db2":
			if db.EmergencyStopped || db.EmergencyStoppedBy != "" ||
				db.EmergencyStoppedAt != nil {
				t.Fatalf("db2 = %+v, want running without attribution", db)
			}
		default:
			t.Fatalf("unexpected database %q", db.Name)
		}
	}
}

// TestConcurrentStopDuringResume_StopWins pins the race rule: a stop that
// latches while a resume is persisting wins, and memory matches the durable
// flag once both finish.
func TestConcurrentStopDuringResume_StopWins(t *testing.T) {
	mgr := newD8Manager("alpha")
	log := newTransitionLog()
	resumeWriting := make(chan struct{})
	releaseResume := make(chan struct{})
	mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		if !stopped {
			close(resumeWriting)
			<-releaseResume
		}
		log.record(inst.Name, stopped, actor)
		return nil
	}
	inst := mgr.GetInstance("alpha")
	mgr.mu.Lock()
	inst.Stopped = true
	inst.StoppedBy = "first@example.com"
	mgr.mu.Unlock()

	resumeDone := make(chan error, 1)
	go func() {
		_, err := mgr.ResumeStrict("alpha", "resumer@example.com")
		resumeDone <- err
	}()
	<-resumeWriting
	stopDone := make(chan error, 1)
	go func() {
		_, err := mgr.EmergencyStopStrict("alpha", "stopper@example.com")
		stopDone <- err
	}()
	waitForStoppedBy(t, mgr, inst, "stopper@example.com")
	close(releaseResume)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("stop: %v", err)
	}

	entries, state := log.snapshot()
	if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
		t.Fatal("the overlapping stop must win: instance resumed in memory")
	}
	if !state["alpha"] {
		t.Fatal("durable flag must end stopped after the overlapping stop")
	}
	want := []transition{
		{db: "alpha", stopped: false, actor: "resumer@example.com"},
		{db: "alpha", stopped: true, actor: "stopper@example.com"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("audited transitions = %v, want %v", entries, want)
	}
}

func waitForStoppedBy(
	t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance, actor string,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.RLock()
		by := inst.StoppedBy
		mgr.mu.RUnlock()
		if by == actor {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stop by %s never latched in memory", actor)
}

// TestConcurrentStopResume_MemoryMatchesDurableFlag hammers stop/resume
// from many goroutines. However the operations interleave, every
// transition is audited and memory ends equal to the durable flag.
func TestConcurrentStopResume_MemoryMatchesDurableFlag(t *testing.T) {
	for i := 0; i < 50; i++ {
		mgr := newD8Manager("alpha", "bravo")
		log := newTransitionLog()
		mgr.persistStop = func(
			_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
		) error {
			time.Sleep(time.Duration(len(actor)%3) * 100 * time.Microsecond)
			log.record(inst.Name, stopped, actor)
			return nil
		}
		ops := runConcurrentStopResume(t, mgr, 20)

		entries, state := log.snapshot()
		if len(entries) != ops*2 {
			t.Fatalf("iteration %d: %d audited transitions, want %d",
				i, len(entries), ops*2)
		}
		for _, name := range []string{"alpha", "bravo"} {
			inst := mgr.GetInstance(name)
			if mgr.InstanceStopped(inst) != state[name] {
				t.Fatalf("iteration %d: %s memory stopped=%v, durable=%v",
					i, name, mgr.InstanceStopped(inst), state[name])
			}
			if mgr.InstanceStopped(inst) == inst.Executor.ExecutorEnabled() {
				t.Fatalf("iteration %d: %s executor gate disagrees with latch", i, name)
			}
		}
	}
}

func runConcurrentStopResume(t *testing.T, mgr *DatabaseManager, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	for j := 0; j < n; j++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			actor := fmt.Sprintf("user%d@example.com", j)
			var err error
			if j%2 == 0 {
				_, err = mgr.EmergencyStopStrict("", actor)
			} else {
				_, err = mgr.ResumeStrict("", actor)
			}
			if err != nil {
				t.Errorf("op %d: %v", j, err)
			}
		}(j)
	}
	wg.Wait()
	return n
}

func TestRegisterRestoresPersistedStopWithAttribution(t *testing.T) {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.readStop = func(context.Context, *DatabaseInstance) (executor.EmergencyStopState, error) {
		return executor.EmergencyStopState{Stopped: true, UpdatedBy: "op@example.com",
			UpdatedAt: at}, nil
	}
	inst := &DatabaseInstance{
		Name:     "orders",
		Config:   config.DatabaseConfig{Name: "orders"},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true, Platform: "postgres"},
	}

	mgr.RegisterInstance(inst)

	assertRestoredStop(t, mgr, inst, "op@example.com", at)
}

func TestRegisterStopReadErrorFailsClosed(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.readStop = func(context.Context, *DatabaseInstance) (executor.EmergencyStopState, error) {
		return executor.EmergencyStopState{}, errors.New("statement timeout")
	}
	inst := &DatabaseInstance{
		Name:     "orders",
		Config:   config.DatabaseConfig{Name: "orders"},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true},
	}

	mgr.RegisterInstance(inst)

	if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
		t.Fatal("an unreadable persisted flag must restore as stopped")
	}
	if inst.StoppedBy != executor.EmergencyStopActorSystem {
		t.Fatalf("StoppedBy = %q, want %q", inst.StoppedBy, executor.EmergencyStopActorSystem)
	}
}

func TestRegisterWithoutExecutorSkipsRestore(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	calls := 0
	mgr.readStop = func(context.Context, *DatabaseInstance) (executor.EmergencyStopState, error) {
		calls++
		return executor.EmergencyStopState{Stopped: true}, nil
	}
	inst := &DatabaseInstance{Name: "executorless:x", Status: &InstanceStatus{}}

	mgr.RegisterInstance(inst)

	if calls != 0 || mgr.InstanceStopped(inst) {
		t.Fatalf("reads=%d stopped=%v; an executor-less instance has no flag",
			calls, mgr.InstanceStopped(inst))
	}
}

func assertRestoredStop(
	t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance,
	actor string, at time.Time,
) {
	t.Helper()
	if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
		t.Fatal("persisted stop must restore the in-memory latch and gate")
	}
	if inst.StoppedBy != actor || !inst.StoppedAt.Equal(at) {
		t.Fatalf("attribution = %q at %v, want %q at %v",
			inst.StoppedBy, inst.StoppedAt, actor, at)
	}
	status := mgr.FleetStatus()
	if !status.Summary.EmergencyStopped || status.Summary.EmergencyStoppedCount != 1 {
		t.Fatalf("summary = %+v, want stopped", status.Summary)
	}
	for _, db := range status.Databases {
		if db.Name != inst.Name {
			continue
		}
		if !db.EmergencyStopped || db.EmergencyStoppedBy != actor {
			t.Fatalf("database status = %+v, want stopped by %s", db, actor)
		}
		assertFamiliesStopped(t, db.Status.Capabilities)
	}
}

func assertFamiliesStopped(t *testing.T, caps ProviderCapabilities) {
	t.Helper()
	if caps.ReadyForAutoSafe {
		t.Fatal("a stopped database must not report ready_for_auto_safe")
	}
	for _, family := range caps.ActionFamilies {
		if family.ActionType != "analyze_table" {
			continue
		}
		if family.Supported || family.Decision != executor.PolicyDecisionBlocked ||
			family.BlockedReason != "emergency stop is active" {
			t.Fatalf("analyze_table = %+v, want blocked by the emergency stop", family)
		}
		return
	}
	t.Fatalf("analyze_table family missing from %+v", caps.ActionFamilies)
}
