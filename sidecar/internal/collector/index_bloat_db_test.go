package collector

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-13: the collector's index_bloat category is a statistics-based
// btree bloat estimate (reltuples, pg_stats widths, fillfactor against
// relpages) over valid leaf btree indexes of at least
// analyzer.index_bloat_min_bytes, at most 200 by size per cycle.
//
// No concurrent access tests: the read runs on the collector's single loop
// goroutine and keeps no state between calls.

const ibSchema = "rules13_ib"

func ibExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// ibFixture creates the test schema and drops it when the test ends.
func ibFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ibSchema+" CASCADE")
	})
	ibExec(t, ctx, pool, "DROP SCHEMA IF EXISTS "+ibSchema+" CASCADE",
		"CREATE SCHEMA "+ibSchema)
}

func ibCollector(t *testing.T, pool *pgxpool.Pool, floor int64) *Collector {
	t.Helper()
	cfg := testConfig()
	cfg.Analyzer.IndexBloatMinBytes = floor
	return New(pool, cfg, serverVersion(t, pool), noopLog)
}

// ibRows reads the category and keeps the test schema's rows by index name.
func ibRows(t *testing.T, c *Collector) (map[string]IndexBloat, bool) {
	t.Helper()
	rows, gaps, err := c.collectIndexBloat(context.Background())
	if err != nil {
		t.Fatalf("collectIndexBloat: %v", err)
	}
	if rows == nil {
		t.Fatal("collectIndexBloat returned nil rows on success, want a (possibly empty) list")
	}
	out := map[string]IndexBloat{}
	for _, r := range rows {
		if r.SchemaName == ibSchema {
			out[r.IndexRelName] = r
		}
	}
	return out, gaps.truncated
}

func mustRow(t *testing.T, c *Collector, index string) IndexBloat {
	t.Helper()
	got, _ := ibRows(t, c)
	row, ok := got[index]
	if !ok {
		t.Fatalf("no index_bloat row for %s among %v", index, got)
	}
	return row
}

// bloatTable creates table name (id bigint, no autovacuum) holding rows
// rows and a btree index name_id_idx on id.
func bloatTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, rows int) {
	t.Helper()
	q := ibSchema + "." + name
	ibExec(t, ctx, pool,
		"CREATE TABLE "+q+" (id bigint) WITH (autovacuum_enabled = false)",
		"INSERT INTO "+q+" SELECT g FROM generate_series(1, "+strconv.Itoa(rows)+") g",
		"CREATE INDEX "+name+"_id_idx ON "+q+" (id)")
}

// Integration: a btree after deleting 70% of its rows (VACUUM, no
// REINDEX) is estimated about 70% bloated; after REINDEX INDEX
// CONCURRENTLY the next reading is back under 10%. An index below the
// size floor is not read.
func TestCollectIndexBloat_DeletedRowsThenReindex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ibFixture(t, ctx, pool)
	bloatTable(t, ctx, pool, "t", 400000)
	ibExec(t, ctx, pool, "ANALYZE "+ibSchema+".t")
	c := ibCollector(t, pool, 1<<20)
	fresh := mustRow(t, c, "t_id_idx")
	if fresh.BloatPct >= 10 || fresh.RelName != "t" || fresh.Fillfactor != 90 ||
		fresh.IndexRelID == 0 || fresh.IndexBytes != fresh.RelPages*8192 {
		t.Fatalf("fresh index = %+v, want < 10%% bloat on table t at fillfactor 90", fresh)
	}
	ibExec(t, ctx, pool, "DELETE FROM "+ibSchema+".t WHERE id <= 280000",
		"VACUUM ANALYZE "+ibSchema+".t")
	bloated := mustRow(t, c, "t_id_idx")
	if bloated.BloatPct < 60 || bloated.BloatPct > 75 || bloated.RelPages < fresh.RelPages {
		t.Fatalf("after deleting 70%%: %+v, want 60-75%% bloat on unchanged pages (fresh %+v)",
			bloated, fresh)
	}
	if want := (bloated.RelPages - bloated.EstimatedPages) * 8192; bloated.BloatBytes != want {
		t.Fatalf("bloat bytes = %d, want (relpages - estimated) x 8192 = %d",
			bloated.BloatBytes, want)
	}
	ibExec(t, ctx, pool, "REINDEX INDEX CONCURRENTLY "+ibSchema+".t_id_idx")
	rebuilt := mustRow(t, c, "t_id_idx")
	if rebuilt.BloatPct >= 10 || rebuilt.RelPages >= bloated.RelPages/2 {
		t.Fatalf("after REINDEX: %+v, want < 10%% bloat on under half the pages", rebuilt)
	}
	got, _ := ibRows(t, ibCollector(t, pool, 64<<20))
	if _, ok := got["t_id_idx"]; ok {
		t.Fatalf("index below a 64 MiB floor was read: %+v", got["t_id_idx"])
	}
}

