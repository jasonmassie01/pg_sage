package tuner

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// market-10: plan anti-patterns the index-missing rules do not catch. An
// index scan whose filter discards most of what the index read, and a
// nested loop whose OR join filter spans both inputs (a UNION candidate).
// Both are advisory: no hint, no rewrite.
//
// No concurrent access tests: ScanPlan and Prescribe are pure over their
// input and share no state between calls.

// scanNode is a fixture scan node; nil fields are left out of the JSON.
type scanNode struct {
	nodeType, index, cond, filter, alias string
	removed, rows                        *float64
	loops                                *int64
}

func (s scanNode) node() map[string]any {
	n := map[string]any{"Node Type": s.nodeType, "Relation Name": "t",
		"Schema": "public", "Alias": s.alias, "Plan Rows": 10}
	if s.alias == "" {
		n["Alias"] = "t"
	}
	setIf(n, "Index Name", s.index)
	setIf(n, "Index Cond", s.cond)
	setIf(n, "Filter", s.filter)
	if s.removed != nil {
		n["Rows Removed by Filter"] = *s.removed
	}
	if s.rows != nil {
		n["Actual Rows"] = *s.rows
	}
	if s.loops != nil {
		n["Actual Loops"] = *s.loops
	}
	return n
}

func setIf(n map[string]any, key, v string) {
	if v != "" {
		n[key] = v
	}
}

func planOf(t *testing.T, root map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{"Plan": root}})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// idxScan is an Index Scan on t using idx_t_a with Index Cond (a = 5) and
// Filter (b = 7).
func idxScan(removed, kept float64, loops *int64) scanNode {
	return scanNode{nodeType: "Index Scan", index: "idx_t_a", cond: "(a = 5)",
		filter: "(b = 7)", removed: ptr(removed), rows: ptr(kept), loops: loops}
}

func scanKind(t *testing.T, plan []byte, k SymptomKind, opts ...ScanOption,
) []PlanSymptom {
	t.Helper()
	all, err := ScanPlan(plan, opts...)
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	var out []PlanSymptom
	for _, s := range all {
		if s.Kind == k {
			out = append(out, s)
		}
	}
	return out
}

