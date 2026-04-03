//go:build e2e

// Package e2e — tests that every tuner symptom kind produces the
// correct hint directive. Each subtest feeds an EXPLAIN plan that
// triggers exactly one symptom, then verifies ScanPlan detects it
// and Prescribe emits the expected hint.
//
// Run with:
//
//	go test -tags=e2e -count=1 -timeout 60s ./e2e/ \
//	    -run TestTunerAllHints
package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/tuner"
)

// defaultCfg provides sane defaults for prescription tests.
func defaultCfg() tuner.TunerConfig {
	return tuner.TunerConfig{
		Enabled:                true,
		WorkMemMaxMB:           1024,
		PlanTimeRatio:          2.0,
		NestedLoopRowThreshold: 10,
		ParallelMinTableRows:   100_000,
	}
}

// TestTunerAllHints_DiskSort verifies that a Sort node with
// SortSpaceType="Disk" triggers SymptomDiskSort and produces
// a Set(work_mem "NMB") hint.
func TestTunerAllHints_DiskSort(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type":       "Sort",
			"Sort Method":     "external merge",
			"Sort Space Used": 32768,
			"Sort Space Type": "Disk",
			"Plan Rows":       500000,
			"Plans":           []any{},
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomDiskSort)
	if found == nil {
		t.Fatalf(
			"expected SymptomDiskSort, got symptoms: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil for disk_sort")
	}

	if !strings.HasPrefix(
		rx.HintDirective, `Set(work_mem "`,
	) {
		t.Errorf(
			"expected Set(work_mem ...), got: %s",
			rx.HintDirective,
		)
	}
	if !strings.HasSuffix(rx.HintDirective, `MB")`) {
		t.Errorf(
			"hint should end with MB\"), got: %s",
			rx.HintDirective,
		)
	}
	t.Logf("DiskSort hint: %s (%s)", rx.HintDirective, rx.Rationale)
}

// TestTunerAllHints_HashSpill verifies that a Hash Join node
// with HashBatches > 1 triggers SymptomHashSpill and produces
// a Set(work_mem) hint.
func TestTunerAllHints_HashSpill(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type":       "Hash Join",
			"Join Type":       "Inner",
			"Hash Batches":    8,
			"Peak Memory Usage": 16384,
			"Plan Rows":       100000,
			"Plans": []any{
				map[string]any{
					"Node Type":      "Seq Scan",
					"Relation Name":  "orders",
					"Alias":          "o",
					"Plan Rows":      100000,
					"Workers Planned": 2,
				},
				map[string]any{
					"Node Type":      "Hash",
					"Plan Rows":      50000,
					"Plans": []any{
						map[string]any{
							"Node Type":      "Seq Scan",
							"Relation Name":  "customers",
							"Alias":          "c",
							"Plan Rows":      50000,
							"Workers Planned": 2,
						},
					},
				},
			},
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomHashSpill)
	if found == nil {
		t.Fatalf(
			"expected SymptomHashSpill, got: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil for hash_spill")
	}

	if !strings.Contains(rx.HintDirective, "work_mem") {
		t.Errorf(
			"expected work_mem hint, got: %s",
			rx.HintDirective,
		)
	}
	t.Logf("HashSpill hint: %s (%s)", rx.HintDirective, rx.Rationale)
}

