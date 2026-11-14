package tuner

import (
	"encoding/json"
	"strings"
	"testing"
)

// tuning-09: the bad-nested-loop symptom fires only when the row estimate
// is off by more than 10x (on the join or its outer child) AND the inner
// side read at least tuner.nested_loop_row_threshold rows in total
// (Actual Rows x Actual Loops). The old rule fired on any 10x misestimate,
// so a 1-row estimate that produced 11 rows earned a HashJoin hint.

// nlInner describes the inner child of a test nested loop. A nil rows
// leaves Actual Rows out (plain EXPLAIN); a zero loops leaves Actual Loops
// out.
type nlInner struct {
	rows  *float64
	loops int64
}

// nestedLoopPlanJSON builds EXPLAIN (ANALYZE, FORMAT JSON) output for a
// nested loop joining a (outer) and b (inner) through a parameterized index
// scan of b (Index Cond id = a.b_id; market-10b: without a join equality
// the join cannot be hashed and gets no HashJoin).
func nestedLoopPlanJSON(
	t *testing.T, joinPlan int64, joinActual float64,
	outerPlan int64, outerActual float64, inner nlInner,
) []byte {
	t.Helper()
	innerNode := map[string]any{
		"Node Type": "Index Scan", "Relation Name": "b", "Schema": "public",
		"Alias": "b", "Parent Relationship": "Inner", "Plan Rows": 1,
		"Index Cond": "(id = a.b_id)",
	}
	if inner.rows != nil {
		innerNode["Actual Rows"] = *inner.rows
	}
	if inner.loops > 0 {
		innerNode["Actual Loops"] = inner.loops
	}
	plan := []map[string]any{{"Plan": map[string]any{
		"Node Type": "Nested Loop", "Join Type": "Inner",
		"Plan Rows": joinPlan, "Actual Rows": joinActual, "Actual Loops": 1,
		"Plans": []any{
			map[string]any{
				"Node Type": "Seq Scan", "Relation Name": "a", "Schema": "public",
				"Alias": "a", "Parent Relationship": "Outer",
				"Plan Rows": outerPlan, "Actual Rows": outerActual, "Actual Loops": 1,
			},
			innerNode,
		},
	}}}
	out, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return out
}

func nestedLoopSymptoms(t *testing.T, plan []byte, opts ...ScanOption) []PlanSymptom {
	t.Helper()
	symptoms, err := ScanPlan(plan, opts...)
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	var out []PlanSymptom
	for _, s := range symptoms {
		if s.Kind == SymptomBadNestedLoop {
			out = append(out, s)
		}
	}
	return out
}

func detailInt64(t *testing.T, s PlanSymptom, key string) int64 {
	t.Helper()
	v, ok := s.Detail[key].(int64)
	if !ok {
		t.Fatalf("Detail[%q] = %#v (%T), want int64", key, s.Detail[key], s.Detail[key])
	}
	return v
}

// Boundary: a 20x misestimate with 9,999 / 10,000 / 10,001 inner rows under
// the default threshold (no option). Only the last two fire; 9,999 fired
// before tuning-09.
func TestNestedLoopThreshold_DefaultBoundary(t *testing.T) {
	cases := []struct {
		innerRows float64
		want      bool
	}{{9_999, false}, {10_000, true}, {10_001, true}}
	for _, tc := range cases {
		plan := nestedLoopPlanJSON(t, 100, 2_000, 100, 100,
			nlInner{rows: ptr(tc.innerRows), loops: 1})
		got := nestedLoopSymptoms(t, plan)
		if tc.want != (len(got) == 1) || len(got) > 1 {
			t.Fatalf("inner rows %.0f: got %d bad_nested_loop symptoms, want fire=%v",
				tc.innerRows, len(got), tc.want)
		}
		if !tc.want {
			continue
		}
		if total := detailInt64(t, got[0], "inner_rows_total"); total != int64(tc.innerRows) {
			t.Errorf("inner_rows_total = %d, want %.0f", total, tc.innerRows)
		}
		if loops := detailInt64(t, got[0], "loops"); loops != 1 {
			t.Errorf("loops = %d, want 1", loops)
		}
		if a := got[0].JoinAliases; len(a) != 2 || a[0] != "a" || a[1] != "b" {
			t.Errorf("aliases = %v, want [a b]", a)
		}
	}
}

