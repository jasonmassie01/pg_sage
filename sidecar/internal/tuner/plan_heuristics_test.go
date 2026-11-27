package tuner

import (
	"strings"
	"testing"
)

// market-10b: a misestimated nested loop gets HashJoin only when its join
// has an equality between the two inputs; a pure OR (or no join condition
// at all) cannot be hash-joined, so pg_hint_plan cannot force one.
//
// Error propagation: not applicable (pure plan walk, no I/O).
// Concurrency: not applicable (pure functions over one plan value).

// misNestedLoop is a Nested Loop of ta a (outer) and the given inner node,
// estimated at 1 row but returning 5,000, whose inner side read 20,000
// rows: a bad_nested_loop at the default thresholds. filter "" leaves the
// Join Filter out.
func misNestedLoop(filter string, inner map[string]any) map[string]any {
	outer := map[string]any{"Node Type": "Seq Scan", "Relation Name": "ta",
		"Alias": "a", "Plan Rows": 10, "Actual Rows": 10.0,
		"Parent Relationship": "Outer"}
	inner["Parent Relationship"] = "Inner"
	if _, ok := inner["Actual Rows"]; !ok {
		inner["Actual Rows"] = 20_000.0
	}
	n := map[string]any{"Node Type": "Nested Loop", "Join Type": "Inner",
		"Plan Rows": 1, "Actual Rows": 5_000.0,
		"Plans": []map[string]any{outer, inner}}
	if filter != "" {
		n["Join Filter"] = filter
	}
	return n
}

// innerScan is a scan of tb b with the given extra keys (conditions).
func innerScan(nodeType string, extra map[string]any) map[string]any {
	n := map[string]any{"Node Type": nodeType, "Relation Name": "tb",
		"Alias": "b", "Plan Rows": 10}
	for k, v := range extra {
		n[k] = v
	}
	return n
}

// oneBadNestedLoop scans plan and returns its single bad_nested_loop.
func oneBadNestedLoop(t *testing.T, root map[string]any) PlanSymptom {
	t.Helper()
	got := scanKind(t, planOf(t, root), SymptomBadNestedLoop)
	if len(got) != 1 {
		t.Fatalf("bad_nested_loop symptoms = %d, want 1: %+v", len(got), got)
	}
	return got[0]
}

func hashableOf(t *testing.T, s PlanSymptom) bool {
	t.Helper()
	h, ok := s.Detail["hashable_join"].(bool)
	if !ok {
		t.Fatalf("detail lacks a bool hashable_join: %#v", s.Detail)
	}
	return h
}

// Happy path: an a.id = b.a_id join with a bad estimate still gets HashJoin.
func TestHashable_EqualityJoinKeepsHashJoin(t *testing.T) {
	s := oneBadNestedLoop(t, misNestedLoop("(a.id = b.a_id)", innerScan("Seq Scan", nil)))
	if !hashableOf(t, s) {
		t.Fatalf("a.id = b.a_id must be hashable: %#v", s.Detail)
	}
	p := Prescribe(s, TunerConfig{})
	if p == nil || p.HintDirective != "HashJoin(a b)" ||
		!strings.Contains(p.Rationale, "inner side read 20000 rows over 1 loops") {
		t.Fatalf("prescription = %+v, want HashJoin(a b)", p)
	}
}

// The bug: a pure OR join gets no join hint and keeps or_join_filter.
func TestHashable_PureORGetsNoJoinHint(t *testing.T) {
	n := misNestedLoop(orJoinFilter, innerScan("Seq Scan", nil))
	n["Rows Removed by Join Filter"] = 50_000.0
	all, err := ScanPlan(planOf(t, n))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	var bnl *PlanSymptom
	for i := range all {
		if all[i].Kind == SymptomBadNestedLoop {
			bnl = &all[i]
		}
	}
	if bnl == nil || !hasKind(all, SymptomORJoinFilter) {
		t.Fatalf("want bad_nested_loop and or_join_filter: %+v", all)
	}
	if hashableOf(t, *bnl) {
		t.Fatalf("a pure OR join must not be hashable: %#v", bnl.Detail)
	}
	if p := Prescribe(*bnl, TunerConfig{}); p != nil {
		t.Fatalf("pure OR join prescribed %+v, want nil", p)
	}
	// The thresholds did not move: the symptom still reports the same totals.
	if bnl.Detail["inner_rows_total"] != int64(20_000) || bnl.Detail["loops"] != int64(1) ||
		bnl.Detail["plan_rows"] != int64(1) || bnl.Detail["actual_rows"] != int64(5_000) {
		t.Fatalf("detail = %#v", bnl.Detail)
	}
}

