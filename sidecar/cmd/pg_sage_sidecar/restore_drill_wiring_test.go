package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/notify"
)

// drill-01a wiring: the leader's hourly pass reads each database's policy
// drill block, starts due drills on clone.provider and records them where
// receipts live; without a provider it opens recoverability_unknown.

func drillWiringFixture(t *testing.T) (*fleet.DatabaseManager, func()) {
	t.Helper()
	pool := reaperPool(t)
	ctx := context.Background()
	for _, sql := range []string{"DELETE FROM sage.restore_drills",
		"DELETE FROM sage.findings WHERE category IN ('restore_drill_failed', " +
			"'recoverability_unknown')"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "live", Pool: pool,
		Config: config.DatabaseConfig{Name: "live"}, Status: &fleet.InstanceStatus{}})
	old := fleetLeader
	fleetLeader = leader.NewElector(stubLease{held: true}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(ctx)
	return mgr, func() { fleetLeader = old }
}

func TestRestoreDrillPassWithoutAProviderOpensRecoverabilityUnknown(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	c := config.DefaultConfig()
	c.Clone.Provider = "none"
	sched := &drill.Scheduler{}
	got := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	if len(got) != 0 {
		t.Fatalf("started %v without a provider", got)
	}
	pool := mgr.GetInstance("live").Pool
	var severity string
	if err := pool.QueryRow(context.Background(), `SELECT severity FROM sage.findings
		WHERE category = 'recoverability_unknown' AND status = 'open'
		AND object_identifier = 'restore_drill:live'`).Scan(&severity); err != nil {
		t.Fatalf("recoverability_unknown: %v", err)
	}
	if severity != "info" {
		t.Fatalf("severity = %s, want info", severity)
	}
}

func TestRestoreDrillPassRecordsAFailedCreateAndPagesCritical(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	c := config.DefaultConfig()
	c.Clone = reaperCloneConfig
	c.Clone.DLEEndpoint = "http://127.0.0.1:1"
	sched := &drill.Scheduler{}
	started := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	sched.Wait()
	if len(started) != 1 || started[0] != "live" {
		t.Fatalf("started %v, want live", started)
	}
	pool := mgr.GetInstance("live").Pool
	var passed bool
	var failure, provider, kind string
	if err := pool.QueryRow(context.Background(), `SELECT passed, failure, provider, kind
		FROM sage.restore_drills WHERE database_name = 'live'`).Scan(&passed, &failure,
		&provider, &kind); err != nil {
		t.Fatalf("drill row: %v", err)
	}
	if passed || !strings.Contains(failure, "create drill clone: dle:") ||
		provider != "dle" || kind != "thin_clone" {
		t.Fatalf("row = %v %q %s %s, want a failed dle create", passed, failure, provider,
			kind)
	}
	var severity string
	if err := pool.QueryRow(context.Background(), `SELECT severity FROM sage.findings
		WHERE category = 'restore_drill_failed' AND status = 'open'`).Scan(&severity); err != nil ||
		severity != "critical" {
		t.Fatalf("restore_drill_failed = %q (%v), want critical", severity, err)
	}
	// The failed attempt is the newest: the next pass does not start another.
	again := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	if len(again) != 0 {
		t.Fatalf("a second pass right after a failure started %v", again)
	}
}

func TestRestoreDrillPassRunsOnTheLeaderOnly(t *testing.T) {
	old := fleetLeader
	defer func() { fleetLeader = old }()
	fleetLeader = leader.NewElector(stubLease{held: false}, "s", "me", time.Minute)
	_ = fleetLeader.Tick(context.Background())
	mgr, _ := managerWith(t, "app")
	c := config.DefaultConfig()
	if got := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil,
		&drill.Scheduler{}); got != nil {
		t.Fatalf("a follower started %v", got)
	}
}

// No DB: the gate and pager helpers are pure.
func TestRestoreDrillHelpers(t *testing.T) {
	agentGate.Store(nil)
	if agentGateNeedsDrill(context.Background(), "app") {
		t.Fatal("without the agent gate no drill is needed for D7")
	}
	agentGate.Store(&agentGateState{})
	t.Cleanup(func() { agentGate.Store(nil) })
	if !agentGateNeedsDrill(context.Background(), "app") {
		t.Fatal("a gate without environment bindings must take the database as production")
	}
	if drillPager(nil) != nil {
		t.Fatal("a nil dispatcher became a typed-nil pager")
	}
	if drillPager(&notify.Dispatcher{}) == nil {
		t.Fatal("a dispatcher was dropped")
	}
	if _, _, err := (drillHistories{}).LastAttempt(context.Background(), "x"); err == nil {
		t.Fatal("a database without a store read history")
	}
}
