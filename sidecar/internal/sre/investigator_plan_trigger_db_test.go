package sre

import (
	"strings"
	"testing"
)

// The narrow plan of a plan-regression investigation offers the model
// the plan-only EXPLAIN (with an explainer configured); a lock
// investigation's narrow plan does not, and neither plan gains budget.

func offeredTools(t *testing.T, body string) []string {
	t.Helper()
	var names []string
	for _, part := range strings.Split(body, `"name":"`)[1:] {
		names = append(names, part[:strings.Index(part, `"`)])
	}
	return names
}

func TestInvestigator_PlanRegressionNarrowPlanOffersExplain(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	for kind, want := range map[TriggerKind]bool{TriggerPlan: true, TriggerLock: false} {
		m := newFakeModel(t, submitFixed(invFinal{Outcome: "inconclusive"}))
		c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
			invOptions{config: InvestigatorConfig{Explainer: NewStatementExplainer(pool)}})
		inv := startAndRun(t, ctx, c, Trigger{CaseID: "trigger-tools:" + string(kind),
			Kind: kind, Subject: "queryid 42", IdempotencyKey: "tt:" + string(kind)})
		run := transcriptOf(t, inv)
		if run.Plan != PlanNarrow || run.Budget.MaxSteps != 5 {
			t.Fatalf("%s: plan %s budget %+v, want narrow", kind, run.Plan, run.Budget)
		}
		got := strings.Join(offeredTools(t, m.body(t, 0)), ",")
		if strings.Contains(got, ToolExplain) != want {
			t.Fatalf("%s: offered tools %s, EXPLAIN wanted %v", kind, got, want)
		}
		if !strings.Contains(got, ToolRunProbe) || !strings.Contains(got, ToolSubmit) {
			t.Fatalf("%s: offered tools %s", kind, got)
		}
	}
}
