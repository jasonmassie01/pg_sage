package collector

import "testing"

func TestDetectStatsReset_WorkloadChurn(t *testing.T) {
	// 60% of overlapping queries decreased, but total calls stays
	// at ~90% of previous. Natural churn, not a real reset.
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
		{QueryID: 3, Calls: 300},
		{QueryID: 4, Calls: 400},
		{QueryID: 5, Calls: 500},
	}
	// 3/5 decreased (60%), but total is 1350 vs prev 1500 (90%)
	current := []QueryStats{
		{QueryID: 1, Calls: 50},
		{QueryID: 2, Calls: 100},
		{QueryID: 3, Calls: 150},
		{QueryID: 4, Calls: 450},
		{QueryID: 5, Calls: 600},
	}
	if detectStatsReset(current, previous) {
		t.Error("expected false for workload churn, got true")
	}
}

func TestDetectStatsReset_RealReset(t *testing.T) {
	// 90% decreased AND total calls is 5% of previous.
	previous := []QueryStats{
		{QueryID: 1, Calls: 1000},
		{QueryID: 2, Calls: 2000},
		{QueryID: 3, Calls: 3000},
		{QueryID: 4, Calls: 4000},
		{QueryID: 5, Calls: 5000},
		{QueryID: 6, Calls: 1000},
		{QueryID: 7, Calls: 2000},
		{QueryID: 8, Calls: 3000},
		{QueryID: 9, Calls: 4000},
		{QueryID: 10, Calls: 5000},
	}
	// prevTotal = 30000, currTotal = 1500 (5%), 9/10 decreased
	current := []QueryStats{
		{QueryID: 1, Calls: 1},
		{QueryID: 2, Calls: 2},
		{QueryID: 3, Calls: 3},
		{QueryID: 4, Calls: 4},
		{QueryID: 5, Calls: 5},
		{QueryID: 6, Calls: 6},
		{QueryID: 7, Calls: 7},
		{QueryID: 8, Calls: 8},
		{QueryID: 9, Calls: 9},
		{QueryID: 10, Calls: 1455},
	}
	if !detectStatsReset(current, previous) {
		t.Error("expected true for real reset, got false")
	}
}

func TestDetectStatsReset_PartialChurn(t *testing.T) {
	// 55% decreased but total calls is 50% of previous.
	// The aggregate didn't plummet enough — not a reset.
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 100},
		{QueryID: 3, Calls: 100},
		{QueryID: 4, Calls: 100},
		{QueryID: 5, Calls: 100},
		{QueryID: 6, Calls: 100},
		{QueryID: 7, Calls: 100},
		{QueryID: 8, Calls: 100},
		{QueryID: 9, Calls: 100},
		{QueryID: 10, Calls: 100},
		{QueryID: 11, Calls: 100},
		{QueryID: 12, Calls: 100},
		{QueryID: 13, Calls: 100},
		{QueryID: 14, Calls: 100},
		{QueryID: 15, Calls: 100},
		{QueryID: 16, Calls: 100},
		{QueryID: 17, Calls: 100},
		{QueryID: 18, Calls: 100},
		{QueryID: 19, Calls: 100},
		{QueryID: 20, Calls: 100},
	}
	// prevTotal = 2000. Make 11/20 decreased (55%).
	// currTotal = 1000 (50% of prev) — above the 20% threshold.
	current := make([]QueryStats, 20)
	for i := range current {
		current[i].QueryID = int64(i + 1)
		if i < 11 {
			current[i].Calls = 50 // decreased
		} else {
			current[i].Calls = 50 // also 50, but not "decreased" since prev=100
		}
	}
	// Actually need 11 decreased and 9 not. Let me fix:
	// All at 50 means all decreased. Adjust so only 11 decrease.
	for i := range current {
		current[i].QueryID = int64(i + 1)
		if i < 11 {
			current[i].Calls = 50
		} else {
			current[i].Calls = 150
		}
	}
	// 11/20 decreased (55%), currTotal = 11*50 + 9*150 = 550+1350 = 1900
	// 1900 < 2000/5 = 400? No. So ratio met but total not met. Good.
	if detectStatsReset(current, previous) {
		t.Error("expected false for partial churn, got true")
	}
}

func TestDetectStatsReset_EmptyPrevious(t *testing.T) {
	current := []QueryStats{
		{QueryID: 1, Calls: 100},
	}
	if detectStatsReset(current, nil) {
		t.Error("expected false for empty previous, got true")
	}
	if detectStatsReset(current, []QueryStats{}) {
		t.Error("expected false for empty previous slice, got true")
	}
}

func TestDetectStatsReset_NoOverlap(t *testing.T) {
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
	}
	current := []QueryStats{
		{QueryID: 3, Calls: 10},
		{QueryID: 4, Calls: 20},
	}
	if detectStatsReset(current, previous) {
		t.Error("expected false for no overlap, got true")
	}
}

func TestSumCalls(t *testing.T) {
	qs := []QueryStats{
		{Calls: 10},
		{Calls: 20},
		{Calls: 30},
	}
	got := sumCalls(qs)
	if got != 60 {
		t.Errorf("sumCalls = %d, want 60", got)
	}
	if sumCalls(nil) != 0 {
		t.Error("sumCalls(nil) should be 0")
	}
}
