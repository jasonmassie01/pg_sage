package tuning

import (
	"math"
	"testing"
)

// The write-cost model: what an index costs on writes, from the table's
// write rates and the index entry width.

func TestEstimateWriteCost_IndexMaintenance(t *testing.T) {
	got := EstimateWriteCost(WriteCostInput{InsertsPerSec: 100, UpdatesPerSec: 50,
		HotUpdatesPerSec: 30, DeletesPerSec: 20, Indexes: 3, LiveTuples: 1_000_000,
		EntryBytes: 32})
	// Index entries are written for inserts and non-HOT updates only.
	if got.IndexWritesPerSec != 360 {
		t.Fatalf("index writes/s = %v, want (100+50-30)*3 = 360", got.IndexWritesPerSec)
	}
	if got.NewIndexWritesPerSec != 120 || got.NewIndexBytesPerSec != 120*32 {
		t.Fatalf("new index: %v writes/s, %v bytes/s", got.NewIndexWritesPerSec,
			got.NewIndexBytesPerSec)
	}
	if math.Abs(got.ShareOfIndexMaintenance-0.25) > 1e-9 {
		t.Fatalf("share = %v, want 120/(360+120) = 0.25", got.ShareOfIndexMaintenance)
	}
	if math.Abs(got.HOTRatio-0.6) > 1e-9 {
		t.Fatalf("HOT ratio = %v, want 0.6", got.HOTRatio)
	}
	// Leaf pages at the default 90% fill factor.
	if want := int64(math.Ceil(1_000_000 * 32 / 0.9)); got.EstimatedNewIndexBytes != want {
		t.Fatalf("estimated size = %d, want %d", got.EstimatedNewIndexBytes, want)
	}
}

func TestEstimateWriteCost_ZeroAndNegativeInputs(t *testing.T) {
	got := EstimateWriteCost(WriteCostInput{})
	if got != (WriteCost{}) {
		t.Fatalf("no writes, no rows: %+v", got)
	}
	got = EstimateWriteCost(WriteCostInput{InsertsPerSec: -5, UpdatesPerSec: 10,
		HotUpdatesPerSec: 50, Indexes: -2, LiveTuples: -1, EntryBytes: -8})
	if got.IndexWritesPerSec != 0 || got.NewIndexWritesPerSec != 0 ||
		got.EstimatedNewIndexBytes != 0 || got.HOTRatio != 1 {
		t.Fatalf("negative inputs clamp to zero, HOT capped at the updates: %+v", got)
	}
	for _, v := range []float64{got.ShareOfIndexMaintenance, got.HOTRatio,
		got.NewIndexBytesPerSec} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("no NaN or Inf: %+v", got)
		}
	}
}

func TestEstimateWriteCost_FirstIndexCarriesAllMaintenance(t *testing.T) {
	got := EstimateWriteCost(WriteCostInput{InsertsPerSec: 10, Indexes: 0, EntryBytes: 16})
	if got.ShareOfIndexMaintenance != 1 || got.NewIndexWritesPerSec != 10 {
		t.Fatalf("an unindexed table: %+v", got)
	}
}

func TestEntryBytes(t *testing.T) {
	for _, tc := range []struct {
		widths []int
		want   int
	}{
		{nil, 16},            // header and item pointer only
		{[]int{4}, 24},       // 4+16 = 20 -> 24
		{[]int{4, 8}, 32},    // 28 -> 32
		{[]int{8, 8}, 32},    // 32 stays
		{[]int{-3, 0}, 16},   // unknown widths count as zero
		{[]int{100, 1}, 120}, // 117 -> 120
	} {
		if got := entryBytes(tc.widths); got != tc.want {
			t.Errorf("entryBytes(%v) = %d, want %d", tc.widths, got, tc.want)
		}
	}
}
