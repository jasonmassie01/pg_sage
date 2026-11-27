package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-03 integration (real PostgreSQL, own database): the index rules
// over partitioned tables emit only statements PostgreSQL accepts.

func partitionDB(t *testing.T, label string, ddl ...string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, sql := range ddl {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return pool
}

// oldClocks starts every index's zero-scan clock 8 days before now. A
// database made for the test has its statistics epoch (stats_reset) at its
// creation, which restarts every clock, so the epoch is moved back too: the
// window is evidence the analyzer gathers over days, not the fixture's.
func oldClocks(snap *collector.Snapshot) *RuleExtras {
	e := defaultExtras()
	now := time.Now()
	e.Now = func() time.Time { return now }
	snap.System.RelationStatsEpoch = now.Add(-9 * 24 * time.Hour)
	for _, idx := range snap.Indexes {
		e.FirstSeen[idx.SchemaName+"."+idx.IndexRelName] = now.Add(-8 * 24 * time.Hour)
	}
	return e
}

var fkPartitionDDL = []string{
	"CREATE TABLE public.customers (id int PRIMARY KEY)",
	"INSERT INTO public.customers SELECT g FROM generate_series(1, 50) g",
	`CREATE TABLE public.orders (id int, customer_id int REFERENCES public.customers (id),
	   k int) PARTITION BY RANGE (k)`,
	"CREATE TABLE public.orders_p1 PARTITION OF public.orders FOR VALUES FROM (0) TO (10)",
	"CREATE TABLE public.orders_p2 PARTITION OF public.orders FOR VALUES FROM (10) TO (20)",
	"CREATE TABLE public.orders_p3 PARTITION OF public.orders FOR VALUES FROM (20) TO (30)",
	"INSERT INTO public.orders SELECT g, g % 50 + 1, g % 30 FROM generate_series(1, 300) g",
}

// CHECK-32: the partition plan of a missing FK index runs step by step and
// leaves a valid parent index; CREATE INDEX CONCURRENTLY on the parent is
// rejected (control).
func TestPartitionFK_PlanRunsOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	pool := partitionDB(t, "rules03_fk", fkPartitionDDL...)
	cfg := wraparoundDBConfig()
	snap := collectOneSnapshot(t, pool, cfg)
	got := ruleMissingFKIndexes(snap, nil, cfg, defaultExtras())
	if len(got) != 1 || got[0].ObjectIdentifier != "public.orders(customer_id)" {
		t.Fatalf("findings = %v, want the parent's key only", identsOf(got))
	}
	if got[0].RecommendedSQL != "" {
		t.Fatalf("parent finding carries SQL %q", got[0].RecommendedSQL)
	}
	_, err := pool.Exec(ctx, "CREATE INDEX CONCURRENTLY ON public.orders (customer_id)")
	if err == nil || !strings.Contains(err.Error(), "partitioned") {
		t.Fatalf("control: CIC on the parent err = %v, want PostgreSQL's rejection", err)
	}
	plan, _ := got[0].Detail["partition_plan"].([]string)
	if len(plan) != 7 {
		t.Fatalf("partition_plan = %q, want 7 statements", plan)
	}
	for _, step := range plan {
		if _, err := pool.Exec(ctx, step); err != nil {
			t.Fatalf("plan step %q: %v", step, err)
		}
	}
	var valid bool
	var attached int
	if err := pool.QueryRow(ctx, `SELECT ix.indisvalid,
	       (SELECT count(*) FROM pg_inherits WHERE inhparent = ix.indexrelid)
	  FROM pg_index ix JOIN pg_class c ON c.oid = ix.indexrelid
	 WHERE c.relname = 'orders_customer_id_fk_idx'`).Scan(&valid, &attached); err != nil {
		t.Fatalf("read the parent index: %v", err)
	}
	if !valid || attached != 3 {
		t.Fatalf("parent index valid=%v attached=%d, want valid with 3 partitions",
			valid, attached)
	}
	next := collectOneSnapshot(t, pool, cfg)
	if again := ruleMissingFKIndexes(next, snap, cfg, defaultExtras()); len(again) != 0 {
		t.Fatalf("after the plan findings = %v, want none", identsOf(again))
	}
}

// The duplicate rule over attached children (two partitioned indexes on
// the same key) emits nothing: neither child can be dropped on its own.
func TestPartitionIndex_DuplicateChildrenOnRealPostgres(t *testing.T) {
	pool := partitionDB(t, "rules03_dup", append(fkPartitionDDL,
		"CREATE INDEX orders_cust_a ON public.orders (customer_id)",
		"CREATE INDEX orders_cust_b ON public.orders (customer_id)")...)
	snap := collectOneSnapshot(t, pool, wraparoundDBConfig())
	attached := 0
	for _, idx := range snap.Indexes {
		if idx.ParentIndexOID != 0 {
			attached++
		}
	}
	if attached != 6 {
		t.Fatalf("collected %d attached children, want 6", attached)
	}
	if got := ruleDuplicateIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("findings = %v, want none over attached children", identsOf(got))
	}
}

