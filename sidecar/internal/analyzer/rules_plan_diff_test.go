package analyzer

import (
	"encoding/json"
	"testing"
)

// Helper to build EXPLAIN JSON from a plan node tree.
func makePlanJSON(t *testing.T, plan map[string]any) []byte {
	t.Helper()
	wrapper := []map[string]any{{"Plan": plan}}
	b, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal plan JSON: %v", err)
	}
	return b
}

func seqScanPlan(t *testing.T) []byte {
	return makePlanJSON(t, map[string]any{
		"Node Type":     "Seq Scan",
		"Relation Name": "orders",
		"Plan Rows":     10000,
	})
}

func indexScanPlan(t *testing.T) []byte {
	return makePlanJSON(t, map[string]any{
		"Node Type":     "Index Scan",
		"Relation Name": "orders",
		"Plan Rows":     100,
	})
}

func diskSortPlan(t *testing.T) []byte {
	return makePlanJSON(t, map[string]any{
		"Node Type": "Limit",
		"Plan Rows": 10,
		"Plans": []map[string]any{
			{
				"Node Type":       "Sort",
				"Sort Space Type": "Disk",
				"Plan Rows":       50000,
				"Plans": []map[string]any{
					{
						"Node Type":     "Seq Scan",
						"Relation Name": "orders",
						"Plan Rows":     50000,
					},
				},
			},
		},
	})
}

func memorySortPlan(t *testing.T) []byte {
	return makePlanJSON(t, map[string]any{
		"Node Type": "Limit",
		"Plan Rows": 10,
		"Plans": []map[string]any{
			{
				"Node Type":       "Sort",
				"Sort Space Type": "Memory",
				"Plan Rows":       500,
				"Plans": []map[string]any{
					{
						"Node Type":     "Index Scan",
						"Relation Name": "orders",
						"Plan Rows":     500,
					},
				},
			},
		},
	})
}

func nestedPlan(t *testing.T) []byte {
	return makePlanJSON(t, map[string]any{
		"Node Type": "Limit",
		"Plan Rows": 10,
		"Plans": []map[string]any{
			{
				"Node Type": "Sort",
				"Plan Rows": 1000,
				"Plans": []map[string]any{
					{
						"Node Type": "Hash Join",
						"Plan Rows": 1000,
						"Plans": []map[string]any{
							{
								"Node Type":     "Seq Scan",
								"Relation Name": "orders",
								"Plan Rows":     1000,
							},
							{
								"Node Type":     "Index Scan",
								"Relation Name": "users",
								"Plan Rows":     50,
							},
						},
					},
				},
			},
		},
	})
}

