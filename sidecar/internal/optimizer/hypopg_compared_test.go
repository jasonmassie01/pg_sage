package optimizer

import (
	"reflect"
	"testing"
)

// comparedCosts (indexes-06 review): a statement planned without the
// index but not with it is unplanned, so the measurement is partial and
// the verdict stays unverified. No concurrent-access test: comparedCosts is
// a pure function of its arguments.

func TestComparedCosts_LostPlanIsUnplanned(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1, Calls: 10}, {QueryID: 2, Calls: 10},
		{QueryID: 3, Calls: 10}, {QueryID: 2, Calls: 10}}
	before := map[int64]float64{1: 100, 2: 100, 3: 100}
	after := map[int64]float64{1: 50, 3: 50}
	res := comparedCosts(queries, before, after, []int64{9})
	if !reflect.DeepEqual(res.UnplannedQueryIDs, []int64{9, 2}) || res.Failed != 2 ||
		res.Measured != 2 || res.Improvement < 49.9 || res.Improvement > 50.1 {
		t.Fatalf("result %+v: want unplanned [9 2], failed 2, measured 2, 50%%", res)
	}
	res.SizeBytes = 8192
	verdict, reason := whatIfVerdict(res, nil, 10)
	want := "planned statements improved 50.0% (call-weighted, 2 of 4); " +
		"not planned: queryid 9, 2"
	if verdict != WhatIfUnverified || reason != want {
		t.Fatalf("verdict %q reason %q, want unverified %q", verdict, reason, want)
	}
}

func TestComparedCosts_CompleteMeasurementHasNoUnplanned(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1, Calls: 1}}
	res := comparedCosts(queries, map[int64]float64{1: 10}, map[int64]float64{1: 8}, nil)
	if res.Failed != 0 || len(res.UnplannedQueryIDs) != 0 || res.Measured != 1 {
		t.Fatalf("result %+v: a complete measurement has nothing unplanned", res)
	}
}

func TestComparedCosts_EmptyWorkload(t *testing.T) {
	res := comparedCosts(nil, nil, nil, nil)
	if !reflect.DeepEqual(res, WhatIfResult{}) {
		t.Fatalf("empty workload produced evidence: %+v", res)
	}
}
