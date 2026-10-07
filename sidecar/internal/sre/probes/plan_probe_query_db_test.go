package probes

import (
	"testing"
	"time"
)

// A query-scoped plan_regressions run reads only that statement, so the
// row cap can never hide it behind larger regressions.

func TestPlanRegressions_ScopedToOneStatement(t *testing.T) {
	pool, ctx := livePool(t)
	bootstrapQueryStore(t, ctx, pool)
	const target, louder, quiet = 882001, 882002, -882003
	seedSamples(t, ctx, pool, target, []sample{
		{30 * time.Minute, 0, 0, "v1:a"}, {20 * time.Minute, 10, 10, "v1:a"},
		{10 * time.Minute, 20, 40, "v1:b"}})
	seedSamples(t, ctx, pool, louder, []sample{
		{30 * time.Minute, 0, 0, "v1:c"}, {20 * time.Minute, 10, 1, "v1:c"},
		{10 * time.Minute, 20, 501, "v1:d"}})
	// No flip: the split is the window's middle, so "before" needs two
	// samples before it (the first one has no delta).
	seedSamples(t, ctx, pool, quiet, []sample{
		{40 * time.Minute, 0, 0, "v1:e"}, {30 * time.Minute, 10, 10, "v1:e"},
		{20 * time.Minute, 20, 20, "v1:e"}, {10 * time.Minute, 30, 32, "v1:e"}})
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	for _, qid := range []int64{target, quiet} {
		res := r.Run(ctx, PlanRegressions, Args{QueryID: qid})
		shifts, err := PlanShifts(res)
		if err != nil || len(shifts) != 1 || shifts[0].QueryID != qid {
			t.Fatalf("scoped to %d: %+v (%v)", qid, shifts, err)
		}
	}
	res := r.Run(ctx, PlanRegressions, Args{QueryID: 882999})
	if res.Status != StatusEmpty || len(res.Rows) != 0 {
		t.Fatalf("an unknown statement reads nothing: %+v", res)
	}
	all, err := PlanShifts(r.Run(ctx, PlanRegressions, Args{}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, s := range all {
		seen[s.QueryID] = true
	}
	if !seen[target] || !seen[louder] || !seen[quiet] {
		t.Fatalf("unscoped reads every statement: %+v", all)
	}
}
