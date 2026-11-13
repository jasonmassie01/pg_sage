package tuner

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

// market-10: the OR-join anti-pattern, and the advisory finding both
// anti-patterns produce.

const orJoinFilter = "((a.x = b.x) OR (a.y = b.y))"

// orJoinNode is a Nested Loop joining ta a (outer) to tb b (inner) with
// the given Join Filter; removed nil leaves the plan without actuals.
func orJoinNode(filter string, removed *float64, loops *int64) map[string]any {
	outer := map[string]any{"Node Type": "Seq Scan", "Relation Name": "ta",
		"Alias": "a", "Plan Rows": 10, "Parent Relationship": "Outer"}
	inner := map[string]any{"Node Type": "Seq Scan", "Relation Name": "tb",
		"Alias": "b", "Plan Rows": 10, "Parent Relationship": "Inner"}
	n := map[string]any{"Node Type": "Nested Loop", "Join Type": "Inner",
		"Plan Rows": 10, "Join Filter": filter,
		"Plans": []map[string]any{outer, inner}}
	if removed != nil {
		n["Rows Removed by Join Filter"] = *removed
		n["Actual Rows"] = 10.0
		outer["Actual Rows"], inner["Actual Rows"] = 10.0, 10.0
	}
	if loops != nil {
		n["Actual Loops"] = *loops
	}
	return n
}

func orJoinPlan(t *testing.T, filter string, removed float64, loops *int64) []byte {
	t.Helper()
	return planOf(t, orJoinNode(filter, ptr(removed), loops))
}

