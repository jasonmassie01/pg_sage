package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/leader"
)

// On a shared control pool governance fences on the fleet leader's lease:
// the scope comes from that elector, so a reload that changes the YAML
// fleet's database list leaves the governance fence on the live row.
func TestGovernanceFenceSharesTheFleetLeaderScope(t *testing.T) {
	preserveAuditFenceGlobals(t)
	cfg = yamlFleetConfig("a", "b")
	cfg.FleetLearning.LeaderLeaseSeconds = 30
	scope := fleetLearningScope(cfg)
	setFleetLeader(leader.NewElector(stubLease{held: true}, scope, "me", time.Minute))
	if err := currentFleetLeader().Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	g := agentGovernanceLeader(context.Background(), nil, nil)
	if g.elector != currentFleetLeader() {
		t.Fatal("a shared control pool must reuse the fleet leader's elector")
	}
	cfg = yamlFleetConfig("a", "b", "c")
	want := envbind.Fence{Scope: scope, Holder: "me", Epoch: 1}
	if f, ok := g.fence("test job"); !ok || f != want {
		t.Fatalf("after reload: fence = %+v,%v, want %+v", f, ok, want)
	}
}

// A separate governance control pool gets its own "agents:" elector; its
// fence carries that elector's scope, and a follower gets no fence.
func TestGovernanceFenceOwnElectorScope(t *testing.T) {
	preserveAuditFenceGlobals(t)
	cfg = &config.Config{MetaDB: "postgres://meta"}
	cfg.FleetLearning.LeaderLeaseSeconds = 30
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := agentGovernanceLeader(ctx, lazyPool(t, "gov"), lazyPool(t, "fleet"))
	if g.elector == nil || g.elector == currentFleetLeader() {
		t.Fatal("a separate control pool needs its own elector")
	}
	if got := g.elector.Status().Scope; got != "agents:meta" {
		t.Fatalf("governance elector scope = %q, want agents:meta", got)
	}
	e := leader.NewElector(stubLease{held: true}, "agents:meta", "me", time.Minute)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := envbind.Fence{Scope: "agents:meta", Holder: "me", Epoch: 1}
	if f, ok := (governanceLeader{elector: e}).fence("test job"); !ok || f != want {
		t.Fatalf("leader fence = %+v,%v, want %+v", f, ok, want)
	}
	follower := leader.NewElector(stubLease{held: false}, "agents:meta", "me", time.Minute)
	_ = follower.Tick(context.Background())
	if f, ok := (governanceLeader{elector: follower}).fence("test job"); ok ||
		f != (envbind.Fence{}) {
		t.Fatalf("follower fence = %+v,%v, want none", f, ok)
	}
}
