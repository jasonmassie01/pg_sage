package analyzer

import (
	"testing"
)

func TestRuleSortWithoutIndex_Fires(t *testing.T) {
	// Sort processes 100000 rows, Limit returns 10.
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Limit",
			"Plan Rows": 10,
			"Plans": [{
				"Node Type": "Sort",
				"Plan Rows": 100000,
				"Plans": [{
					"Node Type": "Seq Scan",
					"Plan Rows": 100000
				}]
			}]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   42,
		QueryText: "SELECT * FROM orders ORDER BY created_at LIMIT 10",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "sort_without_index" {
		t.Errorf("category = %q, want sort_without_index", f.Category)
	}
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want warning", f.Severity)
	}
	sortRows, ok := f.Detail["sort_rows"].(int64)
	if !ok || sortRows != 100000 {
		t.Errorf("sort_rows = %v, want 100000", f.Detail["sort_rows"])
	}
	limitRows, ok := f.Detail["limit_rows"].(int64)
	if !ok || limitRows != 10 {
		t.Errorf("limit_rows = %v, want 10", f.Detail["limit_rows"])
	}
}

func TestRuleSortWithoutIndex_NoSort(t *testing.T) {
	// Plan with Index Scan + Limit, no Sort node.
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Limit",
			"Plan Rows": 10,
			"Plans": [{
				"Node Type": "Index Scan",
				"Plan Rows": 10
			}]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   99,
		QueryText: "SELECT * FROM orders ORDER BY id LIMIT 10",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestRuleSortWithoutIndex_LowRatio(t *testing.T) {
	// Sort processes 50 rows for LIMIT 10 → ratio 5x, below 10x.
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Limit",
			"Plan Rows": 10,
			"Plans": [{
				"Node Type": "Sort",
				"Plan Rows": 50,
				"Plans": [{
					"Node Type": "Seq Scan",
					"Plan Rows": 50
				}]
			}]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   77,
		QueryText: "SELECT * FROM small_table ORDER BY x LIMIT 10",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (ratio < 10x), got %d", len(findings))
	}
}

func TestRuleSortWithoutIndex_NoLimit(t *testing.T) {
	// Sort without a Limit parent — no finding.
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Sort",
			"Plan Rows": 100000,
			"Plans": [{
				"Node Type": "Seq Scan",
				"Plan Rows": 100000
			}]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   55,
		QueryText: "SELECT * FROM orders ORDER BY created_at",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (no Limit), got %d", len(findings))
	}
}

func TestRuleSortWithoutIndex_InvalidJSON(t *testing.T) {
	entries := []ExplainEntry{{
		QueryID:   1,
		QueryText: "SELECT 1",
		PlanJSON:  []byte(`not valid json`),
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for invalid JSON, got %d", len(findings))
	}
}

func TestRuleSortWithoutIndex_NestedLimitSort(t *testing.T) {
	// Limit+Sort nested under a Gather node.
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Gather",
			"Plan Rows": 10,
			"Plans": [{
				"Node Type": "Limit",
				"Plan Rows": 10,
				"Plans": [{
					"Node Type": "Sort",
					"Plan Rows": 500000,
					"Plans": [{
						"Node Type": "Seq Scan",
						"Plan Rows": 500000
					}]
				}]
			}]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   88,
		QueryText: "SELECT * FROM big ORDER BY ts LIMIT 10",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for nested plan, got %d", len(findings))
	}
}

// TestRuleSortWithoutIndex_SubPlanNestedLimitSort verifies that
// findSortLimit walks into a SubPlan (Nested Loop) and detects a
// Limit+Sort pattern inside a correlated EXISTS subquery.
func TestRuleSortWithoutIndex_SubPlanNestedLimitSort(t *testing.T) {
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Nested Loop",
			"Plan Rows": 200,
			"Plans": [
				{
					"Node Type": "Seq Scan",
					"Plan Rows": 200
				},
				{
					"Node Type": "Limit",
					"Plan Rows": 5,
					"Plans": [{
						"Node Type": "Sort",
						"Plan Rows": 80000,
						"Plans": [{
							"Node Type": "Seq Scan",
							"Plan Rows": 80000
						}]
					}]
				}
			]
		}
	}]`)

	entries := []ExplainEntry{{
		QueryID:   201,
		QueryText: "SELECT c.* FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cid = c.id ORDER BY o.ts LIMIT 5)",
		PlanJSON:  planJSON,
	}}

	findings := ruleSortWithoutIndex(entries)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "sort_without_index" {
		t.Errorf("category = %q, want sort_without_index",
			f.Category)
	}
	sr, _ := f.Detail["sort_rows"].(int64)
	if sr != 80000 {
		t.Errorf("sort_rows = %d, want 80000", sr)
	}
	lr, _ := f.Detail["limit_rows"].(int64)
	if lr != 5 {
		t.Errorf("limit_rows = %d, want 5", lr)
	}
}

func TestRuleSortWithoutIndex_EmptyEntries(t *testing.T) {
	findings := ruleSortWithoutIndex(nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil entries, got %d", len(findings))
	}
}
