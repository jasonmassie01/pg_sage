package tuner

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

var defaultCfg = TunerConfig{
	Enabled:      true,
	WorkMemMaxMB: 512,
}

func TestPrescribe_DiskSort(t *testing.T) {
	s := PlanSymptom{
		Kind:   SymptomDiskSort,
		Detail: map[string]any{"sort_space_kb": int64(4096)},
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.Symptom != SymptomDiskSort {
		t.Errorf("symptom = %v, want disk_sort", p.Symptom)
	}
	if !strings.Contains(p.HintDirective, "work_mem") {
		t.Errorf("directive = %q, want work_mem",
			p.HintDirective)
	}
}

func TestPrescribe_HashSpill(t *testing.T) {
	s := PlanSymptom{
		Kind: SymptomHashSpill,
		Detail: map[string]any{
			"peak_memory_kb": int64(8192),
			"hash_batches":   int64(16),
		},
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if !strings.Contains(p.HintDirective, "work_mem") {
		t.Errorf("directive = %q, want work_mem",
			p.HintDirective)
	}
}

// tuning-11: high planning time prescribes no hint. A forced generic plan
// does nothing for simple-protocol statements and can be worse on skewed
// data; the statement gets query_parameterization advice instead. (This
// test pinned Set(plan_cache_mode "force_generic_plan") before tuning-11.)
func TestPrescribe_HighPlanTime(t *testing.T) {
	s := PlanSymptom{Kind: SymptomHighPlanTime}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.Symptom != SymptomHighPlanTime || p.HintDirective != "" || p.Rationale == "" {
		t.Errorf("prescription = %+v, want high_plan_time, no directive, a rationale", p)
	}
}

// Other symptoms keep their hints when combined with high planning time.
func TestPrescribe_HighPlanTimeLeavesOtherHints(t *testing.T) {
	tu := &Tuner{cfg: defaultCfg}
	ps := tu.prescribeAll([]PlanSymptom{{Kind: SymptomHighPlanTime},
		{Kind: SymptomDiskSort, Detail: map[string]any{"sort_space_kb": int64(65536)}}})
	got := CombineHints(ps)
	if got != `Set(work_mem "128MB")` {
		t.Errorf("combined = %q, want only the work_mem hint", got)
	}
}

// A hash-join hint names the join's child aliases (Phase 0 item 11).
func TestPrescribe_BadNestedLoop(t *testing.T) {
	s := PlanSymptom{
		Kind:        SymptomBadNestedLoop,
		JoinAliases: []string{"o", "c"},
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.HintDirective != "HashJoin(o c)" {
		t.Errorf("directive = %q, want HashJoin(o c)",
			p.HintDirective)
	}
}

// HashJoin(items) with one relation is a pg_hint_plan no-op; this test
// used to expect it.
func TestPrescribe_BadNestedLoop_NoAlias(t *testing.T) {
	s := PlanSymptom{
		Kind:         SymptomBadNestedLoop,
		RelationName: "items",
	}
	if p := Prescribe(s, defaultCfg); p != nil {
		t.Fatalf("single-relation join hint prescribed: %+v", p)
	}
}

func TestPrescribe_SeqScanWithIndex(t *testing.T) {
	s := PlanSymptom{
		Kind:      SymptomSeqScanWithIndex,
		Alias:     "u",
		IndexName: "users_pkey",
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	want := "IndexScan(u users_pkey)"
	if p.HintDirective != want {
		t.Errorf("directive = %q, want %q",
			p.HintDirective, want)
	}
}

// No usable index, no index hint (this test used to expect IndexScan(u)).
func TestPrescribe_SeqScanNoIndex(t *testing.T) {
	s := PlanSymptom{
		Kind:  SymptomSeqScanWithIndex,
		Alias: "u",
	}
	if p := Prescribe(s, defaultCfg); p != nil {
		t.Fatalf("index hint without an index: %+v", p)
	}
}

func TestPrescribe_ParallelDisabled(t *testing.T) {
	s := PlanSymptom{Kind: SymptomParallelDisabled}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	want := "max_parallel_workers_per_gather"
	if !strings.Contains(p.HintDirective, want) {
		t.Errorf("directive = %q, want %q",
			p.HintDirective, want)
	}
}

func TestPrescribe_UnknownSymptom(t *testing.T) {
	s := PlanSymptom{Kind: "unknown_thing"}
	p := Prescribe(s, defaultCfg)
	if p != nil {
		t.Errorf("expected nil, got %+v", p)
	}
}

func TestCalcWorkMem_Basic(t *testing.T) {
	tests := []struct {
		name  string
		kb    int64
		maxMB int
		want  int
	}{
		{"normal", 65536, 512, 128},
		{"zero_space", 0, 512, 64},
		{"small_space", 100, 512, 64},
		{"at_cap", 262144, 512, 512},
		{"over_cap", 524288, 512, 512},
		{"no_cap", 524288, 0, 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalcWorkMem(tc.kb, tc.maxMB)
			if got != tc.want {
				t.Errorf("CalcWorkMem(%d, %d) = %d, want %d",
					tc.kb, tc.maxMB, got, tc.want)
			}
		})
	}
}

func TestCalcWorkMemHash_Basic(t *testing.T) {
	tests := []struct {
		name    string
		peakKB  int64
		batches int64
		maxMB   int
		want    int
	}{
		{"normal", 8192, 16, 512, 256},
		{"zero", 0, 0, 512, 64},
		{"over_cap", 8192, 64, 512, 512},
		{"small", 512, 2, 512, 64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalcWorkMemHash(
				tc.peakKB, tc.batches, tc.maxMB,
			)
			if got != tc.want {
				t.Errorf(
					"CalcWorkMemHash(%d,%d,%d) = %d, want %d",
					tc.peakKB, tc.batches, tc.maxMB,
					got, tc.want,
				)
			}
		})
	}
}

func TestCombineHints_Single(t *testing.T) {
	ps := []Prescription{{
		Symptom:       SymptomBadNestedLoop,
		HintDirective: "HashJoin(orders)",
	}}
	got := CombineHints(ps)
	if got != "HashJoin(orders)" {
		t.Errorf("got %q, want HashJoin(orders)", got)
	}
}

func TestCombineHints_Multiple(t *testing.T) {
	ps := []Prescription{
		{HintDirective: "HashJoin(o)"},
		{HintDirective: "IndexScan(u users_pkey)"},
	}
	got := CombineHints(ps)
	want := "HashJoin(o) IndexScan(u users_pkey)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCombineHints_WorkMemDedup(t *testing.T) {
	ps := []Prescription{
		{
			Symptom:       SymptomDiskSort,
			HintDirective: `Set(work_mem "128MB")`,
		},
		{
			Symptom:       SymptomHashSpill,
			HintDirective: `Set(work_mem "256MB")`,
		},
		{
			Symptom:       SymptomBadNestedLoop,
			HintDirective: "HashJoin(o)",
		},
	}
	got := CombineHints(ps)
	if !strings.Contains(got, "256MB") {
		t.Errorf("expected 256MB (max), got %q", got)
	}
	if strings.Contains(got, "128MB") {
		t.Errorf("should not contain 128MB, got %q", got)
	}
	if !strings.Contains(got, "HashJoin(o)") {
		t.Errorf("missing HashJoin(o), got %q", got)
	}
	// Should have exactly one Set(work_mem ...)
	cnt := strings.Count(got, "Set(work_mem")
	if cnt != 1 {
		t.Errorf("work_mem count = %d, want 1", cnt)
	}
}

func TestCombineHints_Empty(t *testing.T) {
	got := CombineHints(nil)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCombineHints_GUCOnly(t *testing.T) {
	ps := []Prescription{
		{HintDirective: `Set(work_mem "64MB")`},
		{HintDirective: `Set(plan_cache_mode "force_generic_plan")`},
	}
	got := CombineHints(ps)
	if !strings.Contains(got, "work_mem") {
		t.Errorf("missing work_mem, got %q", got)
	}
	if !strings.Contains(got, "plan_cache_mode") {
		t.Errorf("missing plan_cache_mode, got %q", got)
	}
}

// market-10b: hashable_join false withholds the join hint; true keeps it.
// A symptom without the key (built by hand, not by the scanner, which
// always records it) keeps the earlier behaviour.
func TestPrescribe_BadNestedLoop_HashableJoin(t *testing.T) {
	cases := []struct {
		name   string
		detail map[string]any
		want   string
	}{
		{"not hashable", map[string]any{"hashable_join": false}, ""},
		{"hashable", map[string]any{"hashable_join": true}, "HashJoin(o c)"},
		{"not recorded", nil, "HashJoin(o c)"},
		{"wrong type", map[string]any{"hashable_join": "false"}, "HashJoin(o c)"},
	}
	for _, c := range cases {
		s := PlanSymptom{Kind: SymptomBadNestedLoop, JoinAliases: []string{"o", "c"},
			Detail: c.detail}
		p := Prescribe(s, defaultCfg)
		got := ""
		if p != nil {
			got = p.HintDirective
		}
		if got != c.want || (c.want == "") != (p == nil) {
			t.Errorf("%s: prescription %+v, want directive %q", c.name, p, c.want)
		}
	}
}

// State transition: a query whose hashable nested loop earned HashJoin(a b)
// loses the prescription on the next scan once its plan joins only by OR;
// the finding keeps the symptoms and the OR advice and says why no join
// hint is offered.
func TestTuner_NonHashablePlanWithdrawsJoinHint(t *testing.T) {
	tu := New(nil, TunerConfig{}, &HintPlanAvailability{}, noopLogFn)
	c := candidate{QueryID: 77, Query: "SELECT 1 FROM ta a JOIN tb b ON ...", Calls: 9}
	hashable := misNestedLoop("(a.id = b.a_id)", innerScan("Seq Scan", nil))
	tu.plans = map[int64]planfresh.Plan{77: {JSON: planOf(t, hashable),
		HasActuals: true, Source: planfresh.SourceCache}}
	first := tu.processCandidate(context.Background(), c)
	if len(first) != 1 || first[0].Detail["hint_directive"] != "HashJoin(a b)" ||
		first[0].Detail["join_hint_note"] != nil {
		t.Fatalf("first scan findings = %+v, want one HashJoin(a b)", first)
	}
	pureOR := misNestedLoop(orJoinFilter, innerScan("Seq Scan", nil))
	pureOR["Rows Removed by Join Filter"] = 50_000.0
	tu.plans[77] = planfresh.Plan{JSON: planOf(t, pureOR), HasActuals: true,
		Source: planfresh.SourceCache}
	second := tu.processCandidate(context.Background(), c)
	if len(second) != 1 {
		t.Fatalf("second scan findings = %d, want 1: %+v", len(second), second)
	}
	f := second[0]
	if f.Detail["hint_directive"] != "" || f.RecommendedSQL != "" ||
		strings.Contains(f.Recommendation, "HashJoin") {
		t.Fatalf("join hint not withdrawn: %+v", f)
	}
	syms, _ := f.Detail["symptoms"].([]string)
	if strings.Join(syms, ",") != "bad_nested_loop,or_join_filter" ||
		f.Detail["plan_antipatterns"] == nil ||
		!strings.Contains(f.Recommendation, "rewriting as UNION") {
		t.Fatalf("finding lost its symptoms or OR advice: %+v", f)
	}
	note, _ := f.Detail["join_hint_note"].(string)
	for _, frag := range []string{"no join hint", "a and b", "no equality",
		"cannot hash-join"} {
		if !strings.Contains(note, frag) || !strings.Contains(f.Recommendation, frag) {
			t.Errorf("note %q / recommendation %q lack %q", note, f.Recommendation, frag)
		}
	}
}
