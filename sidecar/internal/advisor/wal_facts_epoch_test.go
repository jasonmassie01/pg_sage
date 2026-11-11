package advisor

import (
	"context"
	"testing"
	"time"
)

// No concurrent access tests: walFacts is called from one goroutine and
// the fixtures share no state.

// State transitions (advisors-05 review): a statistics reset is detected by
// its epoch, not only by a counter that fell. After a reset the counters
// can already be above the window start's, and the delta would be a lie.
func TestWALFacts_EpochMovedWithHigherCounters(t *testing.T) {
	cur := walCounters(130, 52, 22<<30) // every counter above the start's
	cur.CheckpointStatsEpoch = walNow.Add(-10 * time.Minute)
	f, err := walFacts(context.Background(), walHistory1h(),
		walWindowSnap("1024", "300", cur), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if f.requestedRatio != nil || f.reqDelta != 0 || f.timedDelta != 0 {
		t.Errorf("checkpoint epoch moved: ratio %v deltas %d/%d, want unknown",
			fval(f.requestedRatio), f.reqDelta, f.timedDelta)
	}
	if f.walPerHour == nil || *f.walPerHour != 12<<30 {
		t.Errorf("WAL/h = %v, want 12 GiB/h (pg_stat_wal kept its epoch)",
			fval(f.walPerHour))
	}

	cur = walCounters(130, 52, 22<<30)
	cur.WALStatsEpoch = walNow.Add(-10 * time.Minute) // no insert positions either
	f, err = walFacts(context.Background(), walHistory1h(),
		walWindowSnap("1024", "300", cur), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if f.walPerHour != nil || f.walPerTimeout != nil || walSizeFloor(f).known {
		t.Errorf("pg_stat_wal epoch moved: WAL/h %v, want unknown and no floor",
			fval(f.walPerHour))
	}
	if f.requestedRatio == nil || *f.requestedRatio != 0.9375 {
		t.Errorf("ratio = %v, want 30/32 (the checkpoint epoch did not move)",
			fval(f.requestedRatio))
	}
}
