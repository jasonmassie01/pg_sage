package analyzer

import (
	"strings"
	"testing"
)

// M0 plan_hash: plan-regression findings carry both plan fingerprints so
// a cost regression with an unchanged plan shape (stale statistics,
// parameter drift) is distinguishable from a real plan flip.

const seqPlan = `[{"Plan": {"Node Type": "Seq Scan",
  "Relation Name": "orders", "Total Cost": 5000.0}}]`
const idxPlan = `[{"Plan": {"Node Type": "Index Scan",
  "Index Name": "orders_pkey", "Relation Name": "orders",
  "Total Cost": 100.0}}]`

func planHashFinding(t *testing.T, p planPair) Finding {
	t.Helper()
	findings := rulePlanRegression([]planPair{p})
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	return findings[0]
}

func TestPlanRegression_DetailRecordsPlanFlip(t *testing.T) {
	f := planHashFinding(t, planPair{
		QueryID: 7, QueryText: "select",
		CurrentPlan: []byte(seqPlan), CurrentCost: 5000,
		PreviousPlan: []byte(idxPlan), PreviousCost: 100,
	})
	cur, _ := f.Detail["current_plan_hash"].(string)
	prev, _ := f.Detail["previous_plan_hash"].(string)
	if !strings.HasPrefix(cur, "v1:") || !strings.HasPrefix(prev, "v1:") {
		t.Fatalf("plan hashes missing: current=%q previous=%q", cur, prev)
	}
	if cur == prev {
		t.Fatalf("seq vs index plans share hash %q", cur)
	}
	if changed, ok := f.Detail["plan_changed"].(bool); !ok || !changed {
		t.Fatalf("plan_changed = %v, want true", f.Detail["plan_changed"])
	}
}

func TestPlanRegression_DetailSameShapeCostOnly(t *testing.T) {
	cheap := strings.Replace(seqPlan, "5000.0", "100.0", 1)
	f := planHashFinding(t, planPair{
		QueryID: 8, QueryText: "select",
		CurrentPlan: []byte(seqPlan), CurrentCost: 5000,
		PreviousPlan: []byte(cheap), PreviousCost: 100,
	})
	if f.Detail["current_plan_hash"] != f.Detail["previous_plan_hash"] {
		t.Fatalf("same shape produced different hashes: %v vs %v",
			f.Detail["current_plan_hash"], f.Detail["previous_plan_hash"])
	}
	if changed, ok := f.Detail["plan_changed"].(bool); !ok || changed {
		t.Fatalf("plan_changed = %v, want false for a cost-only change",
			f.Detail["plan_changed"])
	}
}

// A plan that cannot be fingerprinted must not claim "unchanged": the
// hash is omitted and plan_changed is unknown (absent), never false.
func TestPlanRegression_DetailUnhashablePlan(t *testing.T) {
	f := planHashFinding(t, planPair{
		QueryID: 9, QueryText: "select",
		CurrentPlan: []byte(seqPlan), CurrentCost: 5000,
		PreviousPlan: []byte(`not json`), PreviousCost: 100,
	})
	if _, ok := f.Detail["previous_plan_hash"]; ok {
		t.Errorf("previous_plan_hash present for an unparseable plan: %v",
			f.Detail["previous_plan_hash"])
	}
	if _, ok := f.Detail["plan_changed"]; ok {
		t.Errorf("plan_changed must be absent when a hash is unknown, got %v",
			f.Detail["plan_changed"])
	}
	if _, ok := f.Detail["current_plan_hash"].(string); !ok {
		t.Error("current_plan_hash missing for a valid current plan")
	}
}
