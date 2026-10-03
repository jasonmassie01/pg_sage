package collector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Golden comparison (perf fix phase, measured.md M1/M3): the rewritten
// tables and indexes pages return exactly what the v1.8.2 view-based SQL
// returned, except sizes outside the exact top-N, which are relpages
// estimates.

const goldenFixtureSQL = `
CREATE SCHEMA golden_cat;
CREATE TABLE golden_cat.hot (id int PRIMARY KEY, v int, doc text)
  WITH (autovacuum_enabled = off, toast.autovacuum_enabled = off);
CREATE INDEX hot_v ON golden_cat.hot (v) WHERE v > 3;
INSERT INTO golden_cat.hot SELECT g, g % 100,
  CASE WHEN g % 50 = 0 THEN (SELECT string_agg(md5(g::text || r::text), '')
                             FROM generate_series(1, 200) r) ELSE 'x' END
  FROM generate_series(1, 5000) g;
UPDATE golden_cat.hot SET v = v + 1 WHERE id <= 500;
DELETE FROM golden_cat.hot WHERE id > 4900;
CREATE TABLE golden_cat.noidx (a int) WITH (autovacuum_enabled = off);
INSERT INTO golden_cat.noidx SELECT generate_series(1, 300);
CREATE UNLOGGED TABLE golden_cat.unlogged (id int PRIMARY KEY)
  WITH (autovacuum_enabled = off);
INSERT INTO golden_cat.unlogged SELECT generate_series(1, 100);
CREATE TABLE golden_cat.parted (id int, k int) PARTITION BY RANGE (k);
CREATE TABLE golden_cat.parted_p1 PARTITION OF golden_cat.parted
  FOR VALUES FROM (0) TO (10) WITH (autovacuum_enabled = off);
CREATE TABLE golden_cat.parted_p2 PARTITION OF golden_cat.parted
  FOR VALUES FROM (10) TO (20) WITH (autovacuum_enabled = off);
CREATE INDEX parted_id ON golden_cat.parted (id);
INSERT INTO golden_cat.parted SELECT g, g % 20 FROM generate_series(1, 400) g;
CREATE MATERIALIZED VIEW golden_cat.mv AS SELECT id, v FROM golden_cat.hot;
CREATE UNIQUE INDEX mv_id ON golden_cat.mv (id);
VACUUM ANALYZE golden_cat.hot;
ANALYZE golden_cat.noidx;`

