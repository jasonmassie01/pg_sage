package retention

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// An open history partition that closes within trimSkipWindow is not
// trimmed: it is dropped whole once closed, so trimming it first would
// only write WAL for rows about to go anyway (lifeos: ~6 GB, ~34 h before
// the drop). The cap says so, once. One that closes later is trimmed.
func TestSnapshotCap_NoTrimWhenTheHistoryPartitionClosesWithin48h(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	if trimSkipWindow != 48*time.Hour {
		t.Fatalf("trimSkipWindow = %s, want 48h", trimSkipWindow)
	}
	for _, tc := range []struct {
		in    time.Duration
		trims bool
	}{{47 * time.Hour, false}, {49 * time.Hour, true}} {
		closes := time.Now().Add(tc.in).Truncate(time.Second)
		reboundAt(t, ctx, tbl, closes, 3)
		cleanCapRows(t, ctx)
		y := partition.DayStart(time.Now()).AddDate(0, 0, -1)
		for h := 1; h <= 3; h++ {
			insertBlob(t, ctx, y.Add(time.Duration(h)*time.Hour), 16)
		}
		logs := &captureLog{}
		c := New(pool, snapshotCfg(), logs.log)
		c.capBytes = 1
		first, second := c.RunOnce(ctx), c.RunOnce(ctx)
		deleted := first.Deleted["snapshots"] + second.Deleted["snapshots"]
		if tc.trims {
			if deleted != 3 || logs.contains("WARN", "not trimming before") {
				t.Fatalf("closes in %s: deleted %d, logs %v; want the 3 old rows trimmed",
					tc.in, deleted, logs.lines)
			}
			continue
		}
		if deleted != 0 || len(remainingIDs(t, ctx)) != 3 {
			t.Fatalf("closes in %s: deleted %d rows; want none (dropped whole soon)", tc.in,
				deleted)
		}
		waits := 0
		for _, l := range logs.lines {
			if strings.HasPrefix(l, "WARN ") && strings.Contains(l, "not trimming before") {
				waits++
				if !strings.Contains(l, closes.UTC().Format(time.RFC3339)) ||
					!strings.Contains(l, "dropped whole") {
					t.Fatalf("the warning does not say when and what: %s", l)
				}
			}
		}
		if waits != 1 {
			t.Fatalf("closes in %s: waiting logged %d times in 2 runs, want once: %v", tc.in,
				waits, logs.lines)
		}
	}
}
