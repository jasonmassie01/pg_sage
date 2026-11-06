package optimizer

import (
	"slices"
	"testing"
)

// indexes-05: the what-if measures at most 25 statements, chosen by total
// time, and always the recommendation's affected queryids.

func sampleIDs(qs []QueryInfo) []int64 {
	out := make([]int64, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.QueryID)
	}
	return out
}

func idRange(lo, hi int64) []int64 {
	var out []int64
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

func TestWhatIfSampleCapIs25(t *testing.T) {
	if maxWhatIfStatements != 25 {
		t.Fatalf("maxWhatIfStatements = %d, want 25", maxWhatIfStatements)
	}
}

func TestWhatIfSampleHottestByTotalTime(t *testing.T) {
	// Input coldest first, so a sample that keeps input order would fail.
	var qs []QueryInfo
	for i := int64(30); i >= 1; i-- {
		qs = append(qs, QueryInfo{QueryID: i, TotalTimeMs: float64(1000 - i)})
	}
	got := sampleIDs(whatIfSample(qs, nil))
	if !slices.Equal(got, idRange(1, 25)) {
		t.Fatalf("sample = %v, want queryids 1..25 hottest first", got)
	}
}

func TestWhatIfSampleBoundaryAtTheCap(t *testing.T) {
	exact := manyHypoQueries(25)
	if got := whatIfSample(exact, nil); len(got) != 25 {
		t.Fatalf("25 statements sampled to %d, want all 25", len(got))
	}
	over := manyHypoQueries(26)
	got := sampleIDs(whatIfSample(over, nil))
	if len(got) != 25 || slices.Contains(got, 26) {
		t.Fatalf("26 statements sampled to %v, want 1..25 without the coldest", got)
	}
	under := manyHypoQueries(24)
	if got := whatIfSample(under, nil); len(got) != 24 {
		t.Fatalf("24 statements sampled to %d, want all 24", len(got))
	}
}

func TestWhatIfSampleAlwaysKeepsAffected(t *testing.T) {
	qs := manyHypoQueries(200)
	got := sampleIDs(whatIfSample(qs, []int64{150, 199}))
	if len(got) != 25 {
		t.Fatalf("sample has %d statements, want 25", len(got))
	}
	if !slices.Contains(got, 150) || !slices.Contains(got, 199) {
		t.Fatalf("sample %v lost the affected queryids 150 and 199", got)
	}
	if !slices.Equal(got[2:], idRange(1, 23)) {
		t.Fatalf("sample %v: the rest must be the 23 hottest", got)
	}
}

func TestWhatIfSampleIgnoresUnknownAndDuplicateAffected(t *testing.T) {
	qs := manyHypoQueries(30)
	got := sampleIDs(whatIfSample(qs, []int64{777, 28, 28, 0}))
	if len(got) != 25 || got[0] != 28 || slices.Contains(got[1:], 28) {
		t.Fatalf("sample = %v, want 28 once and no unknown queryid", got)
	}
	if slices.Contains(got, 777) {
		t.Fatalf("sample %v holds a queryid with no statement", got)
	}
}

func TestWhatIfSampleMoreAffectedThanTheCap(t *testing.T) {
	qs := manyHypoQueries(40)
	got := sampleIDs(whatIfSample(qs, idRange(1, 40)))
	if !slices.Equal(got, idRange(1, 25)) {
		t.Fatalf("sample = %v, want the 25 hottest of the affected", got)
	}
}

func TestWhatIfSampleNilAndEmpty(t *testing.T) {
	if got := whatIfSample(nil, nil); len(got) != 0 {
		t.Fatalf("nil workload sampled to %v", got)
	}
	if got := whatIfSample([]QueryInfo{}, []int64{1}); len(got) != 0 {
		t.Fatalf("empty workload sampled to %v", got)
	}
}

func TestWhatIfSampleKeepsTiesInInputOrderAndInputIntact(t *testing.T) {
	qs := []QueryInfo{{QueryID: 3, TotalTimeMs: 5}, {QueryID: 1, TotalTimeMs: 5},
		{QueryID: 2, TotalTimeMs: 9}}
	got := sampleIDs(whatIfSample(qs, nil))
	if !slices.Equal(got, []int64{2, 3, 1}) {
		t.Fatalf("sample = %v, want [2 3 1]", got)
	}
	if !slices.Equal(sampleIDs(qs), []int64{3, 1, 2}) {
		t.Fatalf("sampling reordered the caller's slice: %v", sampleIDs(qs))
	}
}

// No concurrency test: whatIfSample is pure and shares no state.