// TestTunerAllHints_BadNestedLoop verifies that a Nested Loop
// with actual rows > 10x plan rows triggers SymptomBadNestedLoop
// and produces a HashJoin(alias) hint.
func TestTunerAllHints_BadNestedLoop(t *testing.T) {
	actualRows := int64(500000)
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type":   "Nested Loop",
			"Join Type":   "Inner",
			"Plan Rows":   100,
			"Actual Rows": actualRows,
			"Actual Loops": int64(1),
			"Alias":       "o",
			"Plans": []any{
				map[string]any{
					"Node Type":      "Index Scan",
					"Relation Name":  "orders",
					"Alias":          "o",
					"Plan Rows":      100,
					"Workers Planned": 1,
				},
				map[string]any{
					"Node Type":      "Index Scan",
					"Relation Name":  "line_items",
					"Alias":          "li",
					"Plan Rows":      1,
					"Workers Planned": 1,
				},
			},
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomBadNestedLoop)
	if found == nil {
		t.Fatalf(
			"expected SymptomBadNestedLoop, got: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil for bad_nested_loop")
	}

	if !strings.HasPrefix(rx.HintDirective, "HashJoin(") {
		t.Errorf(
			"expected HashJoin(...), got: %s",
			rx.HintDirective,
		)
	}
	t.Logf(
		"BadNestedLoop hint: %s (%s)",
		rx.HintDirective, rx.Rationale,
	)
}

// TestTunerAllHints_SeqScanWithIndex verifies that a Seq Scan
// on a named relation triggers SymptomSeqScanWithIndex and
// produces an IndexScan(alias) hint.
func TestTunerAllHints_SeqScanWithIndex(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type":      "Seq Scan",
			"Relation Name":  "orders",
			"Schema":         "public",
			"Alias":          "o",
			"Plan Rows":      500000,
			"Filter":         "(status = 'active'::text)",
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomSeqScanWithIndex)
	if found == nil {
		t.Fatalf(
			"expected SymptomSeqScanWithIndex, got: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal(
			"Prescribe returned nil for seq_scan_with_index",
		)
	}

	if !strings.HasPrefix(rx.HintDirective, "IndexScan(") {
		t.Errorf(
			"expected IndexScan(...), got: %s",
			rx.HintDirective,
		)
	}
	// Should use alias "o", not relation name.
	if !strings.Contains(rx.HintDirective, "o") {
		t.Errorf(
			"hint should reference alias 'o', got: %s",
			rx.HintDirective,
		)
	}
	t.Logf(
		"SeqScan hint: %s (%s)",
		rx.HintDirective, rx.Rationale,
	)
}

// TestTunerAllHints_SeqScanWithIndexName verifies that when an
// index name is available in the symptom, the hint includes it:
// IndexScan(alias indexname).
func TestTunerAllHints_SeqScanWithIndexName(t *testing.T) {
	symptom := tuner.PlanSymptom{
		Kind:         tuner.SymptomSeqScanWithIndex,
		NodeType:     "Seq Scan",
		RelationName: "orders",
		Alias:        "o",
		IndexName:    "idx_orders_status",
	}

	rx := tuner.Prescribe(symptom, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil")
	}

	expected := "IndexScan(o idx_orders_status)"
	if rx.HintDirective != expected {
		t.Errorf(
			"expected %q, got %q", expected, rx.HintDirective,
		)
	}
	t.Logf("SeqScan+index hint: %s", rx.HintDirective)
}

// TestTunerAllHints_HighPlanTime verifies that high planning
// time triggers SymptomHighPlanTime and produces
// Set(plan_cache_mode "force_generic_plan").
//
// Note: HighPlanTime is detected in tuner.go's isHighPlanTime()
// using pg_stat_statements data, not from the EXPLAIN plan. We
// test the prescription path directly.
func TestTunerAllHints_HighPlanTime(t *testing.T) {
	symptom := tuner.PlanSymptom{
		Kind:     tuner.SymptomHighPlanTime,
		NodeType: "Result",
	}

	rx := tuner.Prescribe(symptom, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil for high_plan_time")
	}

	expected := `Set(plan_cache_mode "force_generic_plan")`
	if rx.HintDirective != expected {
		t.Errorf(
			"expected %q, got %q", expected, rx.HintDirective,
		)
	}
	t.Logf(
		"HighPlanTime hint: %s (%s)",
		rx.HintDirective, rx.Rationale,
	)
}

