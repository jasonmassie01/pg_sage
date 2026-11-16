package main

import (
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// env-race: standalone built its runtime, whose workers read fleetMgr, and
// only then assigned fleetMgr. Every mode now has its manager before the
// first runtime worker starts, as YAML fleet and meta-db always did.
func TestRace_FleetMgr_StandaloneManagerPrecedesRuntimeWorkers(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	markFixtureAsAzure(t, dsn)
	stubAzureToken(t, nil)
	base := parityBaseConfig(t, dsn)
	var mu sync.Mutex
	var during *fleet.DatabaseManager
	built := 0
	old := runtimeBuilt
	t.Cleanup(func() { runtimeBuilt = old })
	runtimeBuilt = func(rt *databaseRuntime) {
		mu.Lock()
		during, built = fleetMgr, built+1
		mu.Unlock()
		old(rt)
	}
	inst := buildStandaloneParity(t, base, dsn)
	if inst == nil {
		t.Fatal("standalone registered no runtime")
	}
	mu.Lock()
	defer mu.Unlock()
	if built != 1 {
		t.Fatalf("standalone built %d runtimes, want 1", built)
	}
	if during == nil || during != fleetMgr {
		t.Fatalf("the runtime's workers started under manager %p, but the runtime "+
			"was registered with %p", during, fleetMgr)
	}
	if fleetMgr.GetInstance(inst.Name) != inst {
		t.Fatal("the registered runtime is not the manager's instance")
	}
}
