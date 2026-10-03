package perfgate

import "testing"

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
	if d.LiveRows != rows || int64(d.Relations) != parts {
		t.Fatalf("snapshots: %d live rows in %d relations, want %d in %d partitions",
			d.LiveRows, d.Relations, rows, parts)
	}
}
