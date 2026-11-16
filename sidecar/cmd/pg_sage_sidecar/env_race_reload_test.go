package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// env-race state transitions and error propagation: a SIGHUP reload runs
// while per-database runtimes start, while the fleet is read, and while an
// emergency stop lands. Under -race these exercise every lock the reload,
// the runtime start and the stop latch share; without the detector they
// still pin the end state.

// readFleetUntil reads every instance's config, status and stop flag until
// stop closes, the way the API and the orchestrators do, and counts passes.
func readFleetUntil(stop <-chan struct{}, passes *atomic.Int64) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		for _, inst := range fleetMgr.Instances() {
			_ = inst.ConfigSnapshot()
			_ = inst.SnapshotStatus()
			_ = fleetMgr.InstanceStopped(inst)
		}
		passes.Add(1)
	}
}

func TestRace_FleetReload_AddedRuntimeStartsWhileFleetIsRead(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl")
	ctl := fleetMgr.GetInstance("ctl")
	added := reloadDatabaseConfig(t, "added", testdb.CreateDatabase(t, "reload_added"))
	stop := make(chan struct{})
	var passes atomic.Int64
	var readers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() { defer readers.Done(); readFleetUntil(stop, &passes) }()
	}
	err := env.reload(func(c *config.Config) { c.Databases = append(c.Databases, added) })
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatalf("reload adding a database while the fleet is read: %v", err)
	}
	inst := fleetMgr.GetInstance("added")
	if inst == nil || inst.Pool == nil || inst.SnapshotStatus().Error != "" {
		t.Fatalf("added database not running after the reload: %+v", inst)
	}
	if fleetMgr.GetInstance("ctl") != ctl || poolClosed(ctl.Pool) {
		t.Fatal("adding a database replaced or closed the control database's runtime")
	}
	if passes.Load() == 0 {
		t.Fatal("the readers never ran during the reload")
	}
}

// An emergency stop of the whole fleet lands while a reload rebuilds one
// database: in either order the rebuilt runtime comes up stopped (the fleet
// latch, fleet-09), and a fleet resume releases both.
func TestRace_FleetReload_EmergencyStopDuringRebuild(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	oldB := fleetMgr.GetInstance("b")
	var wg sync.WaitGroup
	var reloadErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		reloadErr = reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
			db.MaxConnections = 4
		})
	}()
	go func() { defer wg.Done(); fleetMgr.EmergencyStop("") }()
	wg.Wait()
	if reloadErr != nil {
		t.Fatalf("rebuild reload during a fleet stop: %v", reloadErr)
	}
	newB, ctl := fleetMgr.GetInstance("b"), fleetMgr.GetInstance("ctl")
	if newB == nil || newB == oldB || newB.ConfigSnapshot().MaxConnections != 4 {
		t.Fatalf("b was not rebuilt with the new setting: %+v", newB)
	}
	for _, inst := range []*fleet.DatabaseInstance{ctl, newB} {
		if !fleetMgr.InstanceStopped(inst) {
			t.Fatalf("%s runs after a fleet stop that raced the reload", inst.Name)
		}
	}
	if n := fleetMgr.Resume(""); n != 2 {
		t.Fatalf("fleet resume released %d databases, want 2", n)
	}
	for _, inst := range []*fleet.DatabaseInstance{ctl, newB} {
		if fleetMgr.InstanceStopped(inst) {
			t.Fatalf("%s still stopped after the fleet resume", inst.Name)
		}
	}
}

// Error propagation: a rebuild that cannot start while the fleet is read
// reports one error, naming the database once, and keeps the runtime that
// was running.
func TestRace_FleetReload_FailedStartReportsItsErrorOnce(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	oldB := fleetMgr.GetInstance("b")
	stop := make(chan struct{})
	var passes atomic.Int64
	var readers sync.WaitGroup
	readers.Add(1)
	go func() { defer readers.Done(); readFleetUntil(stop, &passes) }()
	err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) { db.Port = 1 })
	close(stop)
	readers.Wait()
	if !errors.Is(err, errFleetRuntimeCandidate) {
		t.Fatalf("unreachable rebuild = %v, want errFleetRuntimeCandidate", err)
	}
	if n := strings.Count(err.Error(), `db "b"`); n != 1 {
		t.Fatalf("error names the database %d times, want once: %v", n, err)
	}
	if fleetMgr.GetInstance("b") != oldB || poolClosed(oldB.Pool) {
		t.Fatal("a failed rebuild replaced or closed the running runtime")
	}
	if oldB.SnapshotStatus().Error != "" {
		t.Fatalf("the running runtime took the candidate's error: %q",
			oldB.SnapshotStatus().Error)
	}
}
