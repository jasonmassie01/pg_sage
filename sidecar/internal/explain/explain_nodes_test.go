package explain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// tuning-10: node time is inclusive x loops, and the misestimate warning
// is symmetric (over- and underestimates), with 0 treated as 1.

func oneNode(t *testing.T, fields string) NodeExplain {
	t.Helper()
	nodes := extractNodes(json.RawMessage(`[{"Plan": {"Node Type": "Seq Scan",
		"Relation Name": "t", "Total Cost": 10.0` + fields + `}}]`))
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	return nodes[0]
}

func TestExtractNodes_MisestimateBoundary(t *testing.T) {
	cases := []struct {
		name     string
		est, act string
		warn     bool
	}{
		{"10x underestimate warns", "100", "1000", true},
		{"10x overestimate warns", "1000", "100", true},
		{"9x underestimate is quiet", "100", "900", false},
		{"9x overestimate is quiet", "900", "100", false},
		{"actual 0 vs estimate 50 warns", "50", "0", true},
		{"actual 0 vs estimate 9 is quiet", "9", "0", false},
		{"estimate 0 vs actual 10 warns", "0", "10", true},
		{"estimate 0 vs actual 9 is quiet", "0", "9", false},
		{"exact estimate is quiet", "100", "100", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := oneNode(t, fmt.Sprintf(`, "Plan Rows": %s, "Actual Rows": %s,
				"Actual Total Time": 1.0, "Actual Loops": 1`, c.est, c.act))
			if (n.Warning != "") != c.warn {
				t.Fatalf("Warning = %q, want warning=%v", n.Warning, c.warn)
			}
			if c.warn && !strings.Contains(n.Warning, "est "+c.est) {
				t.Errorf("Warning = %q, want it to name the estimate %s", n.Warning, c.est)
			}
		})
	}
}

func TestExtractNodes_OverestimateWarningSaysOver(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 5000, "Actual Rows": 10, "Actual Total Time": 1.0,
		"Actual Loops": 1`)
	if !strings.Contains(n.Warning, "500x") || !strings.Contains(n.Warning, "actual 10") {
		t.Errorf("Warning = %q, want 500x naming actual 10", n.Warning)
	}
}

func TestExtractNodes_TimeIsInclusiveTimesLoops(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 1, "Actual Rows": 1, "Actual Total Time": 0.2,
		"Actual Loops": 5000`)
	if n.TimeMs == nil || *n.TimeMs < 999.999 || *n.TimeMs > 1000.001 {
		t.Fatalf("TimeMs = %v, want 1000 ms (0.2 ms x 5000 loops)", n.TimeMs)
	}
	if n.Loops == nil || *n.Loops != 5000 {
		t.Errorf("Loops = %v, want 5000", n.Loops)
	}
}

// Rows stay per loop, as PostgreSQL prints them, so they compare with the
// per-loop estimate.
func TestExtractNodes_RowsStayPerLoop(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 3, "Actual Rows": 3, "Actual Total Time": 0.01,
		"Actual Loops": 400`)
	if n.Rows != 3 || n.Warning != "" {
		t.Errorf("Rows=%d Warning=%q, want 3 per loop and no warning", n.Rows, n.Warning)
	}
}

// Nil/empty: a node without actuals has no time, no loops and no warning.
func TestExtractNodes_NoActuals(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 50`)
	if n.TimeMs != nil || n.Loops != nil || n.Warning != "" {
		t.Errorf("TimeMs=%v Loops=%v Warning=%q, want all empty", n.TimeMs, n.Loops, n.Warning)
	}
}

// Boundary: a node that never ran (Actual Loops 0) reports 0 ms, not the
// per-loop time, and no misestimate (it produced nothing to compare).
func TestExtractNodes_NeverExecuted(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 500, "Actual Rows": 0, "Actual Total Time": 0.0,
		"Actual Loops": 0`)
	if n.TimeMs == nil || *n.TimeMs != 0 {
		t.Errorf("TimeMs = %v, want 0", n.TimeMs)
	}
	if n.Warning != "" {
		t.Errorf("Warning = %q, want none for a node that never executed", n.Warning)
	}
}

// A plan without "Actual Loops" (hand-written or older tooling) counts one loop.
func TestExtractNodes_MissingLoopsIsOne(t *testing.T) {
	n := oneNode(t, `, "Plan Rows": 1, "Actual Rows": 1, "Actual Total Time": 7.5`)
	if n.TimeMs == nil || *n.TimeMs != 7.5 {
		t.Errorf("TimeMs = %v, want 7.5", n.TimeMs)
	}
}

func TestBuildResult_ActualTimeIsExecutionTime(t *testing.T) {
	plan := json.RawMessage(`[{"Plan": {"Node Type": "Seq Scan", "Total Cost": 5,
		"Plan Rows": 1, "Actual Total Time": 8.3, "Actual Rows": 1, "Actual Loops": 1},
		"Planning Time": 0.4, "Execution Time": 8.72}]`)
	r := buildResult("SELECT 1", plan, true)
	if r.ActualTimeMs == nil || *r.ActualTimeMs != 8.72 {
		t.Errorf("ActualTimeMs = %v, want the top-level Execution Time 8.72", r.ActualTimeMs)
	}
}

// Nil/empty: without Execution Time the root node's time is used.
func TestBuildResult_ActualTimeFallsBackToRoot(t *testing.T) {
	plan := json.RawMessage(`[{"Plan": {"Node Type": "Seq Scan", "Total Cost": 5,
		"Plan Rows": 1, "Actual Total Time": 3.25, "Actual Rows": 1, "Actual Loops": 1}}]`)
	r := buildResult("SELECT 1", plan, true)
	if r.ActualTimeMs == nil || *r.ActualTimeMs != 3.25 {
		t.Errorf("ActualTimeMs = %v, want the root time 3.25", r.ActualTimeMs)
	}
}

// An auto_explain capture may be a bare {"Plan": ...} object.
func TestBuildResult_BareObjectPlan(t *testing.T) {
	plan := json.RawMessage(`{"Query Text": "SELECT 1", "Plan": {"Node Type": "Sort",
		"Total Cost": 12, "Plan Rows": 10, "Actual Total Time": 2, "Actual Rows": 10,
		"Actual Loops": 1}}`)
	r := buildResult("SELECT 1", plan, true)
	if len(r.NodeBreakdown) != 1 || r.NodeBreakdown[0].NodeType != "Sort" {
		t.Fatalf("NodeBreakdown = %+v, want the Sort node", r.NodeBreakdown)
	}
	if r.EstimatedCost != 12 || r.ActualTimeMs == nil || *r.ActualTimeMs != 2 {
		t.Errorf("cost=%v actual=%v, want 12 and 2", r.EstimatedCost, r.ActualTimeMs)
	}
}
