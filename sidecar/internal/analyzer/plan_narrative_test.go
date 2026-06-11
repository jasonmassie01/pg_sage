package analyzer

import (
	"strings"
	"testing"
)

func TestBuildPlanNarrativePrompt(t *testing.T) {
	f := Finding{
		Category: "plan_regression",
		Detail: map[string]any{
			"query":            "SELECT * FROM orders WHERE customer_id = $1",
			"cost_ratio":       8.5,
			"previous_cost":    120.0,
			"current_cost":     1020.0,
			"node_changes":     []string{"Index Scan -> Seq Scan"},
			"new_disk_spills":  true,
			"previous_summary": "Index Scan on orders_customer_idx",
			"current_summary":  "Seq Scan on orders",
		},
	}
	p := buildPlanNarrativePrompt(f)
	for _, want := range []string{
		"SELECT * FROM orders", "8.5x", "Index Scan -> Seq Scan",
		"disk spills", "Previous plan:", "Current plan:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

// Narrate with a nil client is a no-op (degrades gracefully).
func TestNarrate_NilClientNoOp(t *testing.T) {
	var n *LLMPlanNarrator
	in := []Finding{{Category: "plan_regression"}}
	out := n.Narrate(nil, in)
	if len(out) != 1 || out[0].Detail["narrative"] != nil {
		t.Error("nil narrator should pass findings through unchanged")
	}
}

// planDetailStrings handles both []string and JSON []any.
func TestPlanDetailStrings(t *testing.T) {
	if got := planDetailStrings(map[string]any{"x": []string{"a", "b"}}, "x"); len(got) != 2 {
		t.Errorf("[]string: %v", got)
	}
	if got := planDetailStrings(map[string]any{"x": []any{"a", "b", 3}}, "x"); len(got) != 2 {
		t.Errorf("[]any: %v", got)
	}
}

// TestPlanDetailHelpers covers the numeric/string/nil branches.
func TestPlanDetailHelpers(t *testing.T) {
	if planDetailFloat(map[string]any{"x": int64(5)}, "x") != 5 {
		t.Error("int64 branch")
	}
	if planDetailFloat(map[string]any{"x": int(7)}, "x") != 7 {
		t.Error("int branch")
	}
	if planDetailFloat(map[string]any{"x": 3.5}, "x") != 3.5 {
		t.Error("float64 branch")
	}
	if planDetailFloat(nil, "x") != 0 {
		t.Error("nil map should yield 0")
	}
	if planDetailStr(nil, "x") != "" {
		t.Error("nil map should yield empty string")
	}
	if planDetailStr(map[string]any{"x": 42}, "x") != "" {
		t.Error("non-string should yield empty string")
	}
	if got := planDetailStrings(nil, "x"); got != nil {
		t.Errorf("nil map: %v", got)
	}
	// A finding with empty detail produces a prompt without panicking.
	if p := buildPlanNarrativePrompt(Finding{Category: "plan_regression"}); p == "" {
		t.Error("expected a non-empty prompt skeleton")
	}
}
