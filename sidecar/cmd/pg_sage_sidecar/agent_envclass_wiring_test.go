package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Spec §6.5: governance needs mode: meta or a pinned
// agents.control_database; otherwise it is posture-only.

func lazyPool(t *testing.T, db string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(),
		"postgres://u@127.0.0.1:1/"+db+"?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func managerWith(t *testing.T, names ...string) (*fleet.DatabaseManager,
	map[string]*pgxpool.Pool) {
	t.Helper()
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	pools := map[string]*pgxpool.Pool{}
	for _, n := range names {
		pools[n] = lazyPool(t, n)
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: n, Pool: pools[n],
			Config: config.DatabaseConfig{Name: n}, Status: &fleet.InstanceStatus{}})
	}
	return mgr, pools
}

func TestGovernanceControlPool(t *testing.T) {
	mgr, pools := managerWith(t, "app", "governance")
	meta := &metaDBState{Pool: lazyPool(t, "meta")}
	c := &config.Config{Agents: config.AgentsConfig{ControlDatabase: "governance"}}
	if got := governanceControlPool(c, mgr, meta); got != meta.Pool {
		t.Fatal("meta mode: the meta database is the control database")
	}
	if got := governanceControlPool(c, mgr, nil); got != pools["governance"] {
		t.Fatal("pinned: the named monitored database")
	}
	c.Agents.ControlDatabase = "missing"
	if got := governanceControlPool(c, mgr, nil); got != nil {
		t.Fatal("a pinned name that is not connected: posture-only")
	}
	c.Agents.ControlDatabase = ""
	if got := governanceControlPool(c, mgr, nil); got != nil {
		t.Fatal("neither meta nor pinned: posture-only")
	}
	if got := governanceControlPool(nil, nil, nil); got != nil {
		t.Fatal("no config: posture-only")
	}
}

func TestAgentEnvResolver(t *testing.T) {
	mgr, pools := managerWith(t, "app")
	resolve := agentEnvResolver(mgr)
	db, err := resolve(context.Background(), "app")
	if err != nil || db.Name != "app" || db.Pool != pools["app"] || db.ID != "" ||
		db.ProviderRef != "" {
		t.Fatalf("app: %+v %v", db, err)
	}
	if _, err := resolve(context.Background(), "nope"); !errors.Is(err,
		envbind.ErrUnknownDatabase) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := agentEnvResolver(nil)(context.Background(), "app"); !errors.Is(err,
		envbind.ErrUnknownDatabase) {
		t.Fatalf("no fleet: %v", err)
	}
}

func TestAgentEnvironmentServiceWithoutControl(t *testing.T) {
	mgr, _ := managerWith(t, "app")
	svc := agentEnvironmentService(&config.Config{}, mgr, nil)
	if svc == nil || agentEnvSvc != svc {
		t.Fatal("the service is built and shared with the reconciler")
	}
	_, _, err := svc.SetLabel(context.Background(), "app", envbind.EnvDev, "a@example.com")
	if !errors.Is(err, envbind.ErrNoControl) {
		t.Fatalf("posture-only refuses labels: %v", err)
	}
	v, err := svc.View(context.Background(), "app")
	if err != nil || v.Effective != envbind.EnvProd ||
		len(v.Reasons) != 1 || v.Reasons[0] != envbind.ReasonNoControl {
		t.Fatalf("posture-only view: %+v %v", v, err)
	}
}

func TestGovernanceLeaderFence(t *testing.T) {
	old := fleetLeader
	fleetLeader = nil
	t.Cleanup(func() { fleetLeader = old })
	f, ok := governanceLeader{scope: "s"}.fence("test job")
	if !ok || f.Holder != "" {
		t.Fatalf("no election: run unfenced: %+v %v", f, ok)
	}
}
