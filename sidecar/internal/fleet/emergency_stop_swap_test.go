package fleet

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// savedFlag models sage.config shared by every runtime generation of one
// database: an old failed runtime and its reconnected copy read and write
// the same row.
type savedFlag struct {
	mu    sync.Mutex
	state map[string]executor.EmergencyStopState
}

func installSavedFlag(mgr *DatabaseManager) *savedFlag {
	f := &savedFlag{state: map[string]executor.EmergencyStopState{}}
	mgr.persistStop = func(
		_ context.Context, inst *DatabaseInstance, stopped bool, actor string,
	) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.state[inst.Name] = executor.EmergencyStopState{
			Stopped: stopped, UpdatedBy: actor, UpdatedAt: time.Now()}
		return nil
	}
	mgr.readStop = func(
		_ context.Context, inst *DatabaseInstance,
	) (executor.EmergencyStopState, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.state[inst.Name], nil
	}
	return f
}

func (f *savedFlag) get(name string) executor.EmergencyStopState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[name]
}

// failedRuntime is what meta-db registers for an unreachable database: no
// pool and no executor, so nothing can persist through it.
func failedRuntime(name string) *DatabaseInstance {
	return &DatabaseInstance{
		Name:   name,
		Config: config.DatabaseConfig{Name: name},
		Status: &InstanceStatus{Error: "connection refused"},
	}
}

func reconnectedRuntime(name string) *DatabaseInstance {
	return &DatabaseInstance{
		Name:     name,
		Config:   config.DatabaseConfig{Name: name},
		Executor: newGateExecutor(),
		Status:   &InstanceStatus{Connected: true, Platform: "postgres"},
	}
}

func assertSwappedCopyStopped(
	t *testing.T, mgr *DatabaseManager, flag *savedFlag,
	candidate *DatabaseInstance, actor string,
) {
	t.Helper()
	current := mgr.GetInstance(candidate.Name)
	if current != candidate {
		t.Fatal("the reconnected copy was not published")
	}
	if !mgr.InstanceStopped(current) || current.Executor.ExecutorEnabled() {
		t.Fatal("stop was lost across the reconnect swap: new copy is running")
	}
	saved := flag.get(candidate.Name)
	if !saved.Stopped {
		t.Fatal("stop was lost across the reconnect swap: saved flag is not stopped")
	}
	if actor != "" && (current.StoppedBy != actor || saved.UpdatedBy != actor) {
		t.Fatalf("attribution memory=%q saved=%q, want %q",
			current.StoppedBy, saved.UpdatedBy, actor)
	}
}

// TestStopDuringReconnectSwap_NewCopyStopped runs the stop while the
// reconnect health check holds the swap open (a barrier), so it lands on
// the old failed runtime just before the new copy is published.
func TestStopDuringReconnectSwap_NewCopyStopped(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	flag := installSavedFlag(mgr)
	old := failedRuntime("orders")
	mgr.RegisterInstance(old)
	candidate := reconnectedRuntime("orders")
	checking := make(chan struct{})
	release := make(chan struct{})
	swapped := make(chan error, 1)
	go func() {
		swapped <- mgr.ReplaceInstanceIfCurrent(context.Background(), "orders",
			old, candidate, func(context.Context, *DatabaseInstance) error {
				close(checking)
				<-release
				return nil
			})
	}()
	<-checking

	_, stopErr := mgr.EmergencyStopStrict("orders", "op@example.com")
	close(release)
	if err := <-swapped; err != nil {
		t.Fatalf("swap: %v", err)
	}

	if stopErr != nil {
		t.Fatalf("stop: %v", stopErr)
	}
	assertSwappedCopyStopped(t, mgr, flag, candidate, "op@example.com")
}

// TestStopRacingReconnectSwap_Randomized interleaves a fleet-wide stop with
// the swap at random points. Whatever the order, the stop wins.
func TestStopRacingReconnectSwap_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 300; i++ {
		mgr := NewManager(&config.Config{Mode: "fleet"})
		flag := installSavedFlag(mgr)
		old := failedRuntime("orders")
		mgr.RegisterInstance(old)
		candidate := reconnectedRuntime("orders")
		checkDelay := time.Duration(rng.Intn(200)) * time.Microsecond
		stopDelay := time.Duration(rng.Intn(200)) * time.Microsecond

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := mgr.ReplaceInstanceIfCurrent(context.Background(), "orders",
				old, candidate, func(context.Context, *DatabaseInstance) error {
					time.Sleep(checkDelay)
					return nil
				})
			if err != nil {
				t.Errorf("iteration %d: swap: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(stopDelay)
			if _, err := mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
				t.Errorf("iteration %d: stop: %v", i, err)
			}
		}()
		wg.Wait()
		if t.Failed() {
			return
		}
		assertSwappedCopyStopped(t, mgr, flag, candidate, "op@example.com")
	}
}

// TestInheritedStopIsSavedAndResumable: a stop taken while the database was
// unreachable is saved when the reconnected copy is published, and an
// explicit resume afterwards still releases it.
func TestInheritedStopIsSavedAndResumable(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	flag := installSavedFlag(mgr)
	old := failedRuntime("orders")
	mgr.RegisterInstance(old)
	if _, err := mgr.EmergencyStopStrict("orders", "op@example.com"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	candidate := reconnectedRuntime("orders")

	err := mgr.ReplaceInstanceIfCurrent(context.Background(), "orders", old,
		candidate, func(context.Context, *DatabaseInstance) error { return nil })

	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	assertSwappedCopyStopped(t, mgr, flag, candidate, "op@example.com")
	if _, err := mgr.ResumeStrict("orders", "admin@example.com"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if mgr.InstanceStopped(candidate) || flag.get("orders").Stopped {
		t.Fatal("an explicit resume after the swap must release the new copy")
	}
}

// TestStopResumeSwapRandomized mixes a stop, a resume and the reconnect swap.
// Whatever the order, the published copy's memory equals the saved flag.
func TestStopResumeSwapRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 300; i++ {
		mgr := NewManager(&config.Config{Mode: "fleet"})
		flag := installSavedFlag(mgr)
		old := failedRuntime("orders")
		mgr.RegisterInstance(old)
		candidate := reconnectedRuntime("orders")
		delays := [3]time.Duration{}
		for j := range delays {
			delays[j] = time.Duration(rng.Intn(200)) * time.Microsecond
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = mgr.ReplaceInstanceIfCurrent(context.Background(), "orders", old,
				candidate, func(context.Context, *DatabaseInstance) error {
					time.Sleep(delays[0])
					return nil
				})
		}()
		go func() {
			defer wg.Done()
			time.Sleep(delays[1])
			_, _ = mgr.EmergencyStopStrict("orders", "op@example.com")
		}()
		go func() {
			defer wg.Done()
			time.Sleep(delays[2])
			_, _ = mgr.ResumeStrict("orders", "admin@example.com")
		}()
		wg.Wait()
		current := mgr.GetInstance("orders")
		if current != candidate {
			t.Fatalf("iteration %d: reconnected copy not published", i)
		}
		if mgr.InstanceStopped(current) != flag.get("orders").Stopped {
			t.Fatalf("iteration %d: memory stopped=%v, saved=%v", i,
				mgr.InstanceStopped(current), flag.get("orders").Stopped)
		}
	}
}
