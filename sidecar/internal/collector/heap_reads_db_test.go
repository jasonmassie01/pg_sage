package collector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// forecast-04: the table stats carry heap_blks_read (pg_statio's
// definition: blocks fetched minus blocks hit), so the cache forecast can
// name the tables whose reads missed shared_buffers over a day.

// No concurrent access tests: collectTables reads one keyset page at a
// time over its own connection; nothing here is shared.

func TestTableStatsSQL_ReadsHeapBlocksRead(t *testing.T) {
	want := "pg_stat_get_blocks_fetched(c.oid) - pg_stat_get_blocks_hit(c.oid)"
	if !strings.Contains(tableStatsSQL, want+" AS heap_blks_read") {
		t.Fatalf("tableStatsSQL does not read heap_blks_read as %q", want)
	}
	raw, err := json.Marshal(TableStats{HeapBlksRead: 42})
	if err != nil || !strings.Contains(string(raw), `"heap_blks_read":42`) {
		t.Fatalf("heap_blks_read json = %s (%v)", raw, err)
	}
	// Zero is a real count (every block was a hit), so it is written.
	raw, _ = json.Marshal(TableStats{})
	if !strings.Contains(string(raw), `"heap_blks_read":0`) {
		t.Fatalf("a zero heap_blks_read is not written: %s", raw)
	}
}

// A table larger than a quarter of shared_buffers is created and scanned
// through ring buffers, so the scan reads most of its blocks from outside
// shared_buffers; the collected count matches pg_statio_user_tables.
func TestCollectTables_HeapBlocksReadMatchesStatio(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	mustExec(t, pool, `DROP SCHEMA IF EXISTS heapreads CASCADE`)
	mustExec(t, pool, `CREATE SCHEMA heapreads`)
	t.Cleanup(func() { mustExec(t, pool, `DROP SCHEMA IF EXISTS heapreads CASCADE`) })
	var sharedBlocks int64
	if err := pool.QueryRow(ctx, `SELECT setting::int8 FROM pg_settings
		WHERE name = 'shared_buffers'`).Scan(&sharedBlocks); err != nil {
		t.Fatalf("shared_buffers: %v", err)
	}
	rows := sharedBlocks * 8 / 10 * 30 // about 0.4 x shared_buffers at ~110 B a row
	mustExec(t, pool, `CREATE TABLE heapreads.big AS
		SELECT g AS id, repeat('x', 80) AS pad FROM generate_series(1, $1::int8) g`, rows)
	mustExec(t, pool, `SELECT count(*) FROM heapreads.big`)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	flushStats(t, ctx, conn.Conn())
	conn.Release()

	var want int64
	if err := pool.QueryRow(ctx, `SELECT heap_blks_read FROM pg_statio_user_tables
		WHERE schemaname = 'heapreads' AND relname = 'big'`).Scan(&want); err != nil {
		t.Fatalf("pg_statio_user_tables: %v", err)
	}
	if want < 100 {
		t.Fatalf("fixture read only %d blocks from outside shared_buffers", want)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	for _, ts := range got {
		if ts.SchemaName == "heapreads" && ts.RelName == "big" {
			if ts.HeapBlksRead != want {
				t.Fatalf("heap_blks_read = %d, want pg_statio's %d", ts.HeapBlksRead, want)
			}
			return
		}
	}
	t.Fatal("heapreads.big was not collected")
}
