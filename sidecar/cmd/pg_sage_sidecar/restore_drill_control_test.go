package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// drill-01a review: in a YAML fleet the drill reads the standing policy
// the executor enforces (the global one, on the runtimes' control
// database) and pages through that database's dispatcher, for every
// database, not only the primary. No concurrent access tests: one pass
// runs at a time on the leader (scheduler_test.go covers concurrency).

// secondDatabase registers name in mgr on a pool of its own (to the same
// server: a distinct pool is what the runtime would hold) with a positive
// sage.databases id, as a YAML fleet registers every database.
func secondDatabase(t *testing.T, mgr *fleet.DatabaseManager, name string,
	id int) *pgxpool.Pool {
	t.Helper()
	primary := mgr.GetInstance("live").Pool
	pool, err := pgxpool.NewWithConfig(context.Background(), primary.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, DatabaseID: id, Pool: pool,
		Config: config.DatabaseConfig{Name: name}, Status: &fleet.InstanceStatus{}})
	return pool
}

// withGlobalDrillBlock sets the drill block of the global standing policy
// on pool and restores the document afterwards.
func withGlobalDrillBlock(t *testing.T, pool *pgxpool.Pool, block string) {
	t.Helper()
	ctx := context.Background()
	current, err := policy.NewStore(pool).BootstrapIn(ctx, policy.Scope{},
		config.DefaultPolicyProfile, "", "drill-review-test")
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT doc::text FROM sage.policy WHERE id = $1`,
		current.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.policy SET doc = doc ||
		jsonb_build_object('drill', $2::jsonb) WHERE id = $1`, current.ID, block); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE sage.policy SET doc = $2::jsonb
			WHERE id = $1`, current.ID, before)
	})
}

func openUnknown(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_identifier,
		detail->>'reason' FROM sage.findings WHERE category = 'recoverability_unknown'
		AND status = 'open'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var obj, reason string
		if err := rows.Scan(&obj, &reason); err != nil {
			t.Fatal(err)
		}
		out[obj] = reason
	}
	return out
}

func TestRestoreDrillPassHonoursTheGlobalPolicyForEveryFleetDatabase(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	secondDatabase(t, mgr, "orders", 7)
	control := mgr.GetInstance("live").Pool
	withGlobalDrillBlock(t, control, `{"enabled": false}`)
	c := config.DefaultConfig()
	c.Clone = reaperCloneConfig
	c.Clone.DLEEndpoint = "http://127.0.0.1:1"
	sched := &drill.Scheduler{}
	started := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	sched.Wait()
	if len(started) != 0 {
		t.Fatalf("drills disabled by the standing policy started on %v", started)
	}
	c.Clone.Provider = "none"
	restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	if got := openUnknown(t, control); len(got) != 0 {
		t.Fatalf("drills are disabled, yet recoverability_unknown opened: %v", got)
	}
}

func TestRestoreDrillPassPagesThroughTheRuntimeControlDispatcher(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	other := secondDatabase(t, mgr, "orders", 7)
	c := config.DefaultConfig()
	c.Clone.Provider = "none"
	restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, &drill.Scheduler{})
	got := openUnknown(t, mgr.GetInstance("live").Pool)
	if _, ok := got["restore_drill:orders"]; !ok {
		t.Fatalf("recoverability_unknown for orders missing: %v", got)
	}
	notifyDispatchersMu.Lock()
	defer notifyDispatchersMu.Unlock()
	if notifyDispatchers[other] != nil {
		t.Fatal("a dispatcher was built on orders' own pool: its alerts would miss the " +
			"control database's notification routes")
	}
	if notifyDispatchers[mgr.GetInstance("live").Pool] == nil {
		t.Fatal("no dispatcher on the control database")
	}
}

func TestRestoreDrillPassReportsAKindThePolicyDoesNotAllow(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	control := mgr.GetInstance("live").Pool
	withGlobalDrillBlock(t, control, `{"kinds": ["branch"]}`)
	c := config.DefaultConfig()
	c.Clone = reaperCloneConfig
	c.Clone.DLEEndpoint = "http://127.0.0.1:1"
	sched := &drill.Scheduler{}
	started := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	sched.Wait()
	if len(started) != 0 {
		t.Fatalf("a thin_clone drill started though drill.kinds is [branch]: %v", started)
	}
	reason := openUnknown(t, control)["restore_drill:live"]
	if !strings.Contains(reason, "thin_clone is not in the policy document's drill.kinds") {
		t.Fatalf("recoverability_unknown reason = %q, want the drill.kinds refusal", reason)
	}
}

func TestDrillPolicyOfWithoutAControlDatabaseFailsClosed(t *testing.T) {
	_, err := drillPolicyOf(context.Background(), drillInstance{name: "x"})
	if err == nil || !strings.Contains(err.Error(), "no control database") {
		t.Fatalf("err = %v, want the missing control database", err)
	}
}
