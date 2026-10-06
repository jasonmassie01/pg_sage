package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestFleetLearningScope(t *testing.T) {
	meta := &config.Config{Mode: "fleet", MetaDB: "postgres://meta"}
	if got := fleetLearningScope(meta); got != "meta" {
		t.Fatalf("meta scope = %q", got)
	}
	a := &config.Config{Mode: "fleet", Databases: []config.DatabaseConfig{
		{Name: "orders"}, {Name: "billing"}}}
	b := &config.Config{Mode: "fleet", Databases: []config.DatabaseConfig{
		{Name: "billing"}, {Name: "orders"}}}
	c := &config.Config{Mode: "fleet", Databases: []config.DatabaseConfig{
		{Name: "billing"}, {Name: "other"}}}
	sa, sb, sc := fleetLearningScope(a), fleetLearningScope(b), fleetLearningScope(c)
	if !strings.HasPrefix(sa, "fleet:") || sa != sb {
		t.Fatalf("same fleet in another order = %q vs %q, want one scope", sa, sb)
	}
	if sa == sc {
		t.Fatal("different fleets must not share a scope (and a leader)")
	}
	if strings.Contains(sa, "orders") || strings.Contains(sa, "billing") {
		t.Fatalf("scope %q exposes database names", sa)
	}
	std := &config.Config{Mode: "standalone"}
	std.Postgres.Database = "app"
	if got := fleetLearningScope(std); got != "standalone:app" {
		t.Fatalf("standalone scope = %q", got)
	}
	if got := fleetLearningScope(nil); got != "" {
		t.Fatalf("nil config scope = %q", got)
	}
}

func TestFleetLeaderGate(t *testing.T) {
	prev := fleetLeader
	t.Cleanup(func() { fleetLeader = prev })
	fleetLeader = nil
	if !fleetLeaderAllows("approval cards") {
		t.Fatal("without election every sidecar runs fleet-wide jobs")
	}
	fleetLeader = leader.NewElector(stubLease{held: false}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(context.Background())
	if fleetLeaderAllows("approval cards") {
		t.Fatal("a follower must not run fleet-wide jobs")
	}
	fleetLeader = leader.NewElector(stubLease{held: true}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(context.Background())
	if !fleetLeaderAllows("approval cards") {
		t.Fatal("the leader runs fleet-wide jobs")
	}
}

type stubLease struct{ held bool }

func (s stubLease) Acquire(_ context.Context, _, holder string,
	ttl time.Duration) (leader.Lease, bool, error) {
	if !s.held {
		return leader.Lease{Holder: "someone-else", Epoch: 4,
			ExpiresAt: time.Now().Add(ttl)}, false, nil
	}
	return leader.Lease{Holder: holder, Epoch: 1, ExpiresAt: time.Now().Add(ttl)}, true, nil
}

func (s stubLease) Release(context.Context, string, string) error { return nil }

func TestFleetLearningSourcesCarryBoundaries(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	pool := &pgxpool.Pool{}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "agentdb:d1", Pool: pool,
		Config: config.DatabaseConfig{Tags: []string{"agentdb", "local", "ten-1"}},
		Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "nopool",
		Status: &fleet.InstanceStatus{}})
	got := fleetLearningSources(mgr)()
	if len(got) != 2 {
		t.Fatalf("sources = %+v, want the two with pools", got)
	}
	byName := map[string]string{}
	for _, s := range got {
		byName[s.Name] = s.Boundary
	}
	if byName["agentdb:d1"] != "agentdb-tenant:ten-1" || byName["orders"] != "" {
		t.Fatalf("boundaries = %v", byName)
	}
	if fleetLearningSources(nil)() != nil {
		t.Fatal("nil manager has no sources")
	}
}

func TestMeasureFleetNeeds(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "need_busy")
	ctx := context.Background()
	busy, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(busy.Close)
	if err := schema.Bootstrap(ctx, busy); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	_, err = busy.Exec(ctx, `INSERT INTO sage.incidents (severity, signal_ids,
		root_cause, causal_chain, affected_objects, confidence, source,
		database_name) VALUES ('critical','{}','x','[]','{}',0.9,'deterministic','busy')`)
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "busy", Pool: busy,
		Status: &fleet.InstanceStatus{FindingsOpen: 30, FindingsCritical: 2}})
	// idle has no pool yet (still connecting): need from its status only.
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "idle",
		Status: &fleet.InstanceStatus{}})
	weights := measureFleetNeeds(ctx, mgr)
	if len(weights) != 2 {
		t.Fatalf("weights = %v", weights)
	}
	if weights["busy"] <= weights["idle"] {
		t.Fatalf("busy %v must outweigh idle %v", weights["busy"], weights["idle"])
	}
	if weights["idle"] != 1 {
		t.Fatalf("idle weight %v, want exactly the base 1", weights["idle"])
	}
	if got := measureFleetNeeds(ctx, nil); len(got) != 0 {
		t.Fatalf("nil manager = %v", got)
	}
}

func TestApplyFleetBudgetSplit(t *testing.T) {
	prevCfg, prevBudget := cfg, fleetLLMBudget
	t.Cleanup(func() { cfg, fleetLLMBudget = prevCfg, prevBudget })
	cfg = config.DefaultConfig()
	fleetLLMBudget = fleet.NewBudget(1000, []string{"a", "b"})
	applyFleetBudgetSplit(map[string]float64{"a": 9, "b": 1})
	if fleetLLMBudget.Split() != fleet.SplitNeed ||
		fleetLLMBudget.Allocation("a") <= fleetLLMBudget.Allocation("b") {
		t.Fatalf("need split not applied: a=%d b=%d split=%s",
			fleetLLMBudget.Allocation("a"), fleetLLMBudget.Allocation("b"),
			fleetLLMBudget.Split())
	}
	cfg.FleetLearning.BudgetSplit = "even"
	applyFleetBudgetSplit(map[string]float64{"a": 9, "b": 1})
	if fleetLLMBudget.Allocation("a") != 500 {
		t.Fatalf("even split = %d", fleetLLMBudget.Allocation("a"))
	}
	fleetLLMBudget = nil
	applyFleetBudgetSplit(map[string]float64{"a": 1}) // no budget: no panic
}