// The case named in the problem: 1 estimated, 11 actual, 11 inner rows.
func TestNestedLoopThreshold_TinyMisestimateGivesNoHint(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 1, 11, 1, 11, nlInner{rows: ptr(1.0), loops: 11})
	if got := nestedLoopSymptoms(t, plan); len(got) != 0 {
		t.Fatalf("11-row nested loop flagged: %+v", got)
	}
}

// Happy path: 5,000 loops x 3 rows = 15,000 inner rows fires, and the
// detail and the prescription rationale carry the inner total and loops.
func TestNestedLoopThreshold_LoopsTimesRowsFires(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 50, 5_000, 50, 5_000, nlInner{rows: ptr(3.0), loops: 5_000})
	got := nestedLoopSymptoms(t, plan)
	if len(got) != 1 {
		t.Fatalf("got %d symptoms, want 1", len(got))
	}
	s := got[0]
	if v := detailInt64(t, s, "inner_rows_total"); v != 15_000 {
		t.Errorf("inner_rows_total = %d, want 15000", v)
	}
	if v := detailInt64(t, s, "loops"); v != 5_000 {
		t.Errorf("loops = %d, want 5000", v)
	}
	if v := detailInt64(t, s, "plan_rows"); v != 50 {
		t.Errorf("plan_rows = %d, want 50", v)
	}
	if v := detailInt64(t, s, "actual_rows"); v != 5_000 {
		t.Errorf("actual_rows = %d, want 5000", v)
	}
	p := Prescribe(s, TunerConfig{})
	if p == nil {
		t.Fatal("Prescribe returned nil for a two-alias nested loop")
	}
	if p.HintDirective != "HashJoin(a b)" {
		t.Errorf("hint = %q, want HashJoin(a b)", p.HintDirective)
	}
	for _, want := range []string{"15000", "5000"} {
		if !strings.Contains(p.Rationale, want) {
			t.Errorf("rationale %q lacks %q", p.Rationale, want)
		}
	}
}

// PG18 reports Actual Rows as a per-loop average with two decimals; the
// total is rows x loops (2.5 x 4,000 = 10,000 reaches the threshold).
func TestNestedLoopThreshold_FractionalRowsPG18(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 40, 4_000, 40, 4_000, nlInner{rows: ptr(2.5), loops: 4_000})
	got := nestedLoopSymptoms(t, plan)
	if len(got) != 1 {
		t.Fatalf("got %d symptoms, want 1", len(got))
	}
	if v := detailInt64(t, got[0], "inner_rows_total"); v != 10_000 {
		t.Errorf("inner_rows_total = %d, want 10000", v)
	}
	under := nestedLoopPlanJSON(t, 40, 4_000, 40, 4_000, nlInner{rows: ptr(2.49), loops: 4_000})
	if got := nestedLoopSymptoms(t, under); len(got) != 0 {
		t.Fatalf("2.49 x 4000 = 9960 inner rows fired: %+v", got)
	}
}

// Nil/zero: a missing Actual Loops counts as one loop.
func TestNestedLoopThreshold_MissingLoopsCountsOne(t *testing.T) {
	at := nestedLoopPlanJSON(t, 100, 2_000, 100, 100, nlInner{rows: ptr(10_000.0)})
	got := nestedLoopSymptoms(t, at)
	if len(got) != 1 {
		t.Fatalf("10000 rows, no loops: got %d symptoms, want 1", len(got))
	}
	if v := detailInt64(t, got[0], "loops"); v != 1 {
		t.Errorf("loops = %d, want 1", v)
	}
	below := nestedLoopPlanJSON(t, 100, 2_000, 100, 100, nlInner{rows: ptr(9_999.0)})
	if got := nestedLoopSymptoms(t, below); len(got) != 0 {
		t.Fatalf("9999 rows, no loops fired: %+v", got)
	}
}