// CHECK-31: on a 30-partition fixture the index rules emit no statement
// PostgreSQL rejects. Each one runs: in a rolled-back transaction, or (for
// CONCURRENTLY, which cannot run in one) for real.
func TestPartitionRules_NoRejectedSQLOn30Partitions(t *testing.T) {
	ctx := context.Background()
	ddl := []string{
		"CREATE TABLE public.customers (id int PRIMARY KEY)",
		`CREATE TABLE public.events (id int, customer_id int REFERENCES public.customers (id),
		   ts timestamptz, k int) PARTITION BY RANGE (k)`,
		`DO $$ BEGIN FOR i IN 1..30 LOOP
		   EXECUTE format('CREATE TABLE public.events_p%s PARTITION OF public.events
		     FOR VALUES FROM (%s) TO (%s)', i, i, i + 1);
		 END LOOP; END $$`,
		"CREATE INDEX events_ts_idx ON public.events (ts)",
		"CREATE INDEX events_ts_dup ON public.events (ts)",
		"CREATE INDEX events_p1_k_idx ON public.events_p1 (k)",
		"CREATE INDEX events_p2_ts_solo ON public.events_p2 (ts)",
	}
	pool := partitionDB(t, "rules03_30p", ddl...)
	cfg := wraparoundDBConfig()
	cfg.Analyzer.UnusedIndexWindowDays = 7
	snap := collectOneSnapshot(t, pool, cfg)
	e := oldClocks(snap)
	var findings []Finding
	for _, rule := range []RuleFunc{ruleUnusedIndexes, ruleDuplicateIndexes,
		ruleMissingFKIndexes, ruleInvalidIndexes} {
		findings = append(findings, rule(snap, nil, cfg, e)...)
	}
	ran, rollups := runFindingSQL(t, ctx, pool, findings)
	if ran == 0 || rollups != 2 {
		t.Fatalf("ran %d statements and %d roll-ups (findings %v), want the standalone "+
			"drops and 2 roll-ups", ran, rollups, identsOf(findings))
	}
}

// runFindingSQL runs each distinct recommended statement and each manual
// roll-up statement, failing on any PostgreSQL rejects.
func runFindingSQL(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, findings []Finding,
) (ran, rollups int) {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range findings {
		if manual, ok := f.Detail["manual_sql"].(string); ok {
			rollups++
			runRolledBack(t, ctx, pool, manual)
		}
		if f.RecommendedSQL == "" || seen[f.RecommendedSQL] {
			continue
		}
		seen[f.RecommendedSQL] = true
		ran++
		if strings.Contains(f.RecommendedSQL, "CONCURRENTLY") {
			if _, err := pool.Exec(ctx, f.RecommendedSQL); err != nil {
				t.Errorf("%s %s: PostgreSQL rejects %q: %v", f.Category,
					f.ObjectIdentifier, f.RecommendedSQL, err)
			}
			continue
		}
		runRolledBack(t, ctx, pool, f.RecommendedSQL)
	}
	return ran, rollups
}

func runRolledBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Errorf("PostgreSQL rejects %q: %v", sql, err)
	}
}

// CHECK-33 on real PostgreSQL: 365 daily partitions with 3 partitioned
// indexes give at most 3 unused_index findings (about 1,095 before).
func TestPartitionIndex_365PartitionsOnRealPostgres(t *testing.T) {
	ddl := []string{
		"CREATE TABLE public.daily (ts int, user_id int, kind int) PARTITION BY RANGE (ts)",
		`DO $$ BEGIN FOR i IN 1..365 LOOP
		   EXECUTE format('CREATE TABLE public.daily_d%s PARTITION OF public.daily
		     FOR VALUES FROM (%s) TO (%s)', i, i, i + 1);
		 END LOOP; END $$`,
	}
	for _, col := range []string{"ts", "user_id", "kind"} {
		ddl = append(ddl, fmt.Sprintf("CREATE INDEX daily_%s_idx ON public.daily (%s)", col, col))
	}
	pool := partitionDB(t, "rules03_365", ddl...)
	cfg := wraparoundDBConfig()
	cfg.Analyzer.UnusedIndexWindowDays = 7
	snap := collectOneSnapshot(t, pool, cfg)
	if len(snap.Indexes) < 1095 || len(snap.PartitionedIndexes) != 3 {
		t.Fatalf("collected %d indexes and %d partitioned, want 1095 and 3",
			len(snap.Indexes), len(snap.PartitionedIndexes))
	}
	got := ruleUnusedIndexes(snap, nil, cfg, oldClocks(snap))
	if len(got) == 0 || len(got) > 3 {
		t.Fatalf("unused_index findings = %d, want 1..3", len(got))
	}
}