// Boundary: which Join Filters are hashable.
func TestHashable_JoinFilterBoundaries(t *testing.T) {
	cases := []struct {
		filter string
		want   bool
	}{
		{"((a.x = b.x) AND ((a.y = 1) OR (b.z = 2)))", true}, // AND conjunct
		{"((a.id = b.a_id) AND ((a.x = b.x) OR (a.y = b.y)))", true},
		{"(b.x = a.x)", true},                       // inner on the left
		{"((a.code)::text = (b.code)::text)", true}, // casts
		{`("a".x = "b"."Weird Col")`, true},         // quoted names
		{"((a.x = 5) AND (b.y = 7))", false},        // no cross-input equality
		{"(a.x = a.y)", false},                      // one input
		{"(b.x = b.y)", false},                      // the other input
		{"((a.x = b.x) OR (a.y = b.y))", false},     // pure OR
		{"(a.x < b.x)", false},                      // not an equality
		{"(a.x <> b.x)", false},                     // not an equality
		{"(a.x = ANY (b.arr))", false},              // array membership
		{"(lower(a.x) = lower(b.x))", false},        // expression: fail closed
		{"(c.x = b.x)", false},                      // alias of neither input
		{"(a.note = 'b.x = a.x'::text)", false},     // equality inside a literal
		{"((a.x = b.x) OR (a.y = b.y)", false},      // unbalanced
		{"a.x = b.x = c.x", false},                  // unparseable
		{"not a filter at all", false},              // unparseable
		{"()", false},                               // empty parens
	}
	for _, c := range cases {
		s := oneBadNestedLoop(t, misNestedLoop(c.filter, innerScan("Seq Scan", nil)))
		if got := hashableOf(t, s); got != c.want {
			t.Errorf("Join Filter %q: hashable = %v, want %v", c.filter, got, c.want)
		}
		if p := Prescribe(s, TunerConfig{}); (p != nil) != c.want {
			t.Errorf("Join Filter %q: prescription %+v, want hint=%v", c.filter, p, c.want)
		}
	}
}

// A parameterized inner scan carries the join equality in its own
// conditions: the inner column is unqualified and the outer one qualified.
func TestHashable_ParameterizedInnerScans(t *testing.T) {
	bitmapIdx := map[string]any{"Node Type": "Bitmap Index Scan",
		"Index Name": "tb_a_id_idx", "Plan Rows": 10, "Index Cond": "(a_id = a.id)",
		"Parent Relationship": "Outer"}
	cases := []struct {
		name  string
		inner map[string]any
		want  bool
	}{
		{"index cond", innerScan("Index Scan", map[string]any{
			"Index Cond": "(a_id = a.id)"}), true},
		{"index cond qualified", innerScan("Index Scan", map[string]any{
			"Index Cond": "(b.a_id = a.id)"}), true},
		{"index cond reversed", innerScan("Index Only Scan", map[string]any{
			"Index Cond": "((tenant = 3) AND (a.id = a_id))"}), true},
		{"filter", innerScan("Seq Scan", map[string]any{"Filter": "(a_id = a.id)"}), true},
		{"recheck cond", innerScan("Bitmap Heap Scan", map[string]any{
			"Recheck Cond": "(a_id = a.id)"}), true},
		{"bitmap index child", innerScan("Bitmap Heap Scan", map[string]any{
			"Plans": []map[string]any{bitmapIdx}}), true},
		{"memoize", map[string]any{"Node Type": "Memoize", "Plan Rows": 1,
			"Plans": []map[string]any{innerScan("Index Scan", map[string]any{
				"Index Cond": "(a_id = a.id)", "Parent Relationship": "Outer"})}}, true},
		{"materialize", map[string]any{"Node Type": "Materialize", "Plan Rows": 1,
			"Plans": []map[string]any{innerScan("Seq Scan", map[string]any{
				"Filter": "(a_id = a.id)", "Parent Relationship": "Outer"})}}, true},
		{"constant only", innerScan("Index Scan", map[string]any{
			"Index Cond": "(a_id = 5)"}), false},
		{"two inner columns", innerScan("Seq Scan", map[string]any{
			"Filter": "(a_id = other_id)"}), false},
		{"param of another loop", innerScan("Index Scan", map[string]any{
			"Index Cond": "(a_id = z.id)"}), false},
		{"or of params", innerScan("Seq Scan", map[string]any{
			"Filter": "((a_id = a.id) OR (x = a.x))"}), false},
		{"bitmap or", innerScan("Bitmap Heap Scan", map[string]any{
			"Recheck Cond": "((a.x = x) OR (a.y = y))",
			"Plans": []map[string]any{{"Node Type": "BitmapOr", "Plan Rows": 0,
				"Parent Relationship": "Outer", "Plans": []map[string]any{
					bitmapIndex("tb_x_idx", "(x = a.x)"),
					bitmapIndex("tb_y_idx", "(y = a.y)")}}}}), false},
		{"no conditions", innerScan("Seq Scan", nil), false},
	}
	for _, c := range cases {
		s := oneBadNestedLoop(t, misNestedLoop("", c.inner))
		if got := hashableOf(t, s); got != c.want {
			t.Errorf("%s: hashable = %v, want %v", c.name, got, c.want)
		}
	}
}

