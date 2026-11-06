package tuning

import (
	"slices"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexes-05: the HypoPG what-if measures at most 25 statements and always
// the recommendation's affected queryids, so admission hands the
// proposal's target statements to the optimizer as AffectedQueryIDs.

// recordAffected makes h's admission record the AffectedQueryIDs it is
// handed, then admit as verified.
func recordAffected(h *harness) func() []int64 {
	var mu sync.Mutex
	var seen []int64
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		mu.Lock()
		seen = append([]int64(nil), rec.AffectedQueryIDs...)
		mu.Unlock()
		rec.Table, rec.Category = "public.orders", optimizer.OptimizerCategory
		rec.WhatIf, rec.Validated = optimizer.WhatIfVerified, true
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitAccepted}
	}
	return func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func TestJudge_IndexCreateHandsTargetsToTheWhatIf(t *testing.T) {
	h := newHarness(t)
	seen := recordAffected(h)
	p := createProposal()
	p.TargetQueryIDs = []QueryID{101}
	if j := judgeOne(t, h, nil, p); j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	if got := seen(); !slices.Equal(got, []int64{101}) {
		t.Fatalf("what-if affected queryids = %v, want the proposal's target [101]", got)
	}
}

func TestJudge_IndexCreateWithoutTargetsUsesTheCaseStatements(t *testing.T) {
	h := newHarness(t)
	seen := recordAffected(h)
	if j := judgeOne(t, h, nil, createProposal()); j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	if got := seen(); !slices.Equal(got, []int64{101}) {
		t.Fatalf("what-if affected queryids = %v, want the case's statement [101]", got)
	}
}

// Invalid input: a target that is not a workload statement is not handed
// over (the case statements stand in).
func TestJudge_IndexCreateDropsNonWorkloadTargets(t *testing.T) {
	h := newHarness(t)
	seen := recordAffected(h)
	p := createProposal()
	p.TargetQueryIDs = []QueryID{987654}
	if j := judgeOne(t, h, nil, p); j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	if got := seen(); slices.Contains(got, 987654) || !slices.Equal(got, []int64{101}) {
		t.Fatalf("what-if affected queryids = %v, want [101] without the unknown id", got)
	}
}

// The admitted finding claims no plan source: no plan was captured.
func TestJudge_IndexCreateFindingHasNoPlanSource(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, createProposal())
	if j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	if src, ok := j.Finding.Detail["plan_source"]; ok {
		t.Fatalf("plan_source = %v, want absent", src)
	}
}
