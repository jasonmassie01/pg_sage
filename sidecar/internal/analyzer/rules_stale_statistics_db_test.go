package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-10 integration (real PostgreSQL, own database): a bulk-loaded and
// analyzed lookup table is never re-flagged as the clock moves past the
// cutoff, a 20% update makes it stale, a table whose reloptions raise the
// scale factor to 0.5 is judged by that threshold, and a partitioned
// parent whose partitions changed gets the info finding until it is
// analyzed.

// No concurrent access tests: each step collects one snapshot and runs
// the pure rule on it.

func staleDBConfig() *config.Config {
	cfg := wraparoundDBConfig()
	cfg.Analyzer.AnalyzeStaleDays = 7
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	return cfg
}

func staleDBExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqls ...string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, sql := range sqls {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		// Flush after every statement: counts still pending in this backend
		// when ANALYZE resets mod_since_analyze would be added after it.
		if err := testdb.FlushStats(ctx, conn.Conn()); err != nil {
			t.Fatalf("flush statistics: %v", err)
		}
	}
}

func staleAt(snap *collector.Snapshot, cfg *config.Config, at time.Time) map[string]Finding {
	return staleIdents(ruleStaleStatisticsAt(snap, cfg, at))
}

func TestStaleStatistics_ModSinceAnalyzeOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "stale_mod"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	createStaleFixture(t, ctx, pool)
	cfg := staleDBConfig()
	now := time.Now()
	first := collectOneSnapshot(t, pool, cfg)
	assertNoWeeklyReflag(t, first, cfg, now)
	assertTunedThreshold(t, pool, first)

	// 20% of the lookup table and 30% of the tuned one change.
	staleDBExec(t, ctx, pool,
		"UPDATE public.lookup SET v = 1 WHERE id <= 4000",
		"UPDATE public.tuned SET v = 1 WHERE id <= 6000",
		"ANALYZE public.pe")
	second := collectOneSnapshot(t, pool, cfg)
	got := staleAt(second, cfg, now.AddDate(0, 0, 8))
	f, ok := got["public.lookup"]
	if !ok || f.Detail["mod_since_analyze"] != int64(4000) {
		t.Fatalf("after a 20%% update, lookup finding = %+v (findings %v)", f, got)
	}
	if _, flagged := got["public.tuned"]; flagged {
		t.Errorf("30%% change flagged a table whose scale factor is 0.5: %v", got["public.tuned"])
	}
	if _, flagged := got["public.pe"]; flagged {
		t.Errorf("parent still flagged after ANALYZE: %v", got["public.pe"])
	}
}

// createStaleFixture bulk-loads and analyzes the lookup and tuned tables
// and fills a partitioned table whose parent is never analyzed.
func createStaleFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	staleDBExec(t, ctx, pool,
		`CREATE TABLE public.lookup (id int PRIMARY KEY, v int)
		   WITH (autovacuum_enabled = off)`,
		"INSERT INTO public.lookup SELECT g, 0 FROM generate_series(1, 20000) g",
		"ANALYZE public.lookup",
		`CREATE TABLE public.tuned (id int PRIMARY KEY, v int)
		   WITH (autovacuum_enabled = off, autovacuum_analyze_scale_factor = 0.5)`,
		"INSERT INTO public.tuned SELECT g, 0 FROM generate_series(1, 20000) g",
		"ANALYZE public.tuned",
		"CREATE TABLE public.pe (id int, k int) PARTITION BY RANGE (k)",
		`CREATE TABLE public.pe_1 PARTITION OF public.pe FOR VALUES FROM (0) TO (10)
		   WITH (autovacuum_enabled = off)`,
		`CREATE TABLE public.pe_2 PARTITION OF public.pe FOR VALUES FROM (10) TO (20)
		   WITH (autovacuum_enabled = off)`,
		"INSERT INTO public.pe SELECT g, g % 20 FROM generate_series(1, 12000) g")
}

// assertNoWeeklyReflag is CHECK-08 on a real catalog: four simulated
// weeks re-flag neither the static lookup table nor the tuned one, while
// the never-analyzed partitioned parent keeps its info finding.
func assertNoWeeklyReflag(t *testing.T, snap *collector.Snapshot, cfg *config.Config,
	now time.Time) {
	t.Helper()
	for week := 1; week <= 4; week++ {
		got := staleAt(snap, cfg, now.AddDate(0, 0, 7*week))
		_, lookup := got["public.lookup"]
		_, tuned := got["public.tuned"]
		if lookup || tuned {
			t.Fatalf("week %d re-flagged a static table: %v", week, got)
		}
		parent, ok := got["public.pe"]
		if !ok || parent.Severity != "info" || parent.Detail["partitioned_parent"] != true ||
			parent.Detail["mod_since_analyze"] != int64(12000) {
			t.Fatalf("week %d parent finding = %+v (findings %v)", week, parent, got)
		}
	}
}

// assertTunedThreshold checks the thresholds against the server's
// settings: the table's own autovacuum_analyze_scale_factor = 0.5 over its
// 20000 analyzed rows, and the server's scale factor for the lookup table.
func assertTunedThreshold(t *testing.T, pool *pgxpool.Pool, snap *collector.Snapshot) {
	t.Helper()
	var base, scale float64
	if err := pool.QueryRow(context.Background(), `SELECT
		current_setting('autovacuum_analyze_threshold')::float8,
		current_setting('autovacuum_analyze_scale_factor')::float8`).
		Scan(&base, &scale); err != nil {
		t.Fatalf("settings: %v", err)
	}
	tuned, okTuned := tableIn(snap, "public", "tuned")
	lookup, okLookup := tableIn(snap, "public", "lookup")
	if !okTuned || !okLookup || tuned.AnalyzeThreshold != base+0.5*20000 ||
		lookup.AnalyzeThreshold != base+scale*20000 {
		t.Fatalf("thresholds tuned %v lookup %v, want %v and %v", tuned.AnalyzeThreshold,
			lookup.AnalyzeThreshold, base+0.5*20000, base+scale*20000)
	}
}
