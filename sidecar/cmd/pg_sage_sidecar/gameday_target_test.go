package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// benches-02: game days and local bench runs refuse a local target on the
// cluster of any monitored database or of the control (metadata)
// database, and the monitored set is read at each run, so a fleet
// database added after start is refused too.

// dsnPool is a pool on dsn; it connects only when first used.
func dsnPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func withFleet(t *testing.T, m *fleet.DatabaseManager) {
	t.Helper()
	prev := fleetMgr
	t.Cleanup(func() { fleetMgr = prev })
	fleetMgr = m
}

func refusedHas(t *testing.T, g localTargetGuard, want *pgxpool.Pool) bool {
	t.Helper()
	pools, err := g.pools(context.Background())
	if err != nil {
		t.Fatalf("refused pools: %v", err)
	}
	for _, q := range pools {
		if q == gameday.Querier(want) {
			return true
		}
	}
	return false
}

func TestLocalTargetGuardRefusesMetadataAndLateFleetPools(t *testing.T) {
	withFleet(t, nil)
	own := dsnPool(t, "postgres://app:pw@db.prod:5432/orders")
	meta := dsnPool(t, "postgres://sage:pw@meta.internal:5432/sage_meta")
	g := localTargetGuardFor(nil, meta)
	if !refusedHas(t, g, meta) {
		t.Fatal("with no monitored pools the metadata pool must still be refused")
	}
	g = localTargetGuardFor(own, meta)
	settings := config.DefaultConfig().SRE.Autonomy
	settings.GameDays.Enabled = true
	settings.GameDays.LocalDSN = meta.Config().ConnString()
	none := config.CloneProviderConfig{Provider: "none"}
	if _, _, err := gameDayProvider(settings.GameDays, none, g); !errors.Is(err,
		gameday.ErrMonitoredDSN) {
		t.Fatalf("game days on the metadata DSN: err = %v, want ErrMonitoredDSN", err)
	}
	if _, _, err := localBenchProvider(settings, none, g); !errors.Is(err,
		gameday.ErrMonitoredDSN) {
		t.Fatalf("local bench on the metadata DSN: err = %v, want ErrMonitoredDSN", err)
	}
	m := fleet.NewManager(&config.Config{Mode: "fleet"})
	withFleet(t, m)
	late := dsnPool(t, "postgres://app:pw@late.internal:5432/late")
	if refusedHas(t, g, late) {
		t.Fatal("a pool that is not registered yet is refused")
	}
	m.RegisterInstance(&fleet.DatabaseInstance{Name: "late", Pool: late,
		Status: &fleet.InstanceStatus{}})
	if !refusedHas(t, g, late) || !refusedHas(t, g, own) || !refusedHas(t, g, meta) {
		t.Fatal("the refused pools are not read at each call")
	}
}

func markGameDayTarget(t *testing.T, admin *pgxpool.Pool, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "COMMENT ON DATABASE "+
		pgx.Identifier{cfg.Database}.Sanitize()+" IS 'pg_sage:gameday_target'"); err != nil {
		t.Fatalf("mark game-day target: %v", err)
	}
}

// A fleet database registered after start is refused at Create: the
// target (a marked database on the second cluster) is accepted while only
// the control database on the test server is refused, and refused once a
// fleet database on the target's cluster joins.
func TestGameDayRefusesAFleetPoolAddedAfterStart(t *testing.T) {
	withFleet(t, fleet.NewManager(&config.Config{Mode: "fleet"}))
	meta := autonomyPool(t)
	target := testdb.CreateDatabaseOnSecondCluster(t, "gd_target")
	markGameDayTarget(t, dsnPool(t, target), target)
	late := dsnPool(t, testdb.CreateDatabaseOnSecondCluster(t, "gd_late"))
	settings := config.DefaultConfig().SRE.Autonomy.GameDays
	settings.Enabled, settings.LocalDSN = true, target
	p, name, err := gameDayProvider(settings, config.CloneProviderConfig{Provider: "none"},
		localTargetGuardFor(nil, meta))
	if err != nil || p == nil || name != "local" {
		t.Fatalf("provider = %v %q (%v)", p, name, err)
	}
	ctx := context.Background()
	if _, err := p.Create(ctx, clone.CloneSpec{}); err != nil {
		t.Fatalf("before the fleet database joined: %v", err)
	}
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "late", Pool: late,
		Status: &fleet.InstanceStatus{}})
	if _, err := p.Create(ctx, clone.CloneSpec{}); !errors.Is(err, gameday.ErrMonitoredDSN) {
		t.Fatalf("after the fleet database joined: err = %v, want ErrMonitoredDSN", err)
	}
}

// CHECK-03 and CHECK-04: a local bench run aimed at a sibling of the
// control database fails before any fault program, and the cluster's
// max_wal_size and replication slots are unchanged.
func TestLocalBenchRefusesTheControlDatabaseCluster(t *testing.T) {
	withFleet(t, nil)
	pool := autonomyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, err := newAutonomyLedgers(true).ledgerFor(ctx, pool, "orders",
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	sibling := testdb.CreateDatabase(t, "gd_bench")
	markGameDayTarget(t, pool, sibling)
	state := func() (string, int) {
		var walSize string
		var slots int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT setting FROM pg_catalog.pg_settings WHERE name = 'max_wal_size'),
			(SELECT count(*)::int FROM pg_catalog.pg_replication_slots
			  WHERE slot_name LIKE 'sb\_%')`).Scan(&walSize, &slots); err != nil {
			t.Fatal(err)
		}
		return walSize, slots
	}
	walBefore, slotsBefore := state()
	settings := config.DefaultConfig().SRE.Autonomy
	settings.GameDays.LocalDSN = sibling
	bench, err := newLocalBenchFor("orders", svc, settings,
		config.CloneProviderConfig{Provider: "none"}, localTargetGuardFor(nil, pool))
	if err != nil || bench == nil {
		t.Fatalf("local bench = %v (%v)", bench, err)
	}
	run, err := bench.Run(ctx, []string{"sequence_runway"})
	if !errors.Is(err, gameday.ErrMonitoredDSN) || run.Status != gameday.StatusFailed ||
		run.EvalRunID != "" {
		t.Fatalf("local run on the control cluster = %+v (%v)", run, err)
	}
	if walAfter, slotsAfter := state(); walAfter != walBefore || slotsAfter != slotsBefore {
		t.Fatalf("cluster changed: max_wal_size %s -> %s, sb_ slots %d -> %d", walBefore,
			walAfter, slotsBefore, slotsAfter)
	}
	settings.GameDays.LocalDSN = pool.Config().ConnString()
	if _, err := newLocalBenchFor("orders", svc, settings,
		config.CloneProviderConfig{Provider: "none"},
		localTargetGuardFor(nil, pool)); !errors.Is(err, gameday.ErrMonitoredDSN) {
		t.Fatalf("the control DSN itself: err = %v, want ErrMonitoredDSN", err)
	}
}
