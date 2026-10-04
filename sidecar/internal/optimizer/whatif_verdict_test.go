package optimizer

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

// Phase 0 item 7: the HypoPG what-if gate. Improvement is weighted by each
// query's total execution time (a 90% win on the one hot query must not
// average down to 9% across ten cold ones), and anything that was not
// measured end to end is "unverified", never "validated".

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestWeightedImprovement_HotQueryDominates(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1, TotalTimeMs: 9000, Calls: 9000}}
	before := map[int64]float64{1: 100}
	after := map[int64]float64{1: 10}
	for id := int64(2); id <= 10; id++ {
		queries = append(queries, QueryInfo{QueryID: id, TotalTimeMs: 10, Calls: 1})
		before[id], after[id] = 100, 100
	}
	got, measured := weightedImprovement(queries, before, after)
	if measured != 10 {
		t.Fatalf("measured = %d, want 10", measured)
	}
	want := 90.0 * 9000 / (9000 + 90)
	if !approx(got, want) {
		t.Fatalf("weighted improvement = %.3f, want %.3f (unweighted would be 9)", got, want)
	}
	// Mirror: the win is on the cold query only.
	queries[0].TotalTimeMs, queries[1].TotalTimeMs = 10, 9000
	before[2], after[2] = 100, 10
	after[1] = 100
	got, _ = weightedImprovement(queries, before, after)
	if got > 90 || got < 89 {
		t.Fatalf("win moved to query 2 (weight 9000): %.3f", got)
	}
}

// Without total time the weight falls back to calls, then to equal
// weights; a query with no positive baseline cost is not measured.
func TestWeightedImprovement_FallbacksAndBoundaries(t *testing.T) {
	q := []QueryInfo{{QueryID: 1, Calls: 3}, {QueryID: 2, Calls: 1}}
	got, n := weightedImprovement(q, map[int64]float64{1: 100, 2: 100},
		map[int64]float64{1: 50, 2: 100})
	if n != 2 || !approx(got, 37.5) {
		t.Fatalf("calls-weighted = %.3f/%d, want 37.5/2", got, n)
	}
	q = []QueryInfo{{QueryID: 1}, {QueryID: 2}}
	got, n = weightedImprovement(q, map[int64]float64{1: 100, 2: 100},
		map[int64]float64{1: 50, 2: 100})
	if n != 2 || !approx(got, 25) {
		t.Fatalf("equal-weighted = %.3f/%d, want 25/2", got, n)
	}
	got, n = weightedImprovement(q, map[int64]float64{1: 0}, map[int64]float64{1: 0})
	if n != 0 || got != 0 {
		t.Fatalf("zero-cost baseline measured: %.3f/%d", got, n)
	}
	got, n = weightedImprovement(nil, nil, nil)
	if n != 0 || got != 0 {
		t.Fatalf("empty = %.3f/%d", got, n)
	}
	got, _ = weightedImprovement([]QueryInfo{{QueryID: 1, TotalTimeMs: 1}},
		map[int64]float64{1: 100}, map[int64]float64{1: 150})
	if !approx(got, -50) {
		t.Fatalf("regression = %.3f, want -50", got)
	}
	// Missing "after" for a query (could not be planned after) is not measured.
	got, n = weightedImprovement([]QueryInfo{{QueryID: 1, TotalTimeMs: 1}},
		map[int64]float64{1: 100}, map[int64]float64{})
	if n != 0 || got != 0 {
		t.Fatalf("unmatched after = %.3f/%d", got, n)
	}
}

func TestWhatIfVerdict(t *testing.T) {
	cases := []struct {
		name    string
		res     WhatIfResult
		err     error
		verdict string
		reason  string
	}{
		{"verified", WhatIfResult{Improvement: 20, SizeBytes: 1, Measured: 3}, nil,
			WhatIfVerified, ""},
		{"exactly at minimum", WhatIfResult{Improvement: 10, SizeBytes: 1, Measured: 1}, nil,
			WhatIfVerified, ""},
		{"below minimum", WhatIfResult{Improvement: 9.99, SizeBytes: 1, Measured: 1}, nil,
			WhatIfRejected, "below"},
		{"error", WhatIfResult{}, errors.New("conn reset"), WhatIfUnverified, "conn reset"},
		{"nothing measured", WhatIfResult{SizeBytes: 1}, nil, WhatIfUnverified, "no workload"},
		{"no size", WhatIfResult{Improvement: 50, Measured: 1}, nil, WhatIfUnverified,
			"size"},
		{"failed query", WhatIfResult{Improvement: 50, SizeBytes: 1, Measured: 2, Failed: 1},
			nil, WhatIfUnverified, "1 of 3"},
		{"failed query below minimum", WhatIfResult{Improvement: 1, SizeBytes: 1,
			Measured: 2, Failed: 1}, nil, WhatIfUnverified, "1 of 3"},
	}
	for _, c := range cases {
		verdict, reason := whatIfVerdict(c.res, c.err, 10)
		if verdict != c.verdict || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: verdict %q (%q), want %q containing %q", c.name, verdict,
				reason, c.verdict, c.reason)
		}
	}
}

func TestEnrichWithHypoPG_Unavailable(t *testing.T) {
	o := &Optimizer{cfg: fnTestOptimizerConfig(), logFn: noopLog2,
		whatIf: fakeWhatIf{}}
	rec, rejected := o.enrichWithHypoPG(context.Background(), sampleRecommendation(),
		sampleTableContext())
	if rejected || rec.Validated || rec.WhatIf != WhatIfUnverified ||
		!strings.Contains(rec.WhatIfReason, "unavailable") {
		t.Fatalf("unavailable = %+v rejected=%t", rec, rejected)
	}
	o.whatIf = nil
	rec, rejected = o.enrichWithHypoPG(context.Background(), sampleRecommendation(),
		sampleTableContext())
	if rejected || rec.WhatIf != WhatIfUnverified {
		t.Fatalf("nil validator = %+v rejected=%t", rec, rejected)
	}
}

// Integration: on a real HypoPG session, a hot query that the index helps
// carries the verdict over many cold queries it does not help, and a
// workload query that cannot be planned is isolated (savepoint) instead
// of aborting the whole evaluation.
func TestHypoPGCallWeightedAndIsolated(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, noopLog2)
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items (category)"}
	hot := QueryInfo{QueryID: 1, TotalTimeMs: 100000, Calls: 1000,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}
	cold := QueryInfo{QueryID: 2, TotalTimeMs: 1, Calls: 1,
		Text: "SELECT count(*) FROM hypopg_session_test.items"}
	broken := QueryInfo{QueryID: 3, TotalTimeMs: 1, Calls: 1,
		Text: "SELECT nope FROM hypopg_session_test.items"}
	res, err := h.Validate(t.Context(), rec, []QueryInfo{broken, hot, cold})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Measured != 2 || res.Failed != 1 || res.Improvement < 90 || res.SizeBytes <= 0 {
		t.Fatalf("hot query must dominate and the broken one be isolated: %+v", res)
	}
	assertHypoPGSessionClean(t, pool)
	hot.TotalTimeMs, cold.TotalTimeMs = 1, 100000
	res, err = h.Validate(t.Context(), rec, []QueryInfo{hot, cold})
	if err != nil || res.Measured != 2 || res.Improvement > 5 {
		t.Fatalf("cold-weighted workload: %+v %v", res, err)
	}
	if verdict, _ := whatIfVerdict(res, nil, 10); verdict != WhatIfRejected {
		t.Fatalf("an index that only helps a negligible query must be rejected, got %s",
			verdict)
	}
}
