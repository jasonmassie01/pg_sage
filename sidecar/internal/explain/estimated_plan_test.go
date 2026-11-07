package explain

import (
	"encoding/json"
	"testing"
)

// EstimatedPlan (security-02) reads a plan-only EXPLAIN that pg_sage did
// not run itself (an agent's, from the broker) into the same breakdown.
// No concurrent access tests: it is a pure function of its arguments.

func TestEstimatedPlanReadsNodesAndCost(t *testing.T) {
	plan := json.RawMessage(`[{"Plan":{"Node Type":"Hash Join","Total Cost":42.5,` +
		`"Plan Rows":10,"Plans":[{"Node Type":"Seq Scan","Relation Name":"a",` +
		`"Total Cost":10,"Plan Rows":100}]}}]`)
	res := EstimatedPlan("SELECT 1", plan)
	if res.Query != "SELECT 1" || string(res.PlanJSON) != string(plan) {
		t.Errorf("query/plan = %q / %s", res.Query, res.PlanJSON)
	}
	if res.EstimatedCost != 42.5 {
		t.Errorf("estimated cost = %v, want 42.5", res.EstimatedCost)
	}
	if len(res.NodeBreakdown) != 2 || res.NodeBreakdown[1].Relation != "a" ||
		res.NodeBreakdown[1].RowEstimate != 100 {
		t.Errorf("nodes = %+v", res.NodeBreakdown)
	}
	if res.ActualTimeMs != nil || res.PlanningTimeMs != nil {
		t.Error("an estimated plan carries timings")
	}
}

func TestEstimatedPlanIgnoresTimingsAndBadJSON(t *testing.T) {
	timed := json.RawMessage(`[{"Plan":{"Node Type":"Result","Total Cost":0.01,` +
		`"Actual Total Time":3.5},"Planning Time":0.2}]`)
	if res := EstimatedPlan("q", timed); res.ActualTimeMs != nil || res.PlanningTimeMs != nil {
		t.Errorf("timings read from a plan treated as estimated: %+v", res)
	}
	for _, bad := range []string{``, `{}`, `[]`, `not json`} {
		res := EstimatedPlan("q", json.RawMessage(bad))
		if res.EstimatedCost != 0 || len(res.NodeBreakdown) != 0 {
			t.Errorf("%q: %+v, want no cost and no nodes", bad, res)
		}
	}
}