func oneORJoin(t *testing.T, plan []byte, opts ...ScanOption) PlanSymptom {
	t.Helper()
	got := scanKind(t, plan, SymptomORJoinFilter, opts...)
	if len(got) != 1 {
		t.Fatalf("or_join_filter symptoms = %d, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestORJoin_HappyPathFires(t *testing.T) {
	s := oneORJoin(t, orJoinPlan(t, orJoinFilter, 50_000, ptr(int64(1))))
	if s.NodeType != "Nested Loop" || !reflect.DeepEqual(s.JoinAliases, []string{"a", "b"}) {
		t.Fatalf("symptom identity = %+v", s)
	}
	want := map[string]any{"arms": []string{"(a.x = b.x)", "(a.y = b.y)"},
		"aliases": []string{"a", "b"}, "rows_removed": int64(50_000),
		"loops": int64(1), "join_filter": orJoinFilter}
	for k, v := range want {
		if !reflect.DeepEqual(s.Detail[k], v) {
			t.Errorf("detail %s = %#v, want %#v", k, s.Detail[k], v)
		}
	}
	p := Prescribe(s, TunerConfig{})
	if p == nil || p.Symptom != SymptomORJoinFilter || p.HintDirective != "" {
		t.Fatalf("prescription = %+v, want advisory with no hint", p)
	}
	for _, frag := range []string{"the OR across a and b is applied after the join",
		"rewriting as UNION (or UNION ALL with an exclusion predicate)",
		"lets each arm use its own index", "pg_sage does not rewrite statements",
		"50000 rows"} {
		if !strings.Contains(p.Rationale, frag) {
			t.Errorf("rationale %q lacks %q", p.Rationale, frag)
		}
	}
}

// One arm per input (no join predicate in either) is the classic UNION case.
func TestORJoin_ArmsOnDifferentInputsFire(t *testing.T) {
	s := oneORJoin(t, orJoinPlan(t, "((a.status = 'x'::text) OR (b.flag IS TRUE))",
		50_000, nil))
	if !reflect.DeepEqual(s.Detail["aliases"], []string{"a", "b"}) {
		t.Fatalf("aliases = %#v", s.Detail["aliases"])
	}
	three := oneORJoin(t, orJoinPlan(t, "((a.x = 1) OR (b.y = 2) OR (a.z = b.z))",
		50_000, nil))
	if arms, _ := three.Detail["arms"].([]string); len(arms) != 3 {
		t.Fatalf("three arms = %#v", three.Detail["arms"])
	}
}

func TestORJoin_NonQualifyingFiltersGiveNothing(t *testing.T) {
	for _, filter := range []string{
		"((b.x = 1) OR (b.y = 2))",                // every arm on one input
		"((a.x = 1) OR (a.y = 2))",                // every arm on the other input
		"((a.x = b.x) AND (a.y > b.y))",           // no OR
		"((a.x = b.x) OR (a.y = b.y)",             // malformed: unbalanced
		"(a.note = 'b.x OR a.y'::text)",           // OR only inside a literal
		"((c.x = 1) OR (d.y = 2))",                // aliases of neither input
		"((a.x = b.x) OR (2 = 2))",                // an arm references no input
		"",                                        // empty
		"((a.x = 1) OR ((b.y = 2) OR (b.z = 3)))", // nested OR: top level is a + b
	} {
		got := scanKind(t, orJoinPlan(t, filter, 50_000, nil), SymptomORJoinFilter)
		wantFire := strings.HasPrefix(filter, "((a.x = 1) OR ((b.y")
		if wantFire != (len(got) == 1) || len(got) > 1 {
			t.Errorf("filter %q: %d symptoms, want fire=%v", filter, len(got), wantFire)
		}
	}
}

func TestORJoin_RemovedBoundaryAndLoops(t *testing.T) {
	if got := scanKind(t, orJoinPlan(t, orJoinFilter, 9_999, nil),
		SymptomORJoinFilter); len(got) != 0 {
		t.Fatalf("9,999 removed fired: %+v", got)
	}
	oneORJoin(t, orJoinPlan(t, orJoinFilter, 10_000, nil))
	s := oneORJoin(t, orJoinPlan(t, orJoinFilter, 100, ptr(int64(200))))
	if s.Detail["rows_removed"] != int64(20_000) || s.Detail["loops"] != int64(200) {
		t.Fatalf("100 x 200 loops: detail = %v", s.Detail)
	}
	if got := scanKind(t, orJoinPlan(t, orJoinFilter, 100, ptr(int64(99))),
		SymptomORJoinFilter); len(got) != 0 {
		t.Fatalf("100 x 99 loops = 9,900 fired: %+v", got)
	}
}

func TestORJoin_OnlyNestedLoops(t *testing.T) {
	n := orJoinNode(orJoinFilter, ptr(50_000.0), nil)
	n["Node Type"] = "Hash Join"
	if got := scanKind(t, planOf(t, n), SymptomORJoinFilter); len(got) != 0 {
		t.Fatalf("a hash join is out of scope: %+v", got)
	}
}

// A misestimated nested loop whose join is a pure OR gets no HashJoin: with
// no equality between the inputs PostgreSQL cannot hash-join them, so the
// hint could not take effect (market-10b; this test used to pin
// HashJoin(a b), the accepted bug of market-10 Review issue 2). The
// advisory symptoms add no directive either.
func TestAntipatterns_DoNotChangeExistingHints(t *testing.T) {
	if got := misORJoinHint(t, orJoinFilter, true); got != "" {
		t.Fatalf("combined hint = %q, want none for a pure OR join", got)
	}
	idx := Prescribe(PlanSymptom{Kind: SymptomIndexFilterInefficient}, TunerConfig{})
	if idx == nil || CombineHints([]Prescription{*idx}) != "" {
		t.Fatalf("index filter prescription must add no directive: %+v", idx)
	}
}

// Sibling: an extra equality conjunct makes the join hashable, so the
// misestimated nested loop keeps HashJoin(a b).
func TestAntipatterns_EqualityConjunctKeepsHashJoin(t *testing.T) {
	filter := "((a.id = b.a_id) AND ((a.x = b.x) OR (a.y = b.y)))"
	if got := misORJoinHint(t, filter, false); got != "HashJoin(a b)" {
		t.Fatalf("combined hint = %q, want HashJoin(a b)", got)
	}
}

// misORJoinHint scans a misestimated nested loop with the given Join Filter
// and returns the combined hint; wantOR says whether or_join_filter fires.
func misORJoinHint(t *testing.T, filter string, wantOR bool) string {
	t.Helper()
	n := orJoinNode(filter, ptr(50_000.0), nil)
	n["Plan Rows"], n["Actual Rows"] = 1, 5_000.0
	inner := n["Plans"].([]map[string]any)[1]
	inner["Actual Rows"] = 20_000.0
	all, err := ScanPlan(planOf(t, n))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	if !hasKind(all, SymptomBadNestedLoop) || hasKind(all, SymptomORJoinFilter) != wantOR {
		t.Fatalf("want bad_nested_loop and or_join_filter=%v: %+v", wantOR, all)
	}
	var ps []Prescription
	for _, s := range all {
		if p := Prescribe(s, TunerConfig{}); p != nil {
			ps = append(ps, *p)
		}
	}
	return CombineHints(ps)
}

func TestAntipatterns_Titles(t *testing.T) {
	idx := buildTitle([]PlanSymptom{{Kind: SymptomIndexFilterInefficient}})
	or := buildTitle([]PlanSymptom{{Kind: SymptomORJoinFilter}})
	if idx == "Per-query tuning recommendation" || or == "Per-query tuning recommendation" ||
		idx == or {
		t.Fatalf("titles must be specific: %q, %q", idx, or)
	}
	both := buildTitle([]PlanSymptom{{Kind: SymptomIndexFilterInefficient},
		{Kind: SymptomORJoinFilter}})
	if !strings.Contains(both, "index filter") || !strings.Contains(both, "OR join") {
		t.Fatalf("multi-symptom title = %q", both)
	}
}

// State: the anti-pattern finding is informational even when hints could
// be installed: no recommended SQL and no hint directive.
func TestAntipatterns_FindingIsAdvisory(t *testing.T) {
	ready := &HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}
	tu := New(nil, TunerConfig{}, ready, noopLogFn)
	plan := planOf(t, idxScan(99_000, 1_000, nil).node())
	tu.plans = map[int64]planfresh.Plan{42: {JSON: plan, HasActuals: true,
		Source: planfresh.SourceCache}}
	got := tu.processCandidate(context.Background(),
		candidate{QueryID: 42, Query: "SELECT * FROM t WHERE a = 5 AND b = 7", Calls: 9})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	f := got[0]
	if f.Category != "query_tuning" || f.RecommendedSQL != "" || f.RollbackSQL != "" ||
		f.Detail["hint_directive"] != "" {
		t.Fatalf("finding must be advisory: %+v", f)
	}
	if !reflect.DeepEqual(f.Detail["symptoms"], []string{"index_filter_inefficient"}) ||
		!strings.Contains(f.Recommendation, "discards 99%") {
		t.Fatalf("finding detail/recommendation = %v / %q", f.Detail, f.Recommendation)
	}
	aps, ok := f.Detail["plan_antipatterns"].([]map[string]any)
	if !ok || len(aps) != 1 || aps[0]["symptom"] != "index_filter_inefficient" ||
		aps[0]["index_name"] != "idx_t_a" || aps[0]["rows_removed"] != int64(99_000) {
		t.Fatalf("plan_antipatterns detail = %#v", f.Detail["plan_antipatterns"])
	}
}

// A finding without anti-patterns carries no plan_antipatterns key.
func TestAntipatterns_DetailAbsentForOtherSymptoms(t *testing.T) {
	tu := New(nil, TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
	plan := []byte(`[{"Plan":{"Node Type":"Sort","Sort Space Type":"Disk",` +
		`"Sort Space Used":9000,"Plan Rows":10,"Actual Rows":10}}]`)
	tu.plans = map[int64]planfresh.Plan{7: {JSON: plan, HasActuals: true}}
	got := tu.processCandidate(context.Background(), candidate{QueryID: 7, Calls: 9})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	if _, ok := got[0].Detail["plan_antipatterns"]; ok {
		t.Fatalf("disk-sort finding must not carry plan_antipatterns: %v", got[0].Detail)
	}
}

// An operator-owned hint turns the finding into the operator-hint notice;
// it still carries the anti-pattern facts its recommendation cites.
func TestAntipatterns_OperatorHintFindingCarriesDetail(t *testing.T) {
	tu := New(nil, TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
	sort := map[string]any{"Node Type": "Sort", "Sort Space Type": "Disk",
		"Sort Space Used": 9000, "Plan Rows": 10, "Actual Rows": 10.0,
		"Plans": []map[string]any{idxScan(99_000, 1_000, nil).node()}}
	tu.plans = map[int64]planfresh.Plan{8: {JSON: planOf(t, sort), HasActuals: true}}
	got := tu.processCandidate(context.Background(),
		candidate{QueryID: 8, Calls: 9, foreignHint: true})
	if len(got) != 1 || got[0].Detail["operator_hint_present"] != true {
		t.Fatalf("findings = %+v, want one operator-hint finding", got)
	}
	aps, ok := got[0].Detail["plan_antipatterns"].([]map[string]any)
	if !ok || len(aps) != 1 || aps[0]["symptom"] != "index_filter_inefficient" ||
		!strings.Contains(got[0].Recommendation, "discards 99%") {
		t.Fatalf("operator-hint finding = %+v", got[0])
	}
}
