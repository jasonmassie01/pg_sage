package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// AGENTDB-SPEC §6.2.1 wiring: the decider and the §6.2.7 hold are read at
// decision time; before governance starts (or without a control database)
// agent requests are capped at approval and agent changes fail closed.

func TestAgentGateBeforeStartCapsAndFailsClosed(t *testing.T) {
	agentGate.Store(nil)
	ref := &policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", Tool: "apply_migration"}
	_, level, stop := lateDecider{database: "orders"}.Decide(context.Background(),
		policy.ActionRequest{Principal: ref})
	if stop || level != policy.AgentUngovernedCap {
		t.Fatalf("before start = (%d, %v), want the L2 cap", level, stop)
	}
	if _, err := holdOnControl(context.Background(), ref.ID); !errors.Is(err,
		agentguard.ErrUnavailable) {
		t.Fatalf("hold before start = %v, want ErrUnavailable", err)
	}
}

func TestStartAgentGateWithoutControlStaysPostureOnly(t *testing.T) {
	agentGate.Store(nil)
	mgr, _ := managerWith(t, "app")
	startAgentGate(&config.Config{}, mgr, nil, nil)
	if agentGate.Load() != nil {
		t.Fatal("no control database: governance must not start")
	}
}

func TestStartAgentGateDecidesThroughTheControlDatabase(t *testing.T) {
	agentGate.Store(nil)
	t.Cleanup(func() { agentGate.Store(nil) })
	mgr, pools := managerWith(t, "app", "governance")
	c := &config.Config{Agents: config.AgentsConfig{ControlDatabase: "governance"}}
	startAgentGate(c, mgr, nil, nil)
	state := agentGate.Load()
	if state == nil || state.control != pools["governance"] {
		t.Fatal("the decider runs on the pinned control database")
	}
	// The control database is unreachable here: D1 fails closed.
	ref := &policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", Tool: "apply_migration"}
	d, _, stop := lateDecider{database: "app"}.Decide(context.Background(),
		policy.ActionRequest{Principal: ref, SQL: "CREATE TABLE app.t (id int)"})
	if !stop || d.Verdict != policy.VerdictBlocked {
		t.Fatalf("unreachable control database = %+v stop=%v, want blocked", d, stop)
	}
}

func TestBindStdioPrincipalUnresolvedStaysUnbound(t *testing.T) {
	mgr, _ := managerWith(t, "app")
	c := &config.Config{}
	c.MCP.StdioPrincipal = "coder"
	// No control database: the name cannot resolve; nothing panics and the
	// runtime is left unbound (nil runtime is a no-op too).
	bindStdioPrincipal(nil, c, mgr, nil)
}

func TestFleetRecoveryUnknownDatabaseHasNoPITR(t *testing.T) {
	mgr, _ := managerWith(t, "app")
	pitr, drill, err := fleetRecovery{mgr: mgr}.Recovery(context.Background(), "missing")
	if pitr || !drill.IsZero() || err != nil {
		t.Fatalf("unknown database = (%v, %v, %v), want no PITR", pitr, drill, err)
	}
}

func TestWALArchivedReadsSettings(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set; real-Postgres recovery posture")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var mode string
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('archive_mode')").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	pitr, _, err := walArchived(context.Background(), pool)
	if err != nil {
		t.Fatalf("walArchived: %v", err)
	}
	if mode == "off" && pitr {
		t.Fatal("archive_mode off must not count as PITR")
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "live", Pool: pool,
		Config: config.DatabaseConfig{Name: "live"}, Status: &fleet.InstanceStatus{}})
	got, _, err := fleetRecovery{mgr: mgr}.Recovery(context.Background(), "live")
	if err != nil || got != pitr {
		t.Fatalf("fleetRecovery = (%v, %v), want %v", got, err, pitr)
	}
}
