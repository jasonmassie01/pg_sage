package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// forecast-09 on real PostgreSQL: three databases of one cluster, each
// with its own sage schema and a runtime whose samples show one slot
// filling. Without ownership each opens the slot's finding (today's
// duplication); with a shared ClusterOwners exactly one does, the copies
// resolve naming the owner, and killing the owner hands the finding to
// the next database.

// fleetDB is one database of the test cluster with its monitor.
type fleetDB struct {
	name string
	pool *pgxpool.Pool
	st   *fakeStarter
}

// fleetDatabases creates n bootstrapped databases on the test server.
func fleetDatabases(t *testing.T, ctx context.Context, n int) []*fleetDB {
	t.Helper()
	out := make([]*fleetDB, 0, n)
	for i := range n {
		// ci-15: a copy of the bootstrapped fixture template, so the three
		// bootstraps below only check.
		dsn := testdb.CreateBootstrappedDatabase(t, fmt.Sprintf("rw09_%c", 'a'+i))
		pcfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		name := pcfg.ConnConfig.Database
		pool, err := pgxpool.NewWithConfig(ctx, pcfg)
		if err != nil {
			t.Fatalf("connect to %s: %v", name, err)
		}
		t.Cleanup(pool.Close)
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %s: %v", name, err)
		}
		seedSlotTrend(t, ctx, pool, "fill_slot")
		out = append(out, &fleetDB{name: name, pool: pool, st: &fakeStarter{}})
	}
	return out
}