// TestTunerAllHints_ParallelDisabled verifies that a Scan node
// without WorkersPlanned triggers SymptomParallelDisabled and
// produces Set(max_parallel_workers_per_gather "4").
func TestTunerAllHints_ParallelDisabled(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type":     "Seq Scan",
			"Relation Name": "large_events",
			"Schema":        "public",
			"Alias":         "e",
			"Plan Rows":     5000000,
			// No "Workers Planned" key — parallel disabled.
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomParallelDisabled)
	if found == nil {
		t.Fatalf(
			"expected SymptomParallelDisabled, got: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal(
			"Prescribe returned nil for parallel_disabled",
		)
	}

	expected := `Set(max_parallel_workers_per_gather "4")`
	if rx.HintDirective != expected {
		t.Errorf(
			"expected %q, got %q", expected, rx.HintDirective,
		)
	}
	t.Logf(
		"ParallelDisabled hint: %s (%s)",
		rx.HintDirective, rx.Rationale,
	)
}

// TestTunerAllHints_SortLimit verifies that a Sort node under
// a Limit parent where sort rows >> limit rows triggers
// SymptomSortLimit with a rationale (no hint directive).
func TestTunerAllHints_SortLimit(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type": "Limit",
			"Plan Rows": 10,
			"Plans": []any{
				map[string]any{
					"Node Type":       "Sort",
					"Sort Method":     "top-N heapsort",
					"Sort Space Used": 64,
					"Sort Space Type": "Memory",
					"Plan Rows":       500000,
					"Plans": []any{
						map[string]any{
							"Node Type":      "Seq Scan",
							"Relation Name":  "orders",
							"Alias":          "o",
							"Plan Rows":      500000,
						},
					},
				},
			},
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	found := findSymptom(symptoms, tuner.SymptomSortLimit)
	if found == nil {
		t.Fatalf(
			"expected SymptomSortLimit, got: %v",
			symptomKinds(symptoms),
		)
	}

	rx := tuner.Prescribe(*found, defaultCfg())
	if rx == nil {
		t.Fatal("Prescribe returned nil for sort_limit")
	}

	// SortLimit produces rationale only, no hint directive.
	if rx.HintDirective != "" {
		t.Errorf(
			"sort_limit should have empty directive, got: %s",
			rx.HintDirective,
		)
	}
	if rx.Rationale == "" {
		t.Error("sort_limit should have non-empty rationale")
	}
	if !strings.Contains(rx.Rationale, "500000") {
		t.Errorf(
			"rationale should mention sort row count, got: %s",
			rx.Rationale,
		)
	}
	t.Logf("SortLimit rationale: %s", rx.Rationale)
}

// TestTunerAllHints_CombineWorkMem verifies that when both
// DiskSort and HashSpill are present, CombineHints picks the
// larger work_mem value.
func TestTunerAllHints_CombineWorkMem(t *testing.T) {
	prescriptions := []tuner.Prescription{
		{
			Symptom:       tuner.SymptomDiskSort,
			HintDirective: `Set(work_mem "128MB")`,
			Rationale:     "sort spilled 65536 KB",
		},
		{
			Symptom:       tuner.SymptomHashSpill,
			HintDirective: `Set(work_mem "256MB")`,
			Rationale:     "hash used 4 batches",
		},
		{
			Symptom:       tuner.SymptomSeqScanWithIndex,
			HintDirective: "IndexScan(o)",
			Rationale:     "seq scan on indexed relation",
		},
	}

	combined := tuner.CombineHints(prescriptions)
	t.Logf("Combined hint: %s", combined)

	// Should keep the larger work_mem (256MB).
	if !strings.Contains(combined, `Set(work_mem "256MB")`) {
		t.Errorf(
			"expected larger work_mem (256MB), got: %s",
			combined,
		)
	}

	// Should NOT contain 128MB (deduplicated).
	if strings.Contains(combined, "128MB") {
		t.Errorf(
			"should not contain smaller work_mem: %s",
			combined,
		)
	}

	// Should also contain the IndexScan hint.
	if !strings.Contains(combined, "IndexScan(o)") {
		t.Errorf(
			"should contain IndexScan hint: %s", combined,
		)
	}
}

