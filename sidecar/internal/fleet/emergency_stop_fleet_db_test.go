package fleet

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

func configValue(t *testing.T, pool *pgxpool.Pool, key string) (string, string) {
	t.Helper()
	var value, by string
	err := pool.QueryRow(context.Background(),
		`SELECT value, COALESCE(updated_by, '') FROM sage.config
		 WHERE key = $1 AND COALESCE(database_id, 0) = 0`, key).Scan(&value, &by)
	if err != nil {
		t.Fatalf("read sage.config %s: %v", key, err)
	}
	return value, by
}

func assertDurableStop(t *testing.T, pool *pgxpool.Pool, name, scope string) {
	t.Helper()
	if value, _ := configValue(t, pool, "emergency_stop"); value != "true" {
		t.Fatalf("%s: emergency_stop = %q, want true", name, value)
	}
	if value, _ := configValue(t, pool, FleetScopeKey); value != scope {
		t.Fatalf("%s: %s = %q, want %q", name, FleetScopeKey, value, scope)
	}
	if !executor.CheckEmergencyStop(context.Background(), pool) {
		t.Fatalf("%s: executors would not see the stop", name)
	}
}

// Real Postgres: stop-all, add a database, then rebuild the manager from
// the same pools (a restart). The added database stays stopped, and it
// carries the fleet latch to a database added after the restart.
func TestFleetStopSurvivesRestartOnPostgres(t *testing.T) {
	first, added, third := isolatedStopDB(t), isolatedStopDB(t), isolatedStopDB(t)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(restartedInstance("first", first))
	if _, err := mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	addedInst := restartedInstance("added", added)
	mgr.RegisterInstance(addedInst)
	assertLatchedBy(t, mgr, addedInst, "op@example.com")
	assertDurableStop(t, added, "added", "fleet")
	if _, by := configValue(t, added, "emergency_stop"); by != "op@example.com" {
		t.Fatalf("added: emergency_stop by %q, want op@example.com", by)
	}

	rebuilt := NewManager(&config.Config{Mode: "fleet"})
	restored := restartedInstance("added", added)
	rebuilt.RegisterInstance(restored)
	late := restartedInstance("third", third)
	rebuilt.RegisterInstance(late)

	assertLatchedBy(t, rebuilt, restored, "op@example.com")
	assertLatchedBy(t, rebuilt, late, "op@example.com")
	assertDurableStop(t, third, "third", "fleet")
	if summary := rebuilt.FleetStatus().Summary; !summary.FleetStopped {
		t.Fatalf("summary = %+v, want the fleet latch restored", summary)
	}
	rebuilt.RegisterInstance(restartedInstance("first", first))
	if _, err := rebuilt.ResumeStrict("", "admin@example.com"); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	pools := map[string]*pgxpool.Pool{"first": first, "added": added, "third": third}
	for name, pool := range pools {
		if value, by := configValue(t, pool, "emergency_stop"); value != "false" ||
			by != "admin@example.com" {
			t.Fatalf("%s: emergency_stop = %q by %q, want false by admin", name, value, by)
		}
		if value, _ := configValue(t, pool, FleetScopeKey); value != "database" {
			t.Fatalf("%s: %s = %q after resume-all, want database", name, FleetScopeKey, value)
		}
	}
}

// A per-database SQL resume reads back as 'false' (like the bootstrap
// seed); the reconcile keeps a fleet-latched database stopped and writes
// its stop again.
func TestReconcileKeepsFleetLatchOverSQLResumeOnPostgres(t *testing.T) {
	first, added := isolatedStopDB(t), isolatedStopDB(t)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(restartedInstance("first", first))
	if _, err := mgr.EmergencyStopStrict("", "op@example.com"); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	inst := restartedInstance("added", added)
	mgr.RegisterInstance(inst)
	if _, err := added.Exec(context.Background(),
		`SELECT sage.emergency_stop(false, 'dba')`); err != nil {
		t.Fatalf("sql resume of one fleet member: %v", err)
	}

	mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, mgr, inst, "op@example.com")
	assertDurableStop(t, added, "added", "fleet")
}