// seedSlotTrend writes 12 samples over an hour of a slot retaining 1 kB/s
// more WAL, reaching its limit in about 55 hours (a warning).
func seedSlotTrend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slot string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, limit_value)
		SELECT 'wal_slot', $1, 'seed', now() - make_interval(mins => 60 - i * 5),
		       1e9 + i * 300 * 1000, 1.2e9
		FROM generate_series(0, 11) i`, slot); err != nil {
		t.Fatalf("seed slot trend: %v", err)
	}
}

func (d *fleetDB) monitor(t *testing.T, owners *ClusterOwners,
	runner ProbeRunner) *Monitor {
	t.Helper()
	opts := testOptions()
	opts.Database, opts.Retention, opts.Interval = d.name, 48*time.Hour, time.Minute
	opts.Owners = owners
	if runner == nil {
		runner = liveRunner(d.pool)
	}
	m, err := NewMonitor(d.pool, runner, d.st, opts, func(string, string, ...any) {})
	if err != nil {
		t.Fatalf("NewMonitor %s: %v", d.name, err)
	}
	return m
}

// slotRows counts each database's open fill_slot findings and returns
// the open owner's detail.
func slotRows(t *testing.T, ctx context.Context, dbs []*fleetDB) ([]int, string) {
	t.Helper()
	counts := make([]int, len(dbs))
	owner := ""
	for i, d := range dbs {
		rows, err := d.pool.Query(ctx, `SELECT COALESCE(detail ->> 'owner_database', '')
			FROM sage.findings WHERE category = $1 AND object_identifier = 'slot:fill_slot'
			AND status = 'open'`, CategoryWAL)
		if err != nil {
			t.Fatalf("count %s: %v", d.name, err)
		}
		owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("count %s: %v", d.name, err)
		}
		counts[i] = len(owners)
		for _, o := range owners {
			owner = o
		}
	}
	return counts, owner
}

func tickFleet(t *testing.T, ctx context.Context, ms []*Monitor) []TickResult {
	t.Helper()
	out := make([]TickResult, 0, len(ms))
	for _, m := range ms {
		res, err := m.Tick(ctx)
		if err != nil {
			t.Fatalf("tick %s: %v", m.opts.Database, err)
		}
		out = append(out, res)
	}
	return out
}

// investigationKeys are the distinct investigations asked for: the
// coordinator coalesces a repeated idempotency key into one.
func investigationKeys(dbs []*fleetDB) map[string]bool {
	keys := map[string]bool{}
	for _, d := range dbs {
		for _, tr := range d.st.started() {
			keys[d.name+"/"+tr.IdempotencyKey] = true
		}
	}
	return keys
}

func startedTotal(dbs []*fleetDB) int {
	n := 0
	for _, d := range dbs {
		n += len(d.st.started())
	}
	return n
}

// CHECK-02 and the upgrade path: today three findings and three
// investigations; with ownership the copies close naming the owner and
// one finding with one investigation remains.
func TestOwnerDB_ThreeDatabasesReportOneSlotFinding(t *testing.T) {
	_, ctx := livePool(t)
	dbs := fleetDatabases(t, ctx, 3)
	var solo []*Monitor
	for _, d := range dbs {
		solo = append(solo, d.monitor(t, nil, nil))
	}
	tickFleet(t, ctx, solo)
	if counts, _ := slotRows(t, ctx, dbs); fmt.Sprint(counts) != "[1 1 1]" ||
		startedTotal(dbs) != 3 {
		t.Fatalf("without ownership open %v started %d, want the duplication [1 1 1] "+
			"and 3", counts, startedTotal(dbs))
	}
	owners := NewClusterOwners()
	var ms []*Monitor
	for _, d := range dbs {
		d.st.triggers = nil
		ms = append(ms, d.monitor(t, owners, nil))
	}
	res := tickFleet(t, ctx, ms)
	counts, owner := slotRows(t, ctx, dbs)
	if fmt.Sprint(counts) != "[1 0 0]" || owner != dbs[0].name {
		t.Fatalf("with ownership open %v owner %q, want [1 0 0] owned by %s", counts,
			owner, dbs[0].name)
	}
	if res[0].ClusterOwner != dbs[0].name || res[1].Duplicates != 1 ||
		res[2].Duplicates != 1 || startedTotal(dbs) != 1 ||
		len(dbs[0].st.started()) != 1 {
		t.Fatalf("results %+v started %d, want one owner start and two duplicates closed",
			res, startedTotal(dbs))
	}
	var detail string
	if err := dbs[1].pool.QueryRow(ctx, `SELECT detail ->> 'cluster_owner'
		FROM sage.findings WHERE object_identifier = 'slot:fill_slot'
		AND status = 'resolved'`).Scan(&detail); err != nil || detail != dbs[0].name {
		t.Fatalf("resolved copy cluster_owner = %q (%v)", detail, err)
	}
	tickFleet(t, ctx, ms)
	if counts, _ := slotRows(t, ctx, dbs); fmt.Sprint(counts) != "[1 0 0]" ||
		len(investigationKeys(dbs)) != 1 {
		t.Fatalf("second pass open %v investigations %v, want one of each (the owner "+
			"asks again each tick under the same idempotency key, which coalesces)",
			counts, investigationKeys(dbs))
	}
}

// CHECK-03 and the owner-error rule: a failing owner leaves the copies
// open; a killed owner hands the open finding to the next database
// within one interval.
func TestOwnerDB_FailingOrKilledOwnerNeverHidesTheRunway(t *testing.T) {
	_, ctx := livePool(t)
	dbs := fleetDatabases(t, ctx, 3)
	for _, d := range dbs[1:] { // copies from before the upgrade
		if _, err := d.monitor(t, nil, nil).Tick(ctx); err != nil {
			t.Fatalf("pre-upgrade tick: %v", err)
		}
	}
	owners := NewClusterOwners()
	failing := &probeFailer{inner: liveRunner(dbs[0].pool),
		fail: map[probes.ID]bool{probes.RunwayTrendsProbe: true}}
	a := dbs[0].monitor(t, owners, failing)
	b, c := dbs[1].monitor(t, owners, nil), dbs[2].monitor(t, owners, nil)
	if _, err := a.Tick(ctx); err == nil {
		t.Fatal("the failing owner's tick reported no error")
	}
	tickFleet(t, ctx, []*Monitor{b, c})
	if counts, _ := slotRows(t, ctx, dbs); fmt.Sprint(counts) != "[0 1 1]" {
		t.Fatalf("failing owner: open %v, want the copies [0 1 1] kept", counts)
	}
	delete(failing.fail, probes.RunwayTrendsProbe)
	tickFleet(t, ctx, []*Monitor{a, b, c})
	if counts, _ := slotRows(t, ctx, dbs); fmt.Sprint(counts) != "[1 0 0]" {
		t.Fatalf("recovered owner: open %v, want [1 0 0]", counts)
	}
	runCtx, kill := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.Run(runCtx) }()
	kill()
	<-done
	res := tickFleet(t, ctx, []*Monitor{b, c})
	counts, owner := slotRows(t, ctx, dbs)
	if fmt.Sprint(counts) != "[1 1 0]" || owner != dbs[1].name ||
		res[0].ClusterOwner != dbs[1].name {
		t.Fatalf("after the owner was killed open %v owner %q (%+v), want the finding "+
			"open in %s too", counts, owner, res, dbs[1].name)
	}
}