// Partitioned: the partitioned parent index (relkind I) is never read
// itself; its bloated leaf partition's index is, and a clean sibling's is
// read below the threshold.
func TestCollectIndexBloat_PartitionedParentNeverRead(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ibFixture(t, ctx, pool)
	p := ibSchema + ".p"
	ibExec(t, ctx, pool,
		"CREATE TABLE "+p+" (a bigint) PARTITION BY RANGE (a)",
		"CREATE TABLE "+p+"1 PARTITION OF "+p+" FOR VALUES FROM (0) TO (1000000) "+
			"WITH (autovacuum_enabled = false)",
		"CREATE TABLE "+p+"2 PARTITION OF "+p+" FOR VALUES FROM (1000000) TO (2000000) "+
			"WITH (autovacuum_enabled = false)",
		"INSERT INTO "+p+" SELECT g FROM generate_series(1, 200000) g",
		"INSERT INTO "+p+" SELECT g FROM generate_series(1000000, 1050000) g",
		"CREATE INDEX p_a_idx ON "+p+" (a)",
		"DELETE FROM "+p+" WHERE a <= 140000",
		"VACUUM ANALYZE "+p+"1", "VACUUM ANALYZE "+p+"2", "ANALYZE "+p)
	got, _ := ibRows(t, ibCollector(t, pool, 8192))
	if r, ok := got["p_a_idx"]; ok {
		t.Fatalf("partitioned parent index read: %+v", r)
	}
	byTable := map[string]IndexBloat{}
	for _, r := range got {
		byTable[r.RelName] = r
	}
	if r, ok := byTable["p1"]; !ok || r.BloatPct < 60 {
		t.Fatalf("bloated leaf partition p1: %+v (present %v), want >= 60%%", r, ok)
	}
	if r, ok := byTable["p2"]; !ok || r.BloatPct >= 10 {
		t.Fatalf("clean leaf partition p2: %+v (present %v), want < 10%%", r, ok)
	}
}

// Invalid input: an invalid index, an index on a table never analyzed (no
// pg_stats) and one on a table whose reltuples is unknown (-1) are not
// read; a control index on an analyzed table is.
func TestCollectIndexBloat_SkipsInvalidAndUnanalyzed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ibFixture(t, ctx, pool)
	for _, name := range []string{"ok", "inv", "never", "unknown"} {
		bloatTable(t, ctx, pool, name, 50000)
	}
	ibExec(t, ctx, pool,
		"ANALYZE "+ibSchema+".ok", "ANALYZE "+ibSchema+".inv", "ANALYZE "+ibSchema+".unknown",
		"UPDATE pg_index SET indisvalid = false WHERE indexrelid = '"+
			ibSchema+".inv_id_idx'::regclass",
		"UPDATE pg_class SET reltuples = -1 WHERE oid = '"+ibSchema+".unknown'::regclass")
	got, _ := ibRows(t, ibCollector(t, pool, 8192))
	if _, ok := got["ok_id_idx"]; !ok {
		t.Fatalf("control index on an analyzed table not read: %v", got)
	}
	for _, skipped := range []string{"inv_id_idx", "never_id_idx", "unknown_id_idx"} {
		if r, ok := got[skipped]; ok {
			t.Errorf("%s was read: %+v", skipped, r)
		}
	}
}

// Nil/empty: no btree index above the floor gives an empty (not nil),
// untruncated reading without error.
func TestCollectIndexBloat_NothingAboveFloor(t *testing.T) {
	pool := testPool(t)
	rows, gaps, err := ibCollector(t, pool, 1<<40).collectIndexBloat(context.Background())
	if err != nil || rows == nil || len(rows) != 0 || gaps.truncated || gaps.unknown != nil {
		t.Fatalf("1 TiB floor: rows %v (nil %v), gaps %+v, err %v; want [] without gaps",
			rows, rows == nil, gaps, err)
	}
}

