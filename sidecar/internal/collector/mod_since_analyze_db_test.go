package collector

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rules-10: the table stats carry pg_stat_get_mod_since_analyze and the
// autoanalyze threshold (threshold + scale_factor x reltuples, per-table
// reloptions over the server settings), so stale_statistics judges what
// autoanalyze judges without the advisor's configuration snapshot.

// No concurrent access tests: collectTables reads one keyset page at a
// time over its own connection; nothing here is shared.

func TestTableStatsSQL_ReadsModSinceAnalyzeAndThreshold(t *testing.T) {
	for _, want := range []string{
		"pg_stat_get_mod_since_analyze(c.oid) AS mod_since_analyze",
		"pg_options_to_table(c.reloptions)",
		"'autovacuum_analyze_threshold'", "'autovacuum_analyze_scale_factor'",
		"current_setting('autovacuum_analyze_threshold')",
		"current_setting('autovacuum_analyze_scale_factor')",
		"GREATEST(c.reltuples, 0)", "AS analyze_threshold",
	} {
		if !strings.Contains(tableStatsSQL, want) {
			t.Errorf("tableStatsSQL does not contain %q", want)
		}
	}
	raw, err := json.Marshal(TableStats{ModSinceAnalyze: 7, AnalyzeThreshold: 50.5})
	if err != nil || !strings.Contains(string(raw), `"mod_since_analyze":7`) ||
		!strings.Contains(string(raw), `"analyze_threshold":50.5`) {
		t.Fatalf("json = %s (%v)", raw, err)
	}
}

const modSinceFixtureSQL = `
CREATE SCHEMA msa;
CREATE TABLE msa.plain (id int PRIMARY KEY, v int) WITH (autovacuum_enabled = off);
INSERT INTO msa.plain SELECT g, 0 FROM generate_series(1, 2000) g;
CREATE TABLE msa.tuned (id int) WITH (autovacuum_enabled = off,
  autovacuum_analyze_scale_factor = 0.5, autovacuum_analyze_threshold = 7);
INSERT INTO msa.tuned SELECT generate_series(1, 1000);
CREATE TABLE msa.fresh (id int) WITH (autovacuum_enabled = off);
INSERT INTO msa.fresh SELECT generate_series(1, 300);
CREATE TABLE msa.parted (id int, k int) PARTITION BY RANGE (k);
CREATE TABLE msa.parted_1 PARTITION OF msa.parted FOR VALUES FROM (0) TO (10)
  WITH (autovacuum_enabled = off);
INSERT INTO msa.parted SELECT g, g % 10 FROM generate_series(1, 400) g;`

// Expected values come from pg_stat_user_tables, pg_class and the server
// settings, read independently of the collector's SQL.
func TestCollectTables_ModSinceAnalyzeMatchesServer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createModSinceFixture(t, ctx, pool)
	var base, scale float64
	if err := pool.QueryRow(ctx, `SELECT
		current_setting('autovacuum_analyze_threshold')::float8,
		current_setting('autovacuum_analyze_scale_factor')::float8`).
		Scan(&base, &scale); err != nil {
		t.Fatalf("settings: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	rows := map[string]TableStats{}
	for _, ts := range got {
		if ts.SchemaName == "msa" {
			rows[ts.RelName] = ts
		}
	}
	for name, want := range map[string]struct {
		mods      int64
		threshold float64
	}{
		"plain":    {400, base + scale*2000},
		"tuned":    {0, 7 + 0.5*1000},
		"fresh":    {300, base}, // never analyzed or vacuumed: reltuples -1 counts as 0
		"parted":   {0, base},   // the parent's own counter: autoanalyze never touches it
		"parted_1": {400, base},
	} {
		ts, ok := rows[name]
		if !ok {
			t.Errorf("msa.%s not collected", name)
			continue
		}
		if ts.ModSinceAnalyze != want.mods || math.Abs(ts.AnalyzeThreshold-want.threshold) > 1e-9 {
			t.Errorf("msa.%s mod_since_analyze %d threshold %v, want %d / %v", name,
				ts.ModSinceAnalyze, ts.AnalyzeThreshold, want.mods, want.threshold)
		}
		assertServerModSinceAnalyze(t, pool, name, ts.ModSinceAnalyze)
	}
}

// createModSinceFixture builds the msa schema on one connection, flushing
// after every statement: counts still pending in this backend when ANALYZE
// resets mod_since_analyze would be added after it.
func createModSinceFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `DROP SCHEMA IF EXISTS msa CASCADE`)
	t.Cleanup(func() { mustExec(t, pool, `DROP SCHEMA IF EXISTS msa CASCADE`) })
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, sql := range []string{modSinceFixtureSQL, "ANALYZE msa.plain",
		"ANALYZE msa.tuned", "UPDATE msa.plain SET v = 1 WHERE id <= 400"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		flushStats(t, ctx, conn.Conn())
	}
}

// assertServerModSinceAnalyze compares with pg_stat_user_tables; a
// relation the view does not list (a partitioned parent on older servers)
// reads 0.
func assertServerModSinceAnalyze(t *testing.T, pool *pgxpool.Pool, name string, got int64) {
	t.Helper()
	var want int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE((
		SELECT n_mod_since_analyze FROM pg_stat_user_tables
		 WHERE schemaname = 'msa' AND relname = $1), 0)`, name).Scan(&want); err != nil {
		t.Fatalf("pg_stat_user_tables %s: %v", name, err)
	}
	if got != want {
		t.Errorf("msa.%s mod_since_analyze %d, pg_stat_user_tables %d", name, got, want)
	}
}
