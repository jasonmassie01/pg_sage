package optimizer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-15b on a real server: the version the context carries comes
// from the catalog, the floor's output for a two-key fixture is the same
// as before skip-scan awareness (the PG 14-17 regression guard), and the
// evidence EXPLAIN runs read-only on one bounded connection.

const (
	skipDBIndexDef = "CREATE INDEX events_tenant_created ON public.events " +
		"USING btree (tenant_id, created_at)"
	skipDBCreatedStmt = "SELECT id FROM events WHERE created_at = $1"
	skipDBRangeStmt   = "SELECT id FROM events WHERE tenant_id = $1 AND created_at >= $2"
)

// skipScanDatabase is a fresh database with public.events (rows rows, 12
// tenants) and the (tenant_id, created_at) index, analyzed.
func skipScanDatabase(t *testing.T, rows int) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.CreateDatabase(t, "skip_scan"))
	if err != nil {
		t.Fatalf("connect to the skip-scan database: %v", err)
	}
	t.Cleanup(pool.Close)
	setup := `CREATE TABLE public.events (id bigserial PRIMARY KEY, tenant_id int NOT NULL,
			created_at timestamptz NOT NULL, status text NOT NULL);
		INSERT INTO public.events (tenant_id, created_at, status)
			SELECT g % 12, timestamptz '2026-01-01' + g * interval '1 second', 'done'
			FROM generate_series(1, $1::int) g;
		CREATE INDEX events_tenant_created ON public.events (tenant_id, created_at);
		ANALYZE public.events`
	for _, stmt := range strings.Split(setup, ";") {
		args := []any{}
		if strings.Contains(stmt, "$1") {
			args = append(args, rows)
		}
		if _, err := pool.Exec(t.Context(), stmt, args...); err != nil {
			t.Fatalf("skip-scan fixture: %v", err)
		}
	}
	return pool
}

func skipScanSnapshot(rows int64) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC),
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "events",
			NLiveTup: rows, SeqScan: 900, SeqTupRead: 900 * rows}},
		Indexes: []collector.IndexStats{
			{SchemaName: "public", RelName: "events", IndexRelName: "events_pkey",
				IsUnique: true, IsPrimary: true, IsValid: true,
				IndexDef: "CREATE UNIQUE INDEX events_pkey ON public.events USING btree (id)"},
			{SchemaName: "public", RelName: "events", IndexRelName: "events_tenant_created",
				IsValid: true, IndexDef: skipDBIndexDef}},
		Queries: []collector.QueryStats{
			{QueryID: 21, Calls: 800, MeanExecTime: 4, TotalExecTime: 3200,
				Query: skipDBCreatedStmt},
			{QueryID: 22, Calls: 300, MeanExecTime: 2, TotalExecTime: 600,
				Query: skipDBRangeStmt},
		},
	}
}

func serverMajorOf(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var num int
	if err := pool.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatalf("read server_version_num: %v", err)
	}
	return num / 10000
}

func TestSkipScanDB_ContextCarriesTheCatalogVersion(t *testing.T) {
	pool := skipScanDatabase(t, 20000)
	tc, ok, err := floorOptimizer(pool).TableContext(t.Context(), skipScanSnapshot(20000),
		"public.events")
	if err != nil || !ok {
		t.Fatalf("table context: ok %t err %v", ok, err)
	}
	want := serverMajorOf(t, pool)
	if tc.PGMajor != want || want < 14 {
		t.Fatalf("PGMajor = %d, want %d from server_version_num", tc.PGMajor, want)
	}
	if want < 18 && tc.LeadingKeyStats != nil {
		t.Fatalf("PG %d read leading-key statistics %+v", want, tc.LeadingKeyStats)
	}
}

// The regression guard: the floor's candidates for the two-key fixture,
// with the version read from the catalog and with it forced to 18, are
// exactly the candidate the floor proposed before market-15b.
func TestSkipScanDB_FloorOutputIsUnchanged(t *testing.T) {
	pool := skipScanDatabase(t, 20000)
	tc, ok, err := floorOptimizer(pool).TableContext(t.Context(), skipScanSnapshot(20000),
		"public.events")
	if err != nil || !ok {
		t.Fatalf("table context: ok %t err %v", ok, err)
	}
	golden := []Recommendation{{Table: "public.events",
		DDL: "CREATE INDEX CONCURRENTLY sage_events_created_at_idx ON public.events " +
			"(created_at)",
		Severity: "info", IndexType: "btree", Category: OptimizerCategory,
		Rationale: "Deterministic index floor (no tuning agent this cycle): statements " +
			"making 800 calls filter public.events on created_at, and the table is read " +
			"by sequential scans."}}
	if got := floorCandidates(tc); !reflect.DeepEqual(got, golden) {
		t.Fatalf("PG %d floor candidates = %+v, want %+v", tc.PGMajor, got, golden)
	}
	tc.PGMajor = 18
	if got := floorCandidates(tc); !reflect.DeepEqual(got, golden) {
		t.Fatalf("forced PG 18 floor candidates = %+v, want %+v", got, golden)
	}
}