// Bounded probe: 1,000 indexes above the floor give one catalog statement
// and exactly 200 rows, flagged truncated. A database of its own keeps
// other tests' indexes out of the 200 largest.
func TestCollectIndexBloat_RowCapOnThousandIndexes(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rules13_cap"))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ibFixture(t, ctx, pool)
	ibExec(t, ctx, pool,
		"CREATE TABLE "+ibSchema+".c (x int) WITH (autovacuum_enabled = false)",
		"INSERT INTO "+ibSchema+".c SELECT g FROM generate_series(1, 100) g",
		"DO $do$ BEGIN FOR i IN 1..1000 LOOP "+
			"EXECUTE format('CREATE INDEX c_x_%s ON "+ibSchema+".c (x)', i); "+
			"END LOOP; END $do$",
		"ANALYZE "+ibSchema+".c")
	c := ibCollector(t, pool, 1)
	statements := 0
	c.onCatalogQuery = func(context.Context, pgx.Tx, string, []any) { statements++ }
	rows, gaps, err := c.collectIndexBloat(ctx)
	if err != nil {
		t.Fatalf("collectIndexBloat: %v", err)
	}
	if len(rows) != indexBloatRowCap || indexBloatRowCap != 200 || !gaps.truncated {
		t.Fatalf("rows = %d (cap %d), truncated %v; want 200, truncated",
			len(rows), indexBloatRowCap, gaps.truncated)
	}
	if statements != 1 {
		t.Fatalf("catalog statements = %d, want 1 per reading", statements)
	}
}

// Error propagation: a failed read is returned with its context, not as
// an empty reading.
func TestCollectIndexBloat_ClosedPoolFails(t *testing.T) {
	live := testPool(t)
	version := serverVersion(t, live)
	pool, err := pgxpool.New(context.Background(), live.Config().ConnString())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	pool.Close()
	cfg := testConfig()
	cfg.Analyzer.IndexBloatMinBytes = 8192
	rows, _, err := New(pool, cfg, version, noopLog).collectIndexBloat(context.Background())
	if err == nil || rows != nil || !strings.Contains(err.Error(), "collect index bloat") {
		t.Fatalf("closed pool: rows %v, err %v; want nil rows and a collect index bloat error",
			rows, err)
	}
}

// Review (rules-13): an index read whose key column has no statistics has
// no estimate. It is left out of the rows but named in the gaps, so the
// analyzer can hold its open finding instead of resolving it; the step
// copies the gaps onto the snapshot. VACUUM (no ANALYZE) gives nostats
// the table reltuples the read requires without any pg_stats: PostgreSQL
// 18's CREATE INDEX leaves a never-vacuumed table's reltuples at -1, so
// the index would not be read at all.
func TestCollectIndexBloat_UnknownEstimateNamedInGaps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ibFixture(t, ctx, pool)
	bloatTable(t, ctx, pool, "known", 50000)
	bloatTable(t, ctx, pool, "nostats", 50000)
	ibExec(t, ctx, pool, "ANALYZE "+ibSchema+".known", "VACUUM "+ibSchema+".nostats")
	c := ibCollector(t, pool, 8192)
	rows, gaps, err := c.collectIndexBloat(ctx)
	if err != nil {
		t.Fatalf("collectIndexBloat: %v", err)
	}
	unknown := map[string]bool{}
	for _, id := range gaps.unknown {
		unknown[id] = true
	}
	if !unknown[ibSchema+".nostats_id_idx"] || unknown[ibSchema+".known_id_idx"] {
		t.Fatalf("gaps.unknown = %v, want %s.nostats_id_idx and not known_id_idx",
			gaps.unknown, ibSchema)
	}
	for _, r := range rows {
		if r.SchemaName == ibSchema && r.IndexRelName == "nostats_id_idx" {
			t.Fatalf("index without statistics has a row: %+v", r)
		}
	}
	for _, st := range c.extraSteps() {
		if st.name != "index_bloat" {
			continue
		}
		snap := &Snapshot{}
		if err := st.run(ctx, snap); err != nil {
			t.Fatalf("index_bloat step: %v", err)
		}
		if !slices.Contains(snap.IndexBloatUnknown, ibSchema+".nostats_id_idx") {
			t.Fatalf("snapshot unknown = %v, want the index without statistics",
				snap.IndexBloatUnknown)
		}
		st.reset(snap)
		if snap.IndexBloatUnknown != nil {
			t.Fatalf("reset kept unknown %v", snap.IndexBloatUnknown)
		}
		return
	}
	t.Fatal("no index_bloat extra step")
}
