package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrent-access tests: these helpers read their arguments only.
// The pinned-database integration case is TestPostgresStore_PinnedControl*
// (internal/leader), on real PostgreSQL.

func pinnedFleet(pin string) *config.Config {
	c := &config.Config{Mode: "fleet", Databases: []config.DatabaseConfig{
		{Name: "db0"}, {Name: "db1"}, {Name: "db2"}}}
	c.FleetLearning.ControlDatabase = pin
	return c
}

func TestFleetStartOrder_PinFirstOthersInYAMLOrder(t *testing.T) {
	dbs := pinnedFleet("").Databases
	got := startOrderNames(fleetStartOrder(dbs, "db1"))
	if got != "db1,db0,db2" {
		t.Fatalf("order = %s, want db1,db0,db2", got)
	}
	if got := startOrderNames(fleetStartOrder(dbs, "")); got != "db0,db1,db2" {
		t.Fatalf("unpinned order = %s, want YAML order", got)
	}
	if got := fleetStartOrder(nil, "db1"); len(got) != 0 {
		t.Fatalf("empty fleet = %v", got)
	}
	if dbs[0].Name != "db0" {
		t.Fatal("fleetStartOrder must not reorder its input")
	}
}

func startOrderNames(dbs []config.DatabaseConfig) string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, d.Name)
	}
	return strings.Join(out, ",")
}

func TestElectionControl_PinnedConnectedUsesControlPool(t *testing.T) {
	c := pinnedFleet("db1")
	mgr := fleet.NewManager(c)
	p1 := lazyPool(t, "db1")
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db1", Pool: p1})
	pool, refusal := electionControl(c, mgr, p1)
	if pool != p1 || refusal != "" {
		t.Fatalf("pool=%p refusal=%q, want db1's pool", pool, refusal)
	}
}

func TestElectionControl_PinnedDownNeverFallsBack(t *testing.T) {
	c := pinnedFleet("db1")
	mgr := fleet.NewManager(c)
	p2 := lazyPool(t, "db2")
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db1"}) // failed: no pool
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db2", Pool: p2})
	for _, control := range []*pgxpool.Pool{p2, nil} {
		pool, refusal := electionControl(c, mgr, control)
		if pool != nil {
			t.Fatalf("pool = %p, want none: the lease never moves off db1", pool)
		}
		if !strings.Contains(refusal, `fleet_learning.control_database "db1"`) ||
			!strings.Contains(refusal, "restart this sidecar") {
			t.Fatalf("refusal = %q, want the pinned name and the remedy", refusal)
		}
	}
	if _, refusal := electionControl(c, nil, p2); refusal == "" {
		t.Fatal("no fleet manager must refuse, not fall back")
	}
}

func TestElectionControl_UnpinnedMetaAndNilKeepControlPool(t *testing.T) {
	p := lazyPool(t, "x")
	meta := pinnedFleet("db1")
	meta.MetaDB = "postgres://meta"
	for name, c := range map[string]*config.Config{"unpinned": pinnedFleet(""),
		"meta": meta, "nil": nil} {
		if pool, refusal := electionControl(c, nil, p); pool != p || refusal != "" {
			t.Errorf("%s: pool=%p refusal=%q, want the control pool", name, pool, refusal)
		}
	}
}

func TestUnpinnedYAMLFleet(t *testing.T) {
	meta := pinnedFleet("")
	meta.MetaDB = "postgres://meta"
	std := &config.Config{Mode: "standalone"}
	cases := map[string]struct {
		c    *config.Config
		want bool
	}{
		"unpinned fleet": {pinnedFleet(""), true},
		"pinned fleet":   {pinnedFleet("db1"), false},
		"meta":           {meta, false},
		"standalone":     {std, false},
		"nil":            {nil, false},
	}
	for name, tc := range cases {
		if got := unpinnedYAMLFleet(tc.c); got != tc.want {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
		}
	}
}

func TestStartFleetElector_PinnedDownIsRefusedFollower(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = pinnedFleet("db1")
	cfg.FleetLearning.LeaderLeaseSeconds = 30
	mgr := fleet.NewManager(cfg)
	p2 := lazyPool(t, "db2")
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db2", Pool: p2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := startFleetElector(ctx, p2, mgr, "fleet:test")
	s := e.Status()
	if !s.Enabled || s.Leader || !s.LeadershipRefused ||
		!strings.Contains(s.RefusedReason, `"db1"`) {
		t.Fatalf("status = %+v, want an enabled, refused follower naming db1", s)
	}
	if _, ok := e.Fence(); ok {
		t.Fatal("a refused elector must not fence writes")
	}
}
