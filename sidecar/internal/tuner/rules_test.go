package tuner

import (
	"strings"
	"testing"
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

func TestPrescribe_HighPlanTime(t *testing.T) {
	s := PlanSymptom{Kind: SymptomHighPlanTime}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	want := "force_generic_plan"
	if !strings.Contains(p.HintDirective, want) {
		t.Errorf("directive = %q, want %q",
			p.HintDirective, want)
	}
}

func TestPrescribe_BadNestedLoop(t *testing.T) {
	s := PlanSymptom{
		Kind:  SymptomBadNestedLoop,
		Alias: "orders",
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.HintDirective != "HashJoin(orders)" {
		t.Errorf("directive = %q, want HashJoin(orders)",
			p.HintDirective)
	}
}

func TestPrescribe_BadNestedLoop_NoAlias(t *testing.T) {
	s := PlanSymptom{
		Kind:         SymptomBadNestedLoop,
		RelationName: "items",
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.HintDirective != "HashJoin(items)" {
		t.Errorf("directive = %q, want HashJoin(items)",
			p.HintDirective)
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

func TestPrescribe_SeqScanNoIndex(t *testing.T) {
	s := PlanSymptom{
		Kind:  SymptomSeqScanWithIndex,
		Alias: "u",
	}
	p := Prescribe(s, defaultCfg)
	if p == nil {
		t.Fatal("expected prescription, got nil")
	}
	if p.HintDirective != "IndexScan(u)" {
		t.Errorf("directive = %q, want IndexScan(u)",
			p.HintDirective)
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