func TestRulePlanRegression_CostDoubled(t *testing.T) {
	plan := seqScanPlan(t)
	pairs := []planPair{{
		QueryID:      42,
		QueryText:    "SELECT * FROM orders",
		CurrentPlan:  plan,
		CurrentCost:  250,
		CurrentTime:  50,
		PreviousPlan: plan,
		PreviousCost: 100,
		PreviousTime: 20,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("expected warning, got %s", findings[0].Severity)
	}
	if findings[0].Category != "plan_regression" {
		t.Errorf("expected plan_regression, got %s", findings[0].Category)
	}
}

func TestRulePlanRegression_CostTenX(t *testing.T) {
	plan := seqScanPlan(t)
	pairs := []planPair{{
		QueryID:      99,
		QueryText:    "SELECT * FROM big_table",
		CurrentPlan:  plan,
		CurrentCost:  1200,
		CurrentTime:  100,
		PreviousPlan: plan,
		PreviousCost: 100,
		PreviousTime: 10,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("expected critical, got %s", findings[0].Severity)
	}
}

func TestRulePlanRegression_IndexToSeqScan(t *testing.T) {
	pairs := []planPair{{
		QueryID:      55,
		QueryText:    "SELECT * FROM orders WHERE id = $1",
		CurrentPlan:  seqScanPlan(t),
		CurrentCost:  160,
		CurrentTime:  30,
		PreviousPlan: indexScanPlan(t),
		PreviousCost: 100,
		PreviousTime: 10,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Severity != "warning" {
		t.Errorf("expected warning, got %s", f.Severity)
	}
	detail := f.Detail
	changes, ok := detail["node_changes"].([]string)
	if !ok || len(changes) == 0 {
		t.Errorf("expected node_changes, got %v", detail["node_changes"])
	}
}

func TestRulePlanRegression_NewDiskSpill(t *testing.T) {
	pairs := []planPair{{
		QueryID:      77,
		QueryText:    "SELECT * FROM orders ORDER BY created_at",
		CurrentPlan:  diskSortPlan(t),
		CurrentCost:  160,
		CurrentTime:  40,
		PreviousPlan: memorySortPlan(t),
		PreviousCost: 100,
		PreviousTime: 10,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	spills, ok := f.Detail["new_disk_spills"].(bool)
	if !ok || !spills {
		t.Errorf("expected new_disk_spills=true, got %v", f.Detail["new_disk_spills"])
	}
}

func TestRulePlanRegression_Improvement(t *testing.T) {
	pairs := []planPair{{
		QueryID:      10,
		QueryText:    "SELECT 1",
		CurrentPlan:  indexScanPlan(t),
		CurrentCost:  50,
		CurrentTime:  5,
		PreviousPlan: seqScanPlan(t),
		PreviousCost: 200,
		PreviousTime: 40,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for improvement, got %d", len(findings))
	}
}

func TestRulePlanRegression_MinorChange(t *testing.T) {
	plan := seqScanPlan(t)
	pairs := []planPair{{
		QueryID:      20,
		QueryText:    "SELECT count(*) FROM orders",
		CurrentPlan:  plan,
		CurrentCost:  130,
		CurrentTime:  20,
		PreviousPlan: plan,
		PreviousCost: 100,
		PreviousTime: 15,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for minor change, got %d", len(findings))
	}
}

func TestRulePlanRegression_TrivialQuery(t *testing.T) {
	plan := seqScanPlan(t)
	pairs := []planPair{{
		QueryID:      1,
		QueryText:    "SELECT 1",
		CurrentPlan:  plan,
		CurrentCost:  0.5,
		CurrentTime:  0.1,
		PreviousPlan: plan,
		PreviousCost: 0.1,
		PreviousTime: 0.01,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for trivial query, got %d", len(findings))
	}
}

func TestRulePlanRegression_EmptyPairs(t *testing.T) {
	findings := rulePlanRegression(nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil, got %d", len(findings))
	}
	findings = rulePlanRegression([]planPair{})
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty, got %d", len(findings))
	}
}

func TestRulePlanRegression_InvalidJSON(t *testing.T) {
	pairs := []planPair{{
		QueryID:      5,
		QueryText:    "SELECT 1",
		CurrentPlan:  []byte(`{broken`),
		CurrentCost:  500,
		CurrentTime:  50,
		PreviousPlan: []byte(`{broken`),
		PreviousCost: 100,
		PreviousTime: 10,
	}}
	findings := rulePlanRegression(pairs)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for invalid JSON, got %d", len(findings))
	}
}

func TestCollectNodeTypes(t *testing.T) {
	plan := nestedPlan(t)
	entries := collectNodeTypes(plan)
	if len(entries) == 0 {
		t.Fatal("expected entries, got none")
	}
	expected := []nodeEntry{
		{0, "Limit"},
		{1, "Sort"},
		{2, "Hash Join"},
		{3, "Seq Scan"},
		{3, "Index Scan"},
	}
	if len(entries) != len(expected) {
		t.Fatalf("expected %d entries, got %d: %v",
			len(expected), len(entries), entries)
	}
	for i, e := range expected {
		if entries[i].Depth != e.Depth ||
			entries[i].NodeType != e.NodeType {
			t.Errorf("entry[%d]: expected %v, got %v",
				i, e, entries[i])
		}
	}
}

func TestDetectNodeChanges(t *testing.T) {
	prev := []nodeEntry{
		{0, "Limit"},
		{1, "Sort"},
		{2, "Index Scan"},
	}
	cur := []nodeEntry{
		{0, "Limit"},
		{1, "Sort"},
		{2, "Seq Scan"},
	}
	changes := detectNodeChanges(cur, prev)
	if len(changes) == 0 {
		t.Fatal("expected changes, got none")
	}
	found := false
	for _, c := range changes {
		if c == "Index Scan \u2192 Seq Scan" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Index Scan → Seq Scan', got %v", changes)
	}
}

func TestBuildPlanSummary(t *testing.T) {
	plan := diskSortPlan(t)
	summary := buildPlanSummary(plan)
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}
	// Should follow the leftmost path: Limit → Sort (Disk) → Seq Scan
	if summary != "Limit \u2192 Sort (Disk) \u2192 Seq Scan" {
		t.Errorf("unexpected summary: %s", summary)
	}
}

func TestHasDiskSpill(t *testing.T) {
	if !hasDiskSpill(diskSortPlan(t)) {
		t.Error("expected disk spill detected")
	}
	if hasDiskSpill(memorySortPlan(t)) {
		t.Error("expected no disk spill for memory sort")
	}
	if hasDiskSpill(indexScanPlan(t)) {
		t.Error("expected no disk spill for index scan")
	}
	if hasDiskSpill([]byte(`{broken`)) {
		t.Error("expected no disk spill for invalid JSON")
	}
}

func TestHasDiskSpill_HashBatches(t *testing.T) {
	plan := makePlanJSON(t, map[string]any{
		"Node Type":    "Hash Join",
		"Hash Batches": 4,
		"Plan Rows":    1000,
		"Plans": []map[string]any{
			{
				"Node Type":     "Seq Scan",
				"Relation Name": "orders",
				"Plan Rows":     1000,
			},
			{
				"Node Type":     "Hash",
				"Hash Batches":  4,
				"Plan Rows":     500,
				"Plans": []map[string]any{
					{
						"Node Type":     "Seq Scan",
						"Relation Name": "users",
						"Plan Rows":     500,
					},
				},
			},
		},
	})
	if !hasDiskSpill(plan) {
		t.Error("expected disk spill for hash batches > 1")
	}
}

func TestSeverityFromCostRatio(t *testing.T) {
	tests := []struct {
		ratio    float64
		expected string
	}{
		{15.0, "critical"},
		{10.0, "critical"},
		{5.0, "warning"},
		{2.0, "warning"},
		{1.5, "info"},
		{1.0, "info"},
		{0.5, "info"},
	}
	for _, tc := range tests {
		got := severityFromCostRatio(tc.ratio)
		if got != tc.expected {
			t.Errorf("ratio %.1f: expected %s, got %s",
				tc.ratio, tc.expected, got)
		}
	}
}