// Nil: an inner side without Actual Rows cannot prove the threshold, so
// nothing fires even with a large join misestimate.
func TestNestedLoopThreshold_InnerWithoutActualsGivesNothing(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 1, 1_000_000, 1, 1_000_000, nlInner{})
	if got := nestedLoopSymptoms(t, plan); len(got) != 0 {
		t.Fatalf("inner without actuals fired: %+v", got)
	}
}

// Invalid input: plain EXPLAIN (no ANALYZE) carries no actuals at all.
func TestNestedLoopThreshold_PlainExplainGivesNothing(t *testing.T) {
	plain := `[{"Plan": {"Node Type": "Nested Loop", "Plan Rows": 1,
	  "Plans": [
	    {"Node Type": "Seq Scan", "Alias": "a", "Parent Relationship": "Outer",
	     "Plan Rows": 1},
	    {"Node Type": "Index Scan", "Alias": "b", "Parent Relationship": "Inner",
	     "Plan Rows": 1}]}}]`
	if got := nestedLoopSymptoms(t, []byte(plain)); len(got) != 0 {
		t.Fatalf("plain EXPLAIN fired: %+v", got)
	}
	if _, err := ScanPlan([]byte(`{"Plan": [`)); err == nil {
		t.Fatal("malformed plan JSON: want an error")
	}
}

// The misestimate may sit on the outer child: the join's own estimate is
// right, the outer side produced 50x its estimate, and the inner side was
// probed that many more times.
func TestNestedLoopThreshold_OuterMisestimateFires(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 20_000, 20_000, 10, 500, nlInner{rows: ptr(40.0), loops: 500})
	got := nestedLoopSymptoms(t, plan)
	if len(got) != 1 {
		t.Fatalf("outer 50x misestimate: got %d symptoms, want 1", len(got))
	}
	if v := detailInt64(t, got[0], "inner_rows_total"); v != 20_000 {
		t.Errorf("inner_rows_total = %d, want 20000", v)
	}
}

// Accurate estimates: a big nested loop the planner chose knowingly is not
// a bad nested loop, however many rows the inner side reads.
func TestNestedLoopThreshold_AccurateEstimateGivesNothing(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 50_000, 50_000, 500, 500,
		nlInner{rows: ptr(100.0), loops: 500})
	if got := nestedLoopSymptoms(t, plan); len(got) != 0 {
		t.Fatalf("accurate estimate fired: %+v", got)
	}
}

// Boundary on the ratio: exactly 10x on both the join and the outer child
// is not a misestimate (the rule needs more than 10x).
func TestNestedLoopThreshold_ExactlyTenXGivesNothing(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 1_000, 10_000, 100, 1_000,
		nlInner{rows: ptr(50.0), loops: 1_000})
	if got := nestedLoopSymptoms(t, plan); len(got) != 0 {
		t.Fatalf("exactly 10x fired: %+v", got)
	}
}

// State transition: a skewed plan on a stale table whose inner side stays
// below the threshold is no longer turned into an ANALYZE (stale_stats)
// symptom; above the threshold it still is (positive control).
func TestNestedLoopThreshold_StaleStatsRoutingFollowsThreshold(t *testing.T) {
	cache := mkCache(TunerConfig{StaleStatsModRatio: 0.1})
	cache.entries["public.a"] = staleStatsEntry{liveTuples: 1000, modSinceAnalyze: 900}
	route := func(innerRows float64) []PlanSymptom {
		plan := nestedLoopPlanJSON(t, 100, 2_000, 100, 2_000,
			nlInner{rows: ptr(innerRows), loops: 1})
		symptoms, err := ScanPlan(plan)
		if err != nil {
			t.Fatalf("ScanPlan: %v", err)
		}
		return annotateForStaleStats(plan, symptoms, cache)
	}
	for _, s := range route(50) {
		if s.Kind == SymptomStaleStats || s.Kind == SymptomBadNestedLoop {
			t.Fatalf("50 inner rows routed to %s: %+v", s.Kind, s)
		}
	}
	got := route(20_000)
	if len(got) != 1 || got[0].Kind != SymptomStaleStats || got[0].RelationName != "a" {
		t.Fatalf("20000 inner rows on a stale table: got %+v, want one stale_stats on a", got)
	}
}