func TestPGFleetStopStoreRoundTripWithAudit(t *testing.T) {
	pool := isolatedStopDB(t)
	store := NewPGFleetStopStore(pool)
	ctx := context.Background()

	state, err := store.ReadFleetStop(ctx)
	if err != nil || state.Stopped {
		t.Fatalf("fresh meta db: state=%+v err=%v, want running", state, err)
	}
	if err := store.WriteFleetStop(ctx, true, "op@example.com"); err != nil {
		t.Fatalf("write stop: %v", err)
	}
	state, err = store.ReadFleetStop(ctx)
	if err != nil || !state.Stopped || state.UpdatedBy != "op@example.com" ||
		state.UpdatedAt.IsZero() {
		t.Fatalf("after stop: state=%+v err=%v, want stopped by op@example.com", state, err)
	}
	if err := store.WriteFleetStop(ctx, false, "admin@example.com"); err != nil {
		t.Fatalf("write resume: %v", err)
	}
	if state, _ = store.ReadFleetStop(ctx); state.Stopped {
		t.Fatalf("after resume: %+v, want running", state)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE key = $1`, FleetStopKey).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 2 {
		t.Fatalf("%s audit rows = %d, want 2", FleetStopKey, audits)
	}
	if err := store.WriteFleetStop(ctx, true, ""); err == nil {
		t.Fatal("a fleet stop without an actor must be refused")
	}
}

func TestPGFleetStopStoreReadErrorOnClosedPool(t *testing.T) {
	pool := isolatedStopDB(t)
	store := NewPGFleetStopStore(pool)
	pool.Close()

	if _, err := store.ReadFleetStop(context.Background()); err == nil {
		t.Fatal("a closed meta pool must report a read error, not 'running'")
	}
	if NewPGFleetStopStore(nil) != nil {
		t.Fatal("no meta pool means no fleet stop store")
	}
}

// fleet-09 seed: on a freshly bootstrapped database the documented
// UPDATE matches the seeded row and every executor then reads a stop.
func TestDocumentedUpdateStopsExecutorsOnPostgres(t *testing.T) {
	pool := isolatedStopDB(t)
	ctx := context.Background()
	if executor.CheckEmergencyStop(ctx, pool) {
		t.Fatal("the bootstrap seed must read as running")
	}

	tag, err := pool.Exec(ctx, `UPDATE sage.config SET value = 'true'
		WHERE key = 'emergency_stop'`)

	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("documented UPDATE: %v rows=%d, want 1", err, tag.RowsAffected())
	}
	if !executor.CheckEmergencyStop(ctx, pool) {
		t.Fatal("the documented UPDATE must stop every executor")
	}
}

// CHECK-06 (first half): an SQL stop shows in the overview after one
// reconcile tick, attributed to the actor named in SQL.
func TestReconcileShowsSQLStopOnPostgres(t *testing.T) {
	pool := isolatedStopDB(t)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := restartedInstance("orders", pool)
	mgr.RegisterInstance(inst)
	assertRunning(t, mgr, inst)
	if _, err := pool.Exec(context.Background(),
		`SELECT sage.emergency_stop(true, 'dba-on-call')`); err != nil {
		t.Fatalf("sql stop: %v", err)
	}

	mgr.ReconcileStops(context.Background())

	assertLatchedBy(t, mgr, inst, "dba-on-call")
	if got := mgr.FleetStatus().Summary.EmergencyStoppedCount; got != 1 {
		t.Fatalf("stopped count = %d, want 1", got)
	}
	if _, err := pool.Exec(context.Background(),
		`SELECT sage.emergency_stop(false, 'dba-on-call')`); err != nil {
		t.Fatalf("sql resume: %v", err)
	}
	mgr.ReconcileStops(context.Background())
	assertRunning(t, mgr, inst)
}
