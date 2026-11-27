package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// spec §6.2.1 wiring: the decider and the §6.2.7 hold are read at
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
	dsn := testdb.SkipUnlessLive(t)
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

type objectsFunc func() error

func (f objectsFunc) CheckObjects(context.Context, agentguard.Principal, string,
	envbind.Env, []decide.Object) error {
	return f()
}

func TestAllObjectsNeedsEveryCheckerToPass(t *testing.T) {
	calls := 0
	pass := objectsFunc(func() error { calls++; return nil })
	deny := objectsFunc(func() error {
		calls++
		return &agentguard.DeniedError{Reason: "agent_classification"}
	})
	ctx := context.Background()
	if err := (allObjects{pass, nil, pass}).CheckObjects(ctx, agentguard.Principal{}, "db",
		envbind.EnvDev, nil); err != nil || calls != 2 {
		t.Fatalf("all pass: err %v after %d calls", err, calls)
	}
	calls = 0
	err := (allObjects{pass, deny, pass}).CheckObjects(ctx, agentguard.Principal{}, "db",
		envbind.EnvDev, nil)
	if d, ok := agentguard.IsDenied(err); !ok || d.Reason != "agent_classification" ||
		calls != 2 {
		t.Fatalf("a denial wins and stops: err %v after %d calls", err, calls)
	}
}

// drill-01a (market-07 CHECK-17): D7's drill time is the last passed
// restore drill, on both PITR paths; zero only when no drill passed.

type drillTimes struct {
	last  time.Time
	err   error
	calls []string
}

func (d *drillTimes) LastPassed(_ context.Context, database string) (time.Time, error) {
	d.calls = append(d.calls, database)
	return d.last, d.err
}

func pitrOn(string) *bool { on := true; return &on }

func TestFleetRecoveryCloudPostureReturnsTheLastPassedDrill(t *testing.T) {
	passed := time.Date(2026, 11, 10, 3, 4, 5, 0, time.UTC)
	drills := &drillTimes{last: passed}
	pitr, drill, err := fleetRecovery{drills: drills, cloudPITR: pitrOn}.
		Recovery(context.Background(), "orders")
	if err != nil || !pitr || !drill.Equal(passed) {
		t.Fatalf("cloud posture = (%v, %v, %v), want PITR and the drill time %v", pitr,
			drill, err, passed)
	}
	if len(drills.calls) != 1 || drills.calls[0] != "orders" {
		t.Fatalf("drills read for %v, want orders", drills.calls)
	}
}

func TestFleetRecoveryWithoutAPassedDrillIsZero(t *testing.T) {
	for name, drills := range map[string]drillHistory{"no store": nil,
		"never passed": &drillTimes{},
		"read error":   &drillTimes{err: errors.New("connection refused")}} {
		pitr, drill, err := fleetRecovery{drills: drills, cloudPITR: pitrOn}.
			Recovery(context.Background(), "orders")
		if err != nil || !pitr || !drill.IsZero() {
			t.Errorf("%s = (%v, %v, %v), want PITR with a zero drill time (L2 cap)", name,
				pitr, drill, err)
		}
	}
}

func TestFleetRecoveryWALPathReturnsTheLastPassedDrill(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "live", Pool: pool,
		Config: config.DatabaseConfig{Name: "live"}, Status: &fleet.InstanceStatus{}})
	passed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	noCloud := func(string) *bool { return nil }
	_, drill, err := fleetRecovery{mgr: mgr, drills: &drillTimes{last: passed},
		cloudPITR: noCloud}.Recovery(context.Background(), "live")
	if err != nil || !drill.Equal(passed) {
		t.Fatalf("WAL path drill = %v (%v), want %v", drill, err, passed)
	}
	_, drill, err = fleetRecovery{mgr: mgr, drills: &drillTimes{}, cloudPITR: noCloud}.
		Recovery(context.Background(), "live")
	if err != nil || !drill.IsZero() {
		t.Fatalf("WAL path without a pass = %v (%v), want zero", drill, err)
	}
}