// bitmapIndex is a Bitmap Index Scan child with the given Index Cond.
func bitmapIndex(index, cond string) map[string]any {
	return map[string]any{"Node Type": "Bitmap Index Scan", "Index Name": index,
		"Plan Rows": 10, "Index Cond": cond, "Parent Relationship": "Member"}
}

// Nil/empty: a nested loop without both inputs, or a zero node, is not
// hashable; hashableJoin must not panic on them.
func TestHashable_NilAndEmpty(t *testing.T) {
	onlyOuter := planNode{NodeType: "Nested Loop", JoinFilter: ptr("(a.x = b.x)"),
		Plans: []planNode{{NodeType: "Seq Scan", Alias: "a", ParentRelationship: "Outer"}}}
	if hashableJoin(onlyOuter) {
		t.Fatal("a nested loop with no inner child must not be hashable")
	}
	if hashableJoin(planNode{}) {
		t.Fatal("a zero plan node must not be hashable")
	}
	empty := ""
	n := planNode{NodeType: "Nested Loop", JoinFilter: &empty, Plans: []planNode{
		{NodeType: "Seq Scan", Alias: "a", ParentRelationship: "Outer"},
		{NodeType: "Seq Scan", Alias: "b", ParentRelationship: "Inner", Filter: &empty}}}
	if hashableJoin(n) {
		t.Fatal("empty Join Filter and Filter must not be hashable")
	}
}

// Recheck Cond is read from the plan JSON (it was dropped before).
func TestHashable_RecheckCondParsed(t *testing.T) {
	root, err := parsePlanRoot([]byte(`{"Plan": {"Node Type": "Bitmap Heap Scan",
		"Recheck Cond": "(a_id = a.id)"}}`))
	if err != nil {
		t.Fatalf("parsePlanRoot: %v", err)
	}
	if root.RecheckCond == nil || *root.RecheckCond != "(a_id = a.id)" {
		t.Fatalf("Recheck Cond = %v", root.RecheckCond)
	}
}

// The EXPLAIN page's advice drops the HashJoin suggestion for a join that
// cannot be hashed and says why; a hashable one keeps it.
func TestHashable_DescribeDropsHashJoinAdvice(t *testing.T) {
	s := PlanSymptom{Kind: SymptomBadNestedLoop, JoinAliases: []string{"a", "b"},
		Detail: map[string]any{"hashable_join": false}}
	_, rec := Describe(s)
	if strings.Contains(rec, "HashJoin(") || !strings.Contains(rec, "no equality") ||
		!strings.HasPrefix(rec, "ANALYZE the joined tables") {
		t.Fatalf("non-hashable recommendation = %q", rec)
	}
	s.Detail["hashable_join"] = true
	if _, rec := Describe(s); !strings.Contains(rec, "HashJoin(a b)") {
		t.Fatalf("hashable recommendation = %q", rec)
	}
}
