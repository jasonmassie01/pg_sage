package sre

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Owner decision (2026-10-04, PR #116): the narrow plan offers the
// plan-only EXPLAIN for plan-regression triggers, and only for them;
// its budget does not change. Tools per trigger are plan data, checked
// like the plan's own tools.

func TestInvestigatorPlans_NarrowOffersExplainOnlyForPlanRegressions(t *testing.T) {
	plans := DefaultInvestigatorPlans()
	narrow, broad := plans[PlanNarrow], plans[PlanBroad]
	if got := narrow.ToolsFor(TriggerPlan); !slices.Contains(got, ToolExplain) {
		t.Fatalf("narrow tools for plan_regression = %v, want EXPLAIN", got)
	}
	for _, kind := range []TriggerKind{TriggerLock, TriggerWAL, TriggerConnections,
		TriggerCheckpoint, "never_heard_of_it"} {
		if slices.Contains(narrow.ToolsFor(kind), ToolExplain) {
			t.Errorf("narrow tools for %s offer EXPLAIN", kind)
		}
	}
	if slices.Contains(narrow.Tools, ToolExplain) {
		t.Fatalf("the narrow plan's base tools gained EXPLAIN: %v", narrow.Tools)
	}
	if narrow.MaxSteps != 5 || narrow.MaxProbes != 3 || narrow.MaxTokens != 32000 {
		t.Fatalf("narrow budget changed: %+v", narrow)
	}
	for _, kind := range []TriggerKind{TriggerPlan, TriggerOperator} {
		n := 0
		for _, tool := range broad.ToolsFor(kind) {
			if tool == ToolExplain {
				n++
			}
		}
		if n != 1 {
			t.Errorf("broad tools for %s offer EXPLAIN %d times", kind, n)
		}
	}
}

func TestInvestigatorPlans_ToolsForReturnsACopy(t *testing.T) {
	narrow := DefaultInvestigatorPlans()[PlanNarrow]
	got := narrow.ToolsFor(TriggerPlan)
	got[0] = "mutated"
	if again := DefaultInvestigatorPlans()[PlanNarrow].ToolsFor(TriggerPlan); again[0] ==
		"mutated" {
		t.Fatal("ToolsFor shares the default plan's slice")
	}
	if narrow.ToolsFor(TriggerPlan)[0] == "mutated" {
		t.Fatal("ToolsFor shares the plan's slice")
	}
}

func TestInvestigatorPlan_ValidateChecksTriggerTools(t *testing.T) {
	for name, extra := range map[string][]string{
		"unknown":        {"run_sql"},
		"repeats a base": {ToolRunProbe},
		"repeated":       {ToolExplain, ToolExplain},
		"the final":      {ToolSubmit},
	} {
		p := DefaultInvestigatorPlans()[PlanNarrow]
		p.TriggerTools = map[TriggerKind][]string{TriggerPlan: extra}
		err := p.Validate()
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "tool") {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
	empty := DefaultInvestigatorPlans()[PlanNarrow]
	empty.TriggerTools = nil
	if err := empty.Validate(); err != nil || slices.Contains(empty.ToolsFor(TriggerPlan),
		ToolExplain) {
		t.Fatalf("no trigger tools: %v %v", err, empty.ToolsFor(TriggerPlan))
	}
}
