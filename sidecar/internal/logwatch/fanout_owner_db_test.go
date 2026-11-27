//go:build integration

package logwatch

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-08 against a real sage.incidents store: a fleet of three databases
// on one cluster, fed by one fanout, persists one incident per cluster
// event (CHECK-24) and keeps each database line on its own database.

type countingDispatcher struct {
	mu     sync.Mutex
	events []notify.Event
}

func (d *countingDispatcher) Dispatch(_ context.Context, e notify.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
	return nil
}

func (d *countingDispatcher) detected() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, e := range d.events {
		if e.Type == "incident_detected" {
			n++
		}
	}
	return n
}

func fleetDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// fleetEngines subscribes one engine per name to the fanout, each bound to
// its own database name and log database "<name>_db".
func fleetEngines(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *LogFanout,
	d rca.EventDispatcher, names ...string,
) map[string]*rca.Engine {
	t.Helper()
	engs := map[string]*rca.Engine{}
	for _, name := range names {
		db := "rca08_" + name
		if _, err := pool.Exec(ctx,
			"DELETE FROM sage.incidents WHERE database_name = $1", db); err != nil {
			t.Fatalf("clean %s: %v", db, err)
		}
		eng := rca.NewEngine(&config.RCAConfig{Enabled: true, DedupWindowMinutes: 30,
			EscalationCycles: 5, ResolutionCycles: 2, ConnectionSaturationPct: 80},
			func(string, string, ...any) {})
		eng.WithDatabaseName(db)
		eng.WithLogDatabase(name + "_db")
		eng.WithDispatcher(d)
		eng.SetLogSource(f.Subscribe(name))
		engs[name] = eng
	}
	return engs
}

func runFleetCycle(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	engs map[string]*rca.Engine, snap *collector.Snapshot,
) {
	t.Helper()
	cfg := &config.Config{RCA: config.RCAConfig{Enabled: true,
		ConnectionSaturationPct: 80}}
	for name, eng := range engs {
		eng.AnalyzeContext(ctx, snap, nil, cfg, nil)
		if err := eng.PersistIncidents(ctx, pool); err != nil {
			t.Fatalf("persist %s: %v", name, err)
		}
	}
}

// fleetIncidents returns database_name -> signal ids of each open row.
func fleetIncidents(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) map[string][][]string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT database_name, signal_ids
		FROM sage.incidents WHERE database_name LIKE 'rca08\_%'
		AND resolved_at IS NULL ORDER BY database_name`)
	if err != nil {
		t.Fatalf("query incidents: %v", err)
	}
	defer rows.Close()
	out := map[string][][]string{}
	for rows.Next() {
		var db string
		var ids []string
		if err := rows.Scan(&db, &ids); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[db] = append(out[db], ids)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// deadlockSignal is a deadlock line of database db, with the id and
// severity the watcher gives it (patterns.go), so the deadlock tree opens
// an incident for it.
func deadlockSignal(db string) *rca.Signal {
	return &rca.Signal{ID: "log_deadlock_detected", FiredAt: time.Now(),
		Severity: "critical", Metrics: map[string]any{"database": db}}
}

// CHECK-24: one cluster PANIC gives one incident row and one notification
// across the fleet, on the owner; a deadlock of database b stays on b.
func TestFleetClusterPanicOneIncidentInDB(t *testing.T) {
	pool, ctx := fleetDB(t)
	f := NewLogFanout(&FileWatcher{}).WithClusterKey("pg:5432")
	d := &countingDispatcher{}
	engs := fleetEngines(t, ctx, pool, f, d, "c", "a", "b")
	f.distribute([]*rca.Signal{panicSignal(), deadlockSignal("b_db")})
	quiet := &collector.Snapshot{CollectedAt: time.Now(), System: collector.SystemStats{
		TotalBackends: 10, ClientBackends: 5, MaxConnections: 100, CacheHitRatio: 0.999}}
	runFleetCycle(t, ctx, pool, engs, quiet)

	got := fleetIncidents(t, ctx, pool)
	panics := 0
	for db, incs := range got {
		for _, ids := range incs {
			if slices.Contains(ids, "log_panic_server_crash") {
				panics++
				if db != "rca08_a" {
					t.Errorf("PANIC incident on %s, want the owner rca08_a", db)
				}
			}
		}
	}
	if panics != 1 {
		t.Fatalf("PANIC incidents across the fleet = %d, want 1 (rows %v)", panics, got)
	}
	if len(got["rca08_b"]) != 1 || len(got["rca08_c"]) != 0 {
		t.Fatalf("rows b=%v c=%v, want b's deadlock only on b", got["rca08_b"],
			got["rca08_c"])
	}
	if n := d.detected(); n != 2 {
		t.Fatalf("incident_detected = %d, want 2 (one PANIC, one deadlock)", n)
	}
}

// Integration: a cluster-wide connection saturation seen by two databases
// of one cluster opens one connections_high incident, on the owner.
func TestFleetConnectionsHighOneIncidentInDB(t *testing.T) {
	pool, ctx := fleetDB(t)
	f := NewLogFanout(&FileWatcher{})
	engs := fleetEngines(t, ctx, pool, f, &countingDispatcher{}, "a", "b")
	engs["b"].SetClusterOwner(false)
	hot := &collector.Snapshot{CollectedAt: time.Now(), System: collector.SystemStats{
		TotalBackends: 97, ClientBackends: 90, MaxConnections: 100,
		CacheHitRatio: 0.999}}
	runFleetCycle(t, ctx, pool, engs, hot)
	got := fleetIncidents(t, ctx, pool)
	if len(got["rca08_a"]) != 1 || !slices.Contains(got["rca08_a"][0], "connections_high") ||
		len(got["rca08_b"]) != 0 {
		t.Fatalf("rows = %v, want one connections_high incident on rca08_a", got)
	}
}