// Before 18 the floor admits the created_at candidate without running the
// skip-scan EXPLAIN.
func TestSkipScanDB_BeforePG18TheFloorAdmitsWithoutEvidence(t *testing.T) {
	pool := skipScanDatabase(t, 20000)
	if serverMajorOf(t, pool) >= 18 {
		t.Skip("PostgreSQL 18+: TestSkipScanDB_PG18DropsTheServedCandidate covers this server")
	}
	o := floorOptimizer(pool)
	plans := servedPlans()
	o.skipScanPlans = plans
	res := NewFloor(o, noopLog2).Run(t.Context(), skipScanSnapshot(20000),
		[]string{"public.events"}, nil)
	if len(res.Admitted) != 1 || plans.callCount() != 0 {
		t.Fatalf("admitted %+v (EXPLAIN calls %d), want one and none", res.Admitted,
			plans.callCount())
	}
	if id := res.Admitted[0].Rec.FindingIdentifier(); id != "public.events|btree(created_at)" {
		t.Fatalf("identifier = %q", id)
	}
}

// The evidence planner returns one generic JSON plan per statement and
// gives its connection back; a statement that fails is an error naming
// the evidence step.
func TestSkipScanDB_GenericPlansOnABoundedConnection(t *testing.T) {
	pool := skipScanDatabase(t, 2000)
	if serverMajorOf(t, pool) < 16 {
		t.Skip("EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16; the skip-scan evidence " +
			"runs only on 18")
	}
	p := poolPlanner{pool: pool, lockTimeout: 200 * time.Millisecond}
	plans, err := p.genericPlans(t.Context(), []string{skipDBCreatedStmt, skipDBRangeStmt})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans %d err %v, want 2 plans", len(plans), err)
	}
	for i, plan := range plans {
		if cost := extractTotalCost(plan); cost <= 0 {
			t.Fatalf("plan %d has no total cost: %s", i, plan)
		}
	}
	_, err = p.genericPlans(t.Context(), []string{"SELECT id FROM events WHERE nope = $1"})
	if err == nil || !strings.Contains(err.Error(), "skip-scan evidence") ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v, want the evidence step and the missing column", err)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after the evidence reads", n)
	}
	var settings string
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, "SELECT current_setting('lock_timeout')").
		Scan(&settings); err != nil || settings == "200ms" {
		t.Fatalf("the evidence bounds leaked to the pooled session: %q %v", settings, err)
	}
}

// CHECK-08 (OWNER ACTION without a local PG 18 image): on PostgreSQL 18
// the real generic plan shows the skip scan, so the floor proposes
// nothing for created_at and the rejection names the composite.
func TestSkipScanDB_PG18DropsTheServedCandidate(t *testing.T) {
	pool := skipScanDatabase(t, 200000)
	if major := serverMajorOf(t, pool); major < 18 {
		t.Skipf("CHECK-08 needs PostgreSQL 18 (btree skip scan); this server is %d", major)
	}
	o := floorOptimizer(pool)
	snap := skipScanSnapshot(200000)
	res := NewFloor(o, noopLog2).Run(t.Context(), snap, []string{"public.events"}, nil)
	if len(res.Admitted) != 0 {
		t.Fatalf("admitted %+v on PG 18, want nothing for created_at", res.Admitted)
	}
	tc, _, err := o.TableContext(t.Context(), snap, "public.events")
	if err != nil {
		t.Fatalf("table context: %v", err)
	}
	a := o.Admit(t.Context(), candidate("CREATE INDEX CONCURRENTLY x ON public.events "+
		"(created_at)"), tc)
	if a.Outcome != AdmitRejected || a.Reason != "served_by_skip_scan:events_tenant_created" {
		t.Fatalf("admission = %s %q", a.Outcome, a.Reason)
	}
}
