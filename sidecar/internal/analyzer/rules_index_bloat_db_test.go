package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-13, end to end on a real server: a full collector cycle reads a
// btree whose table lost 70% of its rows (VACUUM, no REINDEX) and the rule
// raises a warning with REINDEX INDEX CONCURRENTLY; after the rebuild the
// next cycle's reading clears it with the category evaluated, so the open
// finding resolves.
func TestIndexBloat_RealCatalogFlagsThenResolves(t *testing.T) {
	ctx := context.Background()
	pool := indexBloatFixturePool(t, ctx)
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	cfg.Analyzer.IndexBloatMinBytes = 1 << 20
	coll := collector.New(pool, cfg, version, noopLog)
	needs := ruleNeeds(t, "index_bloat")

	snap := collectOnce(t, ctx, coll)
	f := findingFor(ruleIndexBloat(snap, nil, cfg, nil), "public.r13_t_id_idx")
	if f == nil || f.Severity != "warning" ||
		f.RecommendedSQL != `REINDEX INDEX CONCURRENTLY "public"."r13_t_id_idx";` {
		t.Fatalf("bloated index finding = %+v, want a warning with REINDEX CONCURRENTLY", f)
	}
	if pct, ok := f.Detail["estimated_bloat_pct"].(float64); !ok || pct < 60 || pct > 75 {
		t.Fatalf("estimated_bloat_pct = %v, want 60-75 after deleting 70%%",
			f.Detail["estimated_bloat_pct"])
	}
	if !needs(snap, nil) {
		t.Fatal("cycle 1: index_bloat input reported unknown")
	}

	if _, err := pool.Exec(ctx, f.RecommendedSQL); err != nil {
		t.Fatalf("run the recommended rebuild: %v", err)
	}
	next := collectOnce(t, ctx, coll)
	if g := findingFor(ruleIndexBloat(next, snap, cfg, nil), "public.r13_t_id_idx"); g != nil {
		t.Fatalf("after REINDEX: finding %+v, want none", g)
	}
	if !needs(next, snap) {
		t.Fatal("cycle 2: index_bloat input unknown, the open finding would not resolve")
	}
}

// indexBloatFixturePool is a bootstrapped database holding public.r13_t
// (400,000 bigints, autovacuum off) with a btree on id, after deleting
// 70% of the rows and a VACUUM ANALYZE.
func indexBloatFixturePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rules13_ib"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, sql := range []string{
		"CREATE TABLE public.r13_t (id bigint) WITH (autovacuum_enabled = false)",
		"INSERT INTO public.r13_t SELECT g FROM generate_series(1, 400000) g",
		"CREATE INDEX r13_t_id_idx ON public.r13_t (id)",
		"DELETE FROM public.r13_t WHERE id <= 280000",
		"VACUUM ANALYZE public.r13_t",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return pool
}
