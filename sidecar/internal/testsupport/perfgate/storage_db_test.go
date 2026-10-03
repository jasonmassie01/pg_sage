package perfgate

import (
	"testing"
	"time"
)

// A partitioned table is read as its partitions: the partitioned parent
// itself (no storage; after ANALYZE its reltuples is the whole table's)
// must not be counted as one more relation, or live rows double.
func TestReadTableStatsCountsPartitionsNotTheParent(t *testing.T) {
	pool, ctx, _ := seededPool(t)
	if err := AnalyzeSage(ctx, pool); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	stats, err := ReadTableStats(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	d := deltaFor(stats.Delta(TableStats{}), "sage.snapshots")
	rows := count(t, ctx, pool, "SELECT count(*) FROM sage.snapshots")
	parts := count(t, ctx, pool, `SELECT count(*) FROM pg_inherits
		WHERE inhparent = 'sage.snapshots'::regclass`)
	// Live rows are estimates (reltuples or n_live_tup); the parent would
	// double them.
	if d.LiveRows < rows*9/10 || d.LiveRows > rows*11/10 || int64(d.Relations) != parts {
		t.Fatalf("snapshots: %d live rows in %d relations, want %d in %d partitions",
			d.LiveRows, d.Relations, rows, parts)
	}
}

// Rows written by a pooled session whose statistics are still pending
// when VACUUM counts the table are not counted twice (CI PG17: 2430 live
// snapshot rows for 2160): AnalyzeSage flushes the pool's sessions first.
// The writer's statistics stay pending because it flushed less than the
// flush interval (1 s; 500 ms on PostgreSQL 14) before its insert.
func TestAnalyzeSageCountsPendingWritesOnce(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.pending_rows (id int)
		WITH (autovacuum_enabled = off)`); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the writer's next idle flush is due
	for _, sql := range []string{"SELECT 1", // flushes, restarting the interval
		"INSERT INTO public.pending_rows SELECT generate_series(1, 1000)"} {
		if _, err := writer.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	writer.Release()
	if err := AnalyzeSage(ctx, pool); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// Every session left in the pool flushes what it still held.
	time.Sleep(1100 * time.Millisecond)
	for _, conn := range pool.AcquireAllIdle(ctx) {
		_, err := conn.Exec(ctx, "SELECT 1")
		conn.Release()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	var live int64
	for {
		live = count(t, ctx, pool, `SELECT n_live_tup FROM pg_stat_user_tables
			WHERE relid = 'public.pending_rows'::regclass`)
		if live >= 1000 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond) // PostgreSQL 14's collector lags
	}
	if live != 1000 {
		t.Fatalf("n_live_tup = %d after AnalyzeSage, want 1000 (pending writes once)", live)
	}
}