// TestTunerAllHints_UnknownSymptom verifies that an unknown
// symptom kind returns nil from Prescribe.
func TestTunerAllHints_UnknownSymptom(t *testing.T) {
	symptom := tuner.PlanSymptom{
		Kind: tuner.SymptomKind("nonexistent_symptom"),
	}

	rx := tuner.Prescribe(symptom, defaultCfg())
	if rx != nil {
		t.Errorf(
			"expected nil for unknown symptom, got: %+v", rx,
		)
	}
}

// TestTunerAllHints_MultipleSymptoms verifies that a complex
// plan with multiple symptoms detects all of them.
func TestTunerAllHints_MultipleSymptoms(t *testing.T) {
	plan := mustJSON(t, map[string]any{
		"Plan": map[string]any{
			"Node Type": "Limit",
			"Plan Rows": 5,
			"Plans": []any{
				map[string]any{
					"Node Type":       "Sort",
					"Sort Method":     "external merge",
					"Sort Space Used": 16384,
					"Sort Space Type": "Disk",
					"Plan Rows":       200000,
					"Plans": []any{
						map[string]any{
							"Node Type":      "Hash Join",
							"Join Type":      "Inner",
							"Hash Batches":   4,
							"Peak Memory Usage": 8192,
							"Plan Rows":      200000,
							"Plans": []any{
								map[string]any{
									"Node Type": "Seq Scan",
									"Relation Name": "orders",
									"Alias":     "o",
									"Plan Rows": 200000,
								},
								map[string]any{
									"Node Type": "Hash",
									"Plan Rows": 50000,
									"Plans": []any{
										map[string]any{
											"Node Type": "Seq Scan",
											"Relation Name": "customers",
											"Alias":     "c",
											"Plan Rows": 50000,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})

	symptoms, err := tuner.ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan failed: %v", err)
	}

	t.Logf("Detected %d symptoms: %v",
		len(symptoms), symptomKinds(symptoms),
	)

	// This plan should trigger at least:
	// - DiskSort (Sort with Disk spill)
	// - HashSpill (Hash Join with batches > 1)
	// - SeqScan (2x Seq Scan on named relations)
	// - SortLimit (Sort under Limit, 200000 >> 5)
	// - ParallelDisabled (Seq Scan without workers)
	expectedKinds := []tuner.SymptomKind{
		tuner.SymptomDiskSort,
		tuner.SymptomHashSpill,
		tuner.SymptomSeqScanWithIndex,
		tuner.SymptomSortLimit,
		tuner.SymptomParallelDisabled,
	}

	for _, kind := range expectedKinds {
		if findSymptom(symptoms, kind) == nil {
			t.Errorf("missing expected symptom: %s", kind)
		}
	}

	// Now prescribe all and combine.
	cfg := defaultCfg()
	var prescriptions []tuner.Prescription
	for _, s := range symptoms {
		if rx := tuner.Prescribe(s, cfg); rx != nil {
			prescriptions = append(prescriptions, *rx)
		}
	}

	combined := tuner.CombineHints(prescriptions)
	t.Logf("Combined hint string: %s", combined)

	if combined == "" {
		t.Error("combined hint should not be empty")
	}
}

// --- helpers ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}

func findSymptom(
	symptoms []tuner.PlanSymptom, kind tuner.SymptomKind,
) *tuner.PlanSymptom {
	for i := range symptoms {
		if symptoms[i].Kind == kind {
			return &symptoms[i]
		}
	}
	return nil
}

func symptomKinds(symptoms []tuner.PlanSymptom) []string {
	var kinds []string
	for _, s := range symptoms {
		kinds = append(kinds, string(s.Kind))
	}
	return kinds
}
