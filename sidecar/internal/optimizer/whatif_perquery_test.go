package optimizer

import "testing"

// perQueryImprovement is each measured query's own planner-cost change,
// so a caller can predict per statement instead of applying the pooled
// figure to every one. Queries not planned on both sides, or with no
// positive baseline cost, are left out. Stateless: no concurrency test.
func TestPerQueryImprovement(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1}, {QueryID: 2}, {QueryID: 3}, {QueryID: 4},
		{QueryID: 5}}
	before := map[int64]float64{1: 100, 2: 100, 3: 0, 4: 50}
	after := map[int64]float64{1: 10, 2: 100, 3: 5, 5: 1}
	got := perQueryImprovement(queries, before, after)
	want := map[int64]float64{1: 90, 2: 0}
	if len(got) != len(want) {
		t.Fatalf("per-query improvement %v, want %v", got, want)
	}
	for id, w := range want {
		if v, ok := got[id]; !ok || !approx(v, w) {
			t.Fatalf("query %d: %v (%v), want %v", id, v, ok, w)
		}
	}
	if got := perQueryImprovement(nil, before, after); len(got) != 0 {
		t.Fatalf("no queries: %v, want empty", got)
	}
	worse := perQueryImprovement([]QueryInfo{{QueryID: 9}}, map[int64]float64{9: 10},
		map[int64]float64{9: 15})
	if !approx(worse[9], -50) {
		t.Fatalf("a costlier plan: %v, want -50", worse[9])
	}
}
