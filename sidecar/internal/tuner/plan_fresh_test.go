package tuner

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

// No concurrent access tests: ScanPlan and planMaxAge are pure.

func TestTuner_PlanMaxAgeDefaults(t *testing.T) {
	cases := map[int]time.Duration{0: 24 * time.Hour, -1: 24 * time.Hour,
		90: 90 * time.Minute}
	for minutes, want := range cases {
		tu := New(nil, TunerConfig{PlanMaxAgeMinutes: minutes}, nil, noopLogFn)
		if got := tu.planMaxAge(); got != want {
			t.Errorf("plan_max_age_minutes=%d: %v, want %v", minutes, got, want)
		}
	}
}

// actualsPlan holds a disk sort, a hash spill and a 50000x nested-loop
// misestimate under a Limit whose sort reads 100x the rows it needs.
const actualsPlan = `[{"Plan":{"Node Type":"Limit","Plan Rows":10,"Plans":[
	{"Node Type":"Sort","Plan Rows":1000,"Sort Space Type":"Disk","Sort Space Used":9000,
	 "Plans":[{"Node Type":"Hash Join","Plan Rows":1000,"Plans":[
	  {"Node Type":"Nested Loop","Plan Rows":10,"Actual Rows":500000,"Actual Loops":1,
	   "Plans":[
	    {"Node Type":"Seq Scan","Relation Name":"o","Alias":"o","Plan Rows":10,
	     "Actual Rows":1000,"Actual Loops":1},
	    {"Node Type":"Index Scan","Relation Name":"i","Alias":"i","Plan Rows":1,
	     "Actual Rows":500,"Actual Loops":1000}]},
	  {"Node Type":"Hash","Hash Batches":8,"Plan Rows":5}]}]}]}}]`

func TestScanPlan_EstimatesOnlySkipsActualsRules(t *testing.T) {
	all, err := ScanPlan([]byte(actualsPlan))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, k := range []SymptomKind{SymptomDiskSort, SymptomHashSpill, SymptomBadNestedLoop,
		SymptomSortLimit} {
		if !hasKind(all, k) {
			t.Fatalf("with actuals the plan yields %s: %v", k, all)
		}
	}
	est, err := ScanPlan([]byte(actualsPlan), WithEstimatesOnly())
	if err != nil {
		t.Fatalf("scan estimates only: %v", err)
	}
	if len(est) != 1 || est[0].Kind != SymptomSortLimit {
		t.Fatalf("estimates only keeps just the estimate rules: %v", est)
	}
	if _, err := ScanPlan([]byte("not json"), WithEstimatesOnly()); err == nil {
		t.Fatal("invalid JSON must still fail")
	}
}

func TestTuner_NoPoolHasNoPlan(t *testing.T) {
	tu := New(nil, TunerConfig{}, nil, noopLogFn)
	if p := tu.cyclePlan(t.Context(), 1); p.Source != planfresh.SourceNone ||
		len(p.JSON) != 0 {
		t.Fatalf("no pool: no plan, got %+v", p)
	}
}
