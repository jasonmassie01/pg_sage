package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestFleetFindingsMCPRestrictsToPermittedDatabases(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "fl_mcp"))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status) VALUES
		('missing_index','warning','table','public.t|btree(a)','t','{}','open')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	prevMgr, prevCfg := fleetMgr, cfg
	t.Cleanup(func() { fleetMgr, cfg = prevMgr, prevCfg })
	cfg = config.DefaultConfig()
	fleetMgr = fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range []string{"a", "b"} {
		fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Status: &fleet.InstanceStatus{}})
	}
	all, err := fleetFindingsMCP{}.FleetFindings(ctx, mcp.FleetFindingsRequest{
		MinDatabases: 2})
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if res := all.(fleetlearn.FleetFindingsResult); len(res.Findings) != 1 ||
		res.DatabasesScanned != 2 {
		t.Fatalf("unrestricted = %+v, want one finding on a and b", res)
	}
	one, _ := fleetFindingsMCP{}.FleetFindings(ctx, mcp.FleetFindingsRequest{
		MinDatabases: 2, Databases: []string{"a"}})
	if res := one.(fleetlearn.FleetFindingsResult); len(res.Findings) != 0 ||
		res.DatabasesScanned != 1 {
		t.Fatalf("restricted to a = %+v, must not read b", res)
	}
	def, _ := fleetFindingsMCP{}.FleetFindings(ctx, mcp.FleetFindingsRequest{})
	if res := def.(fleetlearn.FleetFindingsResult); res.MinDatabases != 3 {
		t.Fatalf("default minimum = %d, want the configured 3", res.MinDatabases)
	}
}

func TestFleetLearningAdaptersWithoutAService(t *testing.T) {
	prevSvc, prevLeader := fleetLearning, fleetLeader
	t.Cleanup(func() { fleetLearning, fleetLeader = prevSvc, prevLeader })
	fleetLearning, fleetLeader = nil, nil
	p, err := fleetPriorSource{}.LookalikePrior(context.Background(), "a", "guc", nil)
	if p != nil || err != nil {
		t.Fatalf("no service: prior %+v err %v, want none", p, err)
	}
	looks, err := fleetLearningAPI{}.LookAlikes(context.Background(), "a")
	if err != nil || len(looks.([]fleetlearn.LookAlike)) != 0 {
		t.Fatalf("no service: look-alikes %v err %v", looks, err)
	}
	status, ok := fleetLearningAPI{}.LeaderStatus().(leader.Status)
	if !ok || status.Enabled || !status.Leader {
		t.Fatalf("election off: status %+v, want disabled and leading", status)
	}
}
