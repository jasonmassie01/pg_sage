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
	rows := count(t, ctx, pool, "SELECT count(*) FROM sage.snapshots")
	parts := count(t, ctx, pool, `SELECT count(*) FROM pg_inherits
		WHERE inhparent = 'sage.snapshots'::regclass`)
	// Live rows are estimates (reltuples or n_live_tup), and the statistics
	// lag the fixture's last writes (CI: 2430 for 2160 rows), so the
	// estimate gets a few seconds to settle; the parent would double it
	// for good, and the relation count is exact.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stats, err := ReadTableStats(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		d := deltaFor(stats.Delta(TableStats{}), "sage.snapshots")
		if int64(d.Relations) != parts {
			t.Fatalf("snapshots read as %d relations, want its %d partitions", d.Relations, parts)
		}
		if d.LiveRows >= rows*9/10 && d.LiveRows <= rows*11/10 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshots: %d live rows in %d relations, want %d in %d partitions",
				d.LiveRows, d.Relations, rows, parts)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
