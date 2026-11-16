package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
)

// env-race: the background index builder asked the process global fleetMgr
// whether its database is emergency-stopped, from its own goroutine, on
// every poll. Standalone mode assigned fleetMgr only after the runtime (and
// the builder) had started, so the builder read a manager that was being
// written (CI's -race run: TestRuntimeParityEquivalentDatabase, 6 of 6
// runs). The builder now answers from the manager the runtime was built
// under, and standalone assigns it before building the runtime.

// stoppedProbe replaces the builder with one that answers Stopped() once
// release closes.
func stoppedProbe(t *testing.T) (release chan struct{}, answer chan bool) {
	t.Helper()
	old := runDeferredIndexBuilder
	t.Cleanup(func() { runDeferredIndexBuilder = old })
	release, answer = make(chan struct{}), make(chan bool, 1)
	runDeferredIndexBuilder = func(_ context.Context, b *schema.DeferredIndexBuilder) {
		<-release
		answer <- b.Stopped()
	}
	return release, answer
}

func builderRuntime(name string, inst *fleet.DatabaseInstance) *databaseRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &databaseRuntime{
		spec: databaseRuntimeSpec{Scope: "fleet", Name: name},
		ctx:  ctx, cancel: cancel, workers: &sync.WaitGroup{}, inst: inst,
	}
}

func awaitStopped(t *testing.T, rt *databaseRuntime, answer chan bool) bool {
	t.Helper()
	defer func() { rt.cancel(); rt.workers.Wait() }()
	select {
	case got := <-answer:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("the builder never asked whether its database is stopped")
		return false
	}
}

func TestRace_FleetMgr_DeferredBuilderAnswersFromItsManager(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(map[bool]string{true: "stopped", false: "running"}[stopped],
			func(t *testing.T) {
				preserveFleetRuntimeGlobals(t)
				cfg = config.DefaultConfig()
				inst := &fleet.DatabaseInstance{Name: "builder-a", Stopped: stopped}
				fleetMgr = fleet.NewManager(cfg)
				release, answer := stoppedProbe(t)
				rt := builderRuntime("builder-a", inst)
				rt.startDeferredIndexBuilder()
				// The process global moves on while the builder runs.
				fleetMgr = nil
				close(release)
				if got := awaitStopped(t, rt, answer); got != stopped {
					t.Fatalf("builder Stopped() = %t, want %t: it read the process "+
						"global instead of its runtime's manager", got, stopped)
				}
			})
	}
}

// Boundary: without a manager (nothing published yet) nothing is stopped,
// as before.
func TestDeferredBuilderWithoutManagerIsNotStopped(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	fleetMgr = nil
	release, answer := stoppedProbe(t)
	rt := builderRuntime("builder-b", &fleet.DatabaseInstance{Name: "builder-b", Stopped: true})
	rt.startDeferredIndexBuilder()
	close(release)
	if awaitStopped(t, rt, answer) {
		t.Fatal("a builder without a manager reported its database stopped")
	}
}

// State transition: the builder follows the manager's emergency stop and
// resume of its database while it runs.
func TestRace_FleetMgr_DeferredBuilderFollowsStopAndResume(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	inst := &fleet.DatabaseInstance{Name: "builder-c"}
	mgr.RegisterInstance(inst)
	fleetMgr = mgr
	old := runDeferredIndexBuilder
	t.Cleanup(func() { runDeferredIndexBuilder = old })
	ask, answer := make(chan struct{}), make(chan bool)
	runDeferredIndexBuilder = func(ctx context.Context, b *schema.DeferredIndexBuilder) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ask:
				answer <- b.Stopped()
			}
		}
	}
	rt := builderRuntime("builder-c", inst)
	rt.startDeferredIndexBuilder()
	defer func() { rt.cancel(); rt.workers.Wait() }()
	query := func() bool { ask <- struct{}{}; return <-answer }
	if query() {
		t.Fatal("running database reported stopped")
	}
	if n := mgr.EmergencyStop("builder-c"); n != 1 {
		t.Fatalf("emergency stop changed %d databases, want 1", n)
	}
	if !query() {
		t.Fatal("builder did not see its database's emergency stop")
	}
	if n := mgr.Resume("builder-c"); n != 1 {
		t.Fatalf("resume changed %d databases, want 1", n)
	}
	if query() {
		t.Fatal("builder still sees its database stopped after the resume")
	}
}
