package tuner

import (
	"context"
	"testing"
)

// tuning-11 review: a plan-time statement whose other symptom prescribes no
// hint (here a stat-based spill the tuner could not size) still gets its
// parameterization advice; the cycle used to drop it while marking the
// shape reported, so it was never reported.
func TestHighPlanTime_AdviceKeptWhenOtherSymptomPrescribesNothing(t *testing.T) {
	tu := New(nil, planTimeCfg, nil, noopLogFn)
	c := highPlanTimeCandidate()
	tu.spill.needs = map[spillKey]spillSizing{keyOf(c): {DeltaCalls: 200, HintMB: 0}}
	got := tu.processCandidate(context.Background(), c)
	if len(got) != 1 || got[0].Category != "query_parameterization" ||
		got[0].Detail["reason"] != "plan_time_share" {
		t.Fatalf("findings = %+v, want one query_parameterization/plan_time_share", got)
	}
	for _, f := range got {
		if f.Category == "query_tuning" {
			t.Errorf("an unprescribable symptom produced a query_tuning finding: %+v", f)
		}
	}
}

// Without the plan-time symptom an unprescribable spill is still no finding.
func TestProcessCandidate_UnprescribableSymptomAloneNoFinding(t *testing.T) {
	tu := New(nil, planTimeCfg, nil, noopLogFn)
	c := highPlanTimeCandidate()
	c.MeanPlanTime = 0
	tu.spill.needs = map[spillKey]spillSizing{keyOf(c): {DeltaCalls: 200, HintMB: 0}}
	if got := tu.processCandidate(context.Background(), c); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}
