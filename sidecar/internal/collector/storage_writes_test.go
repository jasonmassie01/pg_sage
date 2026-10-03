package collector

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// Perf storage phase (M11): the collector wrote every sampled query to
// sage.query_store every cycle. Across cycles it now writes a query only
// when its counters move (or once per keyframe interval).
func TestRecordQueryStore_WritesOnlyMovedQueriesAcrossCycles(t *testing.T) {
	p, ctx, c := preflightPool(t)
	const idleQ, busyQ = int64(9_910_001), int64(9_910_002)
	at := time.Now()
	snap := func(busyCalls int64, minute int) *Snapshot {
		return &Snapshot{CollectedAt: at.Add(time.Duration(minute) * time.Minute),
			Queries: []QueryStats{
				{QueryID: idleQ, Calls: 5, TotalExecTime: 50, MeanExecTime: 10, Rows: 5},
				{QueryID: busyQ, Calls: busyCalls, TotalExecTime: float64(busyCalls),
					MeanExecTime: 1, Rows: busyCalls},
			}}
	}
	for minute := 0; minute < 5; minute++ {
		c.recordQueryStore(ctx, snap(int64(10+minute), minute))
	}
	var idle, busy int
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE queryid = $1),
		count(*) FILTER (WHERE queryid = $2) FROM sage.query_store`, idleQ, busyQ).
		Scan(&idle, &busy); err != nil {
		t.Fatal(err)
	}
	if idle != 1 || busy != 5 {
		t.Fatalf("rows: idle %d, busy %d; want 1 and 5", idle, busy)
	}
}

// The collector creates the day partition its snapshot lands in before
// writing it: a cycle on a day nothing has prepared yet still persists.
func TestPersist_CreatesTheDayPartitionFirst(t *testing.T) {
	p, ctx, c := preflightPool(t)
	far := partition.DayStart(time.Now()).AddDate(0, 0, 9).Add(time.Hour)
	if _, err := p.Exec(ctx, `DROP TABLE IF EXISTS sage.`+
		partition.Snapshots.DayName(far)); err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{CollectedAt: far}
	if err := c.persist(ctx, snap); err != nil {
		t.Fatalf("persist on an unprepared day: %v", err)
	}
	var where string
	if err := p.QueryRow(ctx, `SELECT tableoid::regclass::text FROM sage.snapshots
		WHERE collected_at = $1 AND category = 'system'`, far).Scan(&where); err != nil {
		t.Fatal(err)
	}
	if want := "sage." + partition.Snapshots.DayName(far); where != want {
		t.Fatalf("row stored in %s, want %s (not the default partition)", where, want)
	}
	if _, err := p.Exec(ctx, `DELETE FROM sage.snapshots WHERE collected_at = $1`,
		far); err != nil {
		t.Fatal(err)
	}
}
