package tuner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// measured.md M13 / D_catalog P7: the stale-stats cache joined
// pg_stat_user_tables by name and called pg_relation_size on every table
// every tuner cycle (98-350 ms on lifeos, and a relation-cache entry per
// table in the pool backend). It now reads the same counters by oid and
// sizes from relpages.

const legacyStaleStatsSQL = `
SELECT
    n.nspname AS schemaname,
    c.relname AS tablename,
    COALESCE(s.n_live_tup, 0) AS n_live_tup,
    COALESCE(s.n_mod_since_analyze, 0) AS n_mod,
    s.last_analyze,
    s.last_autoanalyze,
    (pg_relation_size(c.oid) / (1024*1024))::bigint AS size_mb
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s
    ON s.schemaname = n.nspname
   AND s.relname = c.relname
WHERE c.relkind IN ('r','m','p')
  AND n.nspname NOT IN ('pg_catalog','information_schema','sage','hint_plan')
  AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE 'pg_temp%'
`

func staleTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func legacyStaleEntries(t *testing.T, pool *pgxpool.Pool) map[string]staleStatsEntry {
	t.Helper()
	rows, err := pool.Query(context.Background(), legacyStaleStatsSQL)
	if err != nil {
		t.Fatalf("legacy query: %v", err)
	}
	defer rows.Close()
	out := map[string]staleStatsEntry{}
	for rows.Next() {
		var schema, table string
		var e staleStatsEntry
		var lastA, lastAA *time.Time
		if err := rows.Scan(&schema, &table, &e.liveTuples, &e.modSinceAnalyze,
			&lastA, &lastAA, &e.sizeMB); err != nil {
			t.Fatalf("legacy scan: %v", err)
		}
		if lastA != nil {
			e.lastAnalyze = *lastA
		}
		if lastAA != nil {
			e.lastAutoAnalyze = *lastAA
		}
		out[CanonicalTableName(schema, table)] = e
	}
	return out
}

// createStaleFixture: an analyzed table modified since, an unanalyzed
// one, a partitioned table, and the same table name in two schemas.
func createStaleFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS stale_a, stale_b CASCADE")
	})
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, s := range []string{`CREATE SCHEMA stale_a; CREATE SCHEMA stale_b;
		CREATE TABLE stale_a.big (id int, pad text) WITH (autovacuum_enabled = off);
		INSERT INTO stale_a.big SELECT g, repeat('p', 200) FROM generate_series(1, 20000) g;
		CREATE TABLE stale_b.big (id int) WITH (autovacuum_enabled = off);
		CREATE TABLE stale_a.parted (k int) PARTITION BY RANGE (k);
		CREATE TABLE stale_a.parted_1 PARTITION OF stale_a.parted FOR VALUES FROM (0) TO (10)`,
		"ANALYZE stale_a.big",
		"UPDATE stale_a.big SET id = id + 1 WHERE id <= 5000",
		"INSERT INTO stale_b.big SELECT generate_series(1, 50)"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	if err := testdb.FlushStats(ctx, conn); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
}

func TestLoadStaleStatsCache_MatchesLegacyQuery(t *testing.T) {
	pool := staleTestPool(t)
	ctx := context.Background()
	createStaleFixture(t, ctx, pool)
	want := legacyStaleEntries(t, pool)
	got, err := LoadStaleStatsCache(ctx, pool, TunerConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, key := range []string{"stale_a.big", "stale_b.big", "stale_a.parted",
		"stale_a.parted_1"} {
		g, w := got.entries[key], want[key]
		// Size is relpages as of the last VACUUM/ANALYZE: never above the
		// file, and stale_a.big only grew by its 5,000 updated rows since.
		if g.sizeMB > w.sizeMB || g.sizeMB < w.sizeMB-2 {
			t.Errorf("%s: size %d MB, file %d MB", key, g.sizeMB, w.sizeMB)
		}
		g.sizeMB = w.sizeMB
		if g != w {
			t.Errorf("%s: got %+v, want %+v", key, g, w)
		}
	}
	big := got.entries["stale_a.big"]
	if big.sizeMB < 3 || big.modSinceAnalyze < 5000 { // unflushed inserts may land after ANALYZE
		t.Fatalf("fixture did not exercise size/mods: %+v", big)
	}
	if len(got.entries) != len(want) {
		t.Fatalf("%d entries, legacy %d", len(got.entries), len(want))
	}
}

func TestStaleStatsSQL_NoPerTableStorageOrNameJoin(t *testing.T) {
	for _, bad := range []string{"pg_relation_size(", "pg_stat_user_tables",
		"s.relname = c.relname"} {
		if strings.Contains(staleStatsSQL, bad) {
			t.Errorf("staleStatsSQL contains %q", bad)
		}
	}
	if !strings.Contains(staleStatsSQL, "/* pg_sage") {
		t.Error("staleStatsSQL is not tagged as pg_sage's own statement")
	}
}