func oneIndexFilter(t *testing.T, plan []byte, opts ...ScanOption) PlanSymptom {
	t.Helper()
	got := scanKind(t, plan, SymptomIndexFilterInefficient, opts...)
	if len(got) != 1 {
		t.Fatalf("index_filter_inefficient symptoms = %d, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestIndexFilter_HappyPathFiresWithShare(t *testing.T) {
	s := oneIndexFilter(t, planOf(t, idxScan(99_000, 1_000, ptr(int64(1))).node()))
	if s.IndexName != "idx_t_a" || s.RelationName != "t" || s.Schema != "public" ||
		s.Alias != "t" || s.NodeType != "Index Scan" {
		t.Fatalf("symptom identity = %+v", s)
	}
	want := map[string]any{"index_name": "idx_t_a", "index_cond": "(a = 5)",
		"filter": "(b = 7)", "filter_columns": []string{"b"},
		"rows_removed": int64(99_000), "rows_kept": int64(1_000), "loops": int64(1)}
	for k, v := range want {
		if !reflect.DeepEqual(s.Detail[k], v) {
			t.Errorf("detail %s = %#v, want %#v", k, s.Detail[k], v)
		}
	}
	if share, _ := s.Detail["removed_share"].(float64); math.Abs(share-0.99) > 1e-9 {
		t.Fatalf("removed_share = %v, want 0.99", s.Detail["removed_share"])
	}
	p := Prescribe(s, TunerConfig{})
	if p == nil || p.Symptom != SymptomIndexFilterInefficient || p.HintDirective != "" {
		t.Fatalf("prescription = %+v, want advisory with no hint", p)
	}
	for _, frag := range []string{"index idx_t_a reads 100000 rows per execution",
		"discards 99%", "an index on (a, b) would read only the kept rows"} {
		if !strings.Contains(p.Rationale, frag) {
			t.Errorf("rationale %q lacks %q", p.Rationale, frag)
		}
	}
}

func TestIndexFilter_IndexOnlyAndBitmapHeapScanFire(t *testing.T) {
	only := idxScan(50_000, 10, nil)
	only.nodeType = "Index Only Scan"
	if s := oneIndexFilter(t, planOf(t, only.node())); s.NodeType != "Index Only Scan" {
		t.Fatalf("index only scan symptom = %+v", s)
	}
	heap := scanNode{nodeType: "Bitmap Heap Scan", filter: "(b = 7)",
		removed: ptr(50_000.0), rows: ptr(10.0)}.node()
	heap["Plans"] = []map[string]any{{"Node Type": "Bitmap Index Scan",
		"Index Name": "idx_t_a", "Index Cond": "(a = 5)", "Plan Rows": 10,
		"Actual Rows": 50_010.0, "Parent Relationship": "Outer"}}
	s := oneIndexFilter(t, planOf(t, heap))
	if s.IndexName != "idx_t_a" || s.Detail["index_cond"] != "(a = 5)" {
		t.Fatalf("bitmap heap scan must name the bitmap index and its cond: %+v", s)
	}
	seq := idxScan(99_000, 1_000, nil)
	seq.nodeType, seq.index, seq.cond = "Seq Scan", "", ""
	if got := scanKind(t, planOf(t, seq.node()), SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("a seq scan is not an index filter problem: %+v", got)
	}
}

func TestIndexFilter_RemovedBoundary(t *testing.T) {
	below := planOf(t, idxScan(9_999, 0, ptr(int64(1))).node())
	if got := scanKind(t, below, SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("9,999 removed fired: %+v", got)
	}
	oneIndexFilter(t, planOf(t, idxScan(10_000, 0, ptr(int64(1))).node()))
}

func TestIndexFilter_ShareBoundary(t *testing.T) {
	low := planOf(t, idxScan(89_000, 11_000, nil).node())
	if got := scanKind(t, low, SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("share 0.89 fired: %+v", got)
	}
	s := oneIndexFilter(t, planOf(t, idxScan(90_000, 10_000, nil).node()))
	if share, _ := s.Detail["removed_share"].(float64); math.Abs(share-0.90) > 1e-9 {
		t.Fatalf("removed_share = %v, want 0.90", s.Detail["removed_share"])
	}
}

func TestIndexFilter_RemovedAndKeptAreMultipliedByLoops(t *testing.T) {
	s := oneIndexFilter(t, planOf(t, idxScan(100, 0, ptr(int64(200))).node()))
	if s.Detail["rows_removed"] != int64(20_000) || s.Detail["loops"] != int64(200) {
		t.Fatalf("100 x 200 loops: detail = %v", s.Detail)
	}
	few := planOf(t, idxScan(100, 0, ptr(int64(99))).node())
	if got := scanKind(t, few, SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("100 x 99 loops = 9,900 fired: %+v", got)
	}
	// 100 removed and 12 kept per loop is an 89% share whatever the loops.
	kept := planOf(t, idxScan(100, 12, ptr(int64(200))).node())
	if got := scanKind(t, kept, SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("kept rows must scale by loops too: %+v", got)
	}
}

func TestIndexFilter_MissingLoopsCountsAsOne(t *testing.T) {
	s := oneIndexFilter(t, planOf(t, idxScan(10_000, 5, nil).node()))
	if s.Detail["rows_removed"] != int64(10_000) || s.Detail["loops"] != int64(1) ||
		s.Detail["rows_kept"] != int64(5) {
		t.Fatalf("no Actual Loops must count as one loop: %v", s.Detail)
	}
	zero := oneIndexFilter(t, planOf(t, idxScan(10_000, 5, ptr(int64(0))).node()))
	if zero.Detail["loops"] != int64(1) {
		t.Fatalf("Actual Loops 0 must count as one loop: %v", zero.Detail)
	}
}

func TestAntipatternThresholds_ZeroAndNegativeUseDefault(t *testing.T) {
	for _, n := range []int64{0, -1} {
		below := planOf(t, idxScan(9_999, 0, nil).node())
		if got := scanKind(t, below, SymptomIndexFilterInefficient,
			WithIndexFilterMinRemoved(n)); len(got) != 0 {
			t.Fatalf("threshold %d: 9,999 removed fired", n)
		}
		oneIndexFilter(t, planOf(t, idxScan(10_000, 0, nil).node()), WithIndexFilterMinRemoved(n))
		orBelow := orJoinPlan(t, orJoinFilter, 9_999, nil)
		if got := scanKind(t, orBelow, SymptomORJoinFilter,
			WithORJoinMinRemoved(n)); len(got) != 0 {
			t.Fatalf("threshold %d: OR join removing 9,999 fired", n)
		}
		oneORJoin(t, orJoinPlan(t, orJoinFilter, 10_000, nil), WithORJoinMinRemoved(n))
	}
}

func TestAntipatternThresholds_ConfiguredValuesApply(t *testing.T) {
	low := planOf(t, idxScan(499, 0, nil).node())
	if got := scanKind(t, low, SymptomIndexFilterInefficient,
		WithIndexFilterMinRemoved(500)); len(got) != 0 {
		t.Fatal("499 removed fired under a 500 threshold")
	}
	oneIndexFilter(t, planOf(t, idxScan(500, 0, nil).node()), WithIndexFilterMinRemoved(500))
	// One option does not move the other rule's threshold.
	if got := scanKind(t, orJoinPlan(t, orJoinFilter, 500, nil), SymptomORJoinFilter,
		WithIndexFilterMinRemoved(500)); len(got) != 0 {
		t.Fatal("the index threshold moved the OR-join threshold")
	}
	oneORJoin(t, orJoinPlan(t, orJoinFilter, 500, nil), WithORJoinMinRemoved(500))
	big := planOf(t, idxScan(20_000, 0, nil).node())
	if got := scanKind(t, big, SymptomIndexFilterInefficient,
		WithIndexFilterMinRemoved(50_000)); len(got) != 0 {
		t.Fatal("20,000 removed fired under a 50,000 threshold")
	}
}

func TestAntipatterns_PlainExplainGivesNothing(t *testing.T) {
	plain := scanNode{nodeType: "Index Scan", index: "idx_t_a", cond: "(a = 5)",
		filter: "(b = 7)"}
	nl := orJoinNode(orJoinFilter, nil, nil)
	for name, plan := range map[string][]byte{"index": planOf(t, plain.node()),
		"or-join": planOf(t, nl)} {
		all, err := ScanPlan(plan)
		if err != nil {
			t.Fatalf("%s: ScanPlan: %v", name, err)
		}
		for _, s := range all {
			if s.Kind == SymptomIndexFilterInefficient || s.Kind == SymptomORJoinFilter {
				t.Fatalf("%s: plain EXPLAIN yielded %s", name, s.Kind)
			}
		}
	}
	// Actuals present but the plan is flagged estimates-only: skipped too.
	analyzed := planOf(t, idxScan(99_000, 1_000, nil).node())
	if got := scanKind(t, analyzed, SymptomIndexFilterInefficient,
		WithEstimatesOnly()); len(got) != 0 {
		t.Fatalf("estimates-only plan yielded the index symptom: %+v", got)
	}
	// Rows Removed by Filter without Actual Rows is not an executed plan.
	half := scanNode{nodeType: "Index Scan", index: "i", filter: "(b = 7)",
		removed: ptr(99_000.0)}
	if got := scanKind(t, planOf(t, half.node()), SymptomIndexFilterInefficient); len(got) != 0 {
		t.Fatalf("no Actual Rows must give nothing: %+v", got)
	}
}

func TestIndexFilter_UnparseableFilterStillFiresWithoutColumns(t *testing.T) {
	for _, filter := range []string{"((b = 7) AND (c", "((b = 7) OR (c = 8))",
		"(lower(b) = 'x'::text)"} {
		n := idxScan(99_000, 1_000, nil)
		n.filter = filter
		s := oneIndexFilter(t, planOf(t, n.node()))
		cols, ok := s.Detail["filter_columns"].([]string)
		if !ok || len(cols) != 0 {
			t.Fatalf("filter %q: filter_columns = %#v, want empty []string", filter,
				s.Detail["filter_columns"])
		}
		p := Prescribe(s, TunerConfig{})
		if strings.Contains(p.Rationale, "()") || strings.Contains(p.Rationale, "(a, )") ||
			!strings.Contains(p.Rationale, "idx_t_a") {
			t.Fatalf("filter %q: rationale %q", filter, p.Rationale)
		}
	}
}

func TestParsing_FractionalRemovedCountersParse(t *testing.T) {
	// PG18 prints per-loop averages with decimals; an int field failed the
	// whole plan.
	s := oneIndexFilter(t, planOf(t, idxScan(12.5, 0.5, ptr(int64(1_000))).node()))
	if s.Detail["rows_removed"] != int64(12_500) || s.Detail["rows_kept"] != int64(500) {
		t.Fatalf("12.5 x 1000 loops: detail = %v", s.Detail)
	}
	or := oneORJoin(t, orJoinPlan(t, orJoinFilter, 20.25, ptr(int64(1_000))))
	if or.Detail["rows_removed"] != int64(20_250) {
		t.Fatalf("20.25 x 1000 loops: detail = %v", or.Detail)
	}
}
