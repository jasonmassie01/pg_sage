package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// drill-01d: the API and MCP read a database's drill rows where the drill
// pass records them (drillInstances): the control database, else the
// database's own pool (a nil answer). No concurrent access tests: the
// resolver keeps no state; it reads the manager per call.

func TestRestoreDrillResults_ReadsWhereDrillsRecord(t *testing.T) {
	mgr, pools := managerWith(t, "app", "governance")
	meta := &metaDBState{Pool: lazyPool(t, "meta")}
	c := &config.Config{Agents: config.AgentsConfig{ControlDatabase: "governance"}}
	if got := restoreDrillResults(c, mgr, meta)("app"); got != meta.Pool {
		t.Fatal("meta mode: drill rows live in the meta database")
	}
	results := restoreDrillResults(c, mgr, nil)
	if got := results("app"); got != pools["governance"] {
		t.Fatal("pinned control database: drill rows live there")
	}
	for _, inst := range drillInstances(c, mgr, nil) {
		if got := results(inst.name); got != inst.results {
			t.Fatalf("%s: the API reads %p, drills record in %p", inst.name, got, inst.results)
		}
	}
	c.Agents.ControlDatabase = ""
	if got := results("app"); got != nil {
		t.Fatal("no control database: nil, so the API reads the database's own pool")
	}
	if got := restoreDrillResults(nil, nil, nil)("app"); got != nil {
		t.Fatal("no config: nil")
	}
}

func TestRestoreDrillResults_StandaloneMatchesTheDrillPass(t *testing.T) {
	mgr, pools := managerWith(t, "app")
	c := &config.Config{}
	inst := drillInstances(c, mgr, nil)
	if len(inst) != 1 || inst[0].results != pools["app"] {
		t.Fatalf("drill pass = %+v, want app's own pool", inst)
	}
	if got := restoreDrillResults(c, mgr, nil)("app"); got != nil {
		t.Fatal("standalone: nil, the API's fallback to app's own pool")
	}
}

func TestMCPDependenciesWireRestoreDrills(t *testing.T) {
	prev := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = prev })
	if mcpDependencies().RestoreDrills == nil {
		t.Fatal("restore_drills has no backend: every call would answer unavailable")
	}
}
