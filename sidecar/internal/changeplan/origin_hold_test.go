package changeplan

import (
	"strings"
	"testing"
)

// review-03b: a model-proposed step of a model-origin plan needs its own
// approval until its model is granted authority (review-05a's G-M1).
// These are the pure coverage rules; the store and engine reads are
// tested on a server in internal/review/plan. No concurrent access test:
// CoverageWithHolds is a pure function of its arguments.

func holdPlan() Plan {
	return Plan{PlanHash: strings.Repeat("a", 64), Origin: OriginModel, Steps: []Step{
		{ID: 1, Ordinal: 1, Kind: KindAction, ActionType: "create_index_concurrently",
			ProposedSQL: "CREATE INDEX CONCURRENTLY i1 ON public.t (a)",
			RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.i1"},
		{ID: 2, Ordinal: 2, Kind: KindAction, ActionType: "create_index_concurrently",
			ProposedSQL: "CREATE INDEX CONCURRENTLY i2 ON public.t (b)",
			RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.i2"},
		{ID: 3, Ordinal: 3, Kind: KindCheck, Metric: MetricReadback, Setting: "work_mem"},
	}}
}

func TestCoverageWithHolds_ApproveAllSkipsTheHeldModelStep(t *testing.T) {
	p := holdPlan()
	live := &Approval{PlanHash: p.PlanHash, Mode: ModeAll}
	holds := map[int64]string{2: ReasonModelOrigin + ": proposed by model m1"}
	got := CoverageWithHolds(p, live, nil, nil, holds)
	if len(got) != 3 {
		t.Fatalf("coverage of %d steps, want 3", len(got))
	}
	if !got[0].Covered || got[0].By != CoveredByPlan || got[0].NeedsOwnApproval {
		t.Errorf("generator step: %+v, want covered by the plan", got[0])
	}
	if got[1].Covered || !got[1].NeedsOwnApproval ||
		!strings.HasPrefix(got[1].Reason, ReasonModelOrigin+":") {
		t.Errorf("model step: %+v, want held with the model_origin reason", got[1])
	}
	if !got[2].Covered {
		t.Errorf("check step: %+v, want covered (it changes nothing)", got[2])
	}
}

func TestCoverageWithHolds_OwnStepApprovalCoversTheHeldStep(t *testing.T) {
	p := holdPlan()
	live := &Approval{PlanHash: p.PlanHash, Mode: ModeAll}
	own := []StepApproval{{StepID: 2, PlanHash: p.PlanHash}}
	got := CoverageWithHolds(p, live, own, nil, map[int64]string{2: ReasonModelOrigin + ": x"})
	if !got[1].Covered || got[1].By != CoveredByStep || !got[1].NeedsOwnApproval {
		t.Fatalf("model step with its own approval: %+v, want covered by the step", got[1])
	}
	stale := []StepApproval{{StepID: 2, PlanHash: strings.Repeat("b", 64)}}
	got = CoverageWithHolds(p, live, stale, nil, map[int64]string{2: ReasonModelOrigin + ": x"})
	if got[1].Covered {
		t.Fatalf("a step approval at another hash covered the held step: %+v", got[1])
	}
}

func TestCoverageWithHolds_NilHoldsIsCoverage(t *testing.T) {
	p := holdPlan()
	live := &Approval{PlanHash: p.PlanHash, Mode: ModeAll}
	a, b := Coverage(p, live, nil, nil), CoverageWithHolds(p, live, nil, nil, nil)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("step %d: Coverage %+v, CoverageWithHolds(nil) %+v", i+1, a[i], b[i])
		}
	}
	if !a[1].Covered {
		t.Fatalf("without holds the second step is %+v, want covered", a[1])
	}
}

func TestCoverageWithHolds_ClassReasonWinsOverTheHold(t *testing.T) {
	p := holdPlan()
	p.Steps[1].ProposedSQL = "DROP TABLE public.t"
	live := &Approval{PlanHash: p.PlanHash, Mode: ModeAll}
	got := CoverageWithHolds(p, live, nil, nil, map[int64]string{2: ReasonModelOrigin + ": x"})
	if got[1].Covered || !strings.HasPrefix(got[1].Reason, ReasonDroppedData) {
		t.Fatalf("dropping step: %+v, want held for dropped_data", got[1])
	}
}