// createGoldenCatalog builds the fixture and generates scans on one
// connection, then flushes that backend's statistics.
func createGoldenCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS golden_cat CASCADE")
	})
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, goldenFixtureSQL); err != nil {
		t.Fatalf("golden fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT count(*) FROM golden_cat.hot;
		SELECT count(*) FROM golden_cat.noidx;
		SET enable_seqscan = off;
		SELECT * FROM golden_cat.hot WHERE id = 7;
		SELECT * FROM golden_cat.hot WHERE v = 9;
		SELECT * FROM golden_cat.mv WHERE id = 3;
		RESET enable_seqscan`); err != nil {
		t.Fatalf("golden scans: %v", err)
	}
	flushStats(t, ctx, conn.Conn())
}

// flushStats makes the activity of conn visible to other backends.
func flushStats(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	var v int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&v); err != nil {
		t.Fatalf("version: %v", err)
	}
	if v >= 150000 {
		if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
			t.Fatalf("flush stats: %v", err)
		}
		if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatalf("flush stats: %v", err)
		}
	}
	time.Sleep(1500 * time.Millisecond) // PG14's collector, and PG15+ idle flush
}

func legacyTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]TableStats {
	t.Helper()
	rows, err := pool.Query(ctx, legacyTableStatsSQL, uint32(0), 1_000_000)
	if err != nil {
		t.Fatalf("legacy tables: %v", err)
	}
	defer rows.Close()
	out := map[string]TableStats{}
	for rows.Next() {
		var ts TableStats
		var relid uint32
		if err := rows.Scan(&ts.SchemaName, &ts.RelName, &ts.SeqScan, &ts.SeqTupRead,
			&ts.IdxScan, &ts.IdxTupFetch, &ts.NTupIns, &ts.NTupUpd, &ts.NTupDel,
			&ts.NTupHotUpd, &ts.NLiveTup, &ts.NDeadTup, &ts.LastVacuum,
			&ts.LastAutovacuum, &ts.LastAnalyze, &ts.LastAutoanalyze, &ts.VacuumCount,
			&ts.AutovacuumCount, &ts.AnalyzeCount, &ts.AutoanalyzeCount, &ts.TotalBytes,
			&ts.TableBytes, &ts.IndexBytes, &ts.Relpersistence, &ts.XIDAge, &relid); err != nil {
			t.Fatalf("legacy tables scan: %v", err)
		}
		out[ts.SchemaName+"."+ts.RelName] = ts
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("legacy tables: %v", err)
	}
	return out
}

func legacyIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[uint32]IndexStats {
	t.Helper()
	rows, err := pool.Query(ctx, legacyIndexStatsSQL, uint32(0), 1_000_000)
	if err != nil {
		t.Fatalf("legacy indexes: %v", err)
	}
	defer rows.Close()
	out := map[uint32]IndexStats{}
	for rows.Next() {
		var idx IndexStats
		if err := rows.Scan(&idx.SchemaName, &idx.RelName, &idx.IndexRelName,
			&idx.IdxScan, &idx.IdxTupRead, &idx.IdxTupFetch, &idx.IndexBytes,
			&idx.IsUnique, &idx.IsPrimary, &idx.IsValid, &idx.IndexDef, &idx.IndexType,
			&idx.IndexRelID, &idx.LastIdxScan); err != nil {
			t.Fatalf("legacy indexes scan: %v", err)
		}
		out[idx.IndexRelID] = idx
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("legacy indexes: %v", err)
	}
	return out
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(j)
}

func goldenTables(tables []TableStats) map[string]TableStats {
	out := map[string]TableStats{}
	for _, ts := range tables {
		if ts.SchemaName == "golden_cat" {
			out[ts.SchemaName+"."+ts.RelName] = ts
		}
	}
	return out
}

func TestCollectTables_GoldenAgainstLegacyView(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createGoldenCatalog(t, ctx, pool)
	want := legacyTables(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.exactTopN = 1 << 30 // every relation exact: the full legacy row
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	gold := goldenTables(got)
	if len(gold) != 7 {
		t.Fatalf("collected %d golden_cat relations, want 7 (tables, partitions, "+
			"partitioned parent, matview): %v", len(gold), gold)
	}
	for key, ts := range gold {
		if w, ok := want[key]; !ok || asJSON(t, w) != asJSON(t, ts) {
			t.Errorf("%s\n got  %s\n want %s", key, asJSON(t, ts), asJSON(t, want[key]))
		}
	}
	if gold["golden_cat.hot"].IdxScan == 0 || gold["golden_cat.hot"].SeqScan == 0 ||
		gold["golden_cat.noidx"].IdxTupFetch != 0 {
		t.Fatalf("fixture activity missing: %+v / %+v", gold["golden_cat.hot"],
			gold["golden_cat.noidx"])
	}
	for key, w := range want { // nothing dropped across pages either
		if _, ok := tablesByKey(got)[key]; !ok && w.SchemaName != "sage" {
			t.Errorf("legacy relation %s missing from the rewrite", key)
		}
	}
}

func tablesByKey(tables []TableStats) map[string]bool {
	out := map[string]bool{}
	for _, ts := range tables {
		out[ts.SchemaName+"."+ts.RelName] = true
	}
	return out
}

func TestCollectTables_SizesAreEstimatesOutsideTopN(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createGoldenCatalog(t, ctx, pool)
	want := legacyTables(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.exactTopN = 0
	var sizeCalls int
	c.onCatalogQuery = func(_ context.Context, _ pgx.Tx, sql string, _ []any) {
		if strings.Contains(sql, "pg_table_size(") || strings.Contains(sql, "pg_indexes_size(") {
			sizeCalls++
		}
	}
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	if sizeCalls != 0 {
		t.Fatalf("%d exact-size queries with exactTopN=0", sizeCalls)
	}
	for key, ts := range goldenTables(got) {
		w := want[key]
		if ts.TotalBytes != ts.TableBytes+ts.IndexBytes {
			t.Errorf("%s total %d != table %d + index %d", key, ts.TotalBytes,
				ts.TableBytes, ts.IndexBytes)
		}
		if ts.TableBytes > w.TableBytes || ts.IndexBytes > w.IndexBytes {
			t.Errorf("%s estimate above the exact size: %d/%d vs %d/%d", key,
				ts.TableBytes, ts.IndexBytes, w.TableBytes, w.IndexBytes)
		}
		w.TotalBytes, w.TableBytes, w.IndexBytes = ts.TotalBytes, ts.TableBytes, ts.IndexBytes
		if asJSON(t, w) != asJSON(t, ts) {
			t.Errorf("%s non-size fields differ\n got  %s\n want %s", key,
				asJSON(t, ts), asJSON(t, w))
		}
	}
	hot, exact := goldenTables(got)["golden_cat.hot"], want["golden_cat.hot"]
	if hot.TableBytes < exact.TableBytes*8/10 || hot.IndexBytes < exact.IndexBytes*8/10 {
		t.Fatalf("vacuumed table estimate %d/%d too far under exact %d/%d (heap+toast "+
			"relpages, index relpages)", hot.TableBytes, hot.IndexBytes,
			exact.TableBytes, exact.IndexBytes)
	}
}

func TestCollectTables_DefaultTopNMeasuresOnlyTheLargest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createGoldenCatalog(t, ctx, pool)
	want := legacyTables(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.exactTopN = 1
	var measured []uint32
	c.onCatalogQuery = func(_ context.Context, _ pgx.Tx, sql string, args []any) {
		if strings.Contains(sql, "pg_table_size(") && len(args) == 1 {
			measured, _ = args[0].([]uint32)
		}
	}
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	if len(measured) != 1 {
		t.Fatalf("exact sizes requested for %d relations, want 1", len(measured))
	}
	var largest TableStats
	for _, ts := range got {
		if ts.TotalBytes > largest.TotalBytes {
			largest = ts
		}
	}
	w := want[largest.SchemaName+"."+largest.RelName]
	if largest.TableBytes != w.TableBytes || largest.IndexBytes != w.IndexBytes {
		t.Fatalf("largest relation %s.%s sized %d/%d, want exact %d/%d",
			largest.SchemaName, largest.RelName, largest.TableBytes, largest.IndexBytes,
			w.TableBytes, w.IndexBytes)
	}
}

func TestCollectIndexes_GoldenAgainstLegacyView(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createGoldenCatalog(t, ctx, pool)
	want := legacyIndexes(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.exactTopN = 1 << 30
	for cycle := 1; cycle <= 2; cycle++ { // second cycle is served from the def cache
		got, err := c.collectIndexes(ctx)
		if err != nil {
			t.Fatalf("cycle %d: collectIndexes: %v", cycle, err)
		}
		if len(got) != len(want) {
			t.Fatalf("cycle %d: %d indexes, legacy %d", cycle, len(got), len(want))
		}
		golden := 0
		for _, idx := range got {
			if idx.SchemaName == "golden_cat" {
				golden++
			}
			if w := want[idx.IndexRelID]; asJSON(t, w) != asJSON(t, idx) {
				t.Errorf("cycle %d index %d\n got  %s\n want %s", cycle, idx.IndexRelID,
					asJSON(t, idx), asJSON(t, w))
			}
		}
		if golden != 6 {
			t.Fatalf("cycle %d: %d golden_cat indexes, want 6 (pkeys, partial, "+
				"matview, partitions)", cycle, golden)
		}
	}
}
