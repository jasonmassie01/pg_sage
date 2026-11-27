package tuning

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexes-02: a replacement hands the optimizer the index it removes (name
// and OID), so the per-table index cap does not count that index.
func TestJudge_ReplacementTellsAdmitWhichIndexItRemoves(t *testing.T) {
	h, cur := replaceSetup(t)
	var got *optimizer.IndexRef
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		got = rec.Replaces
		rec.Table, rec.Category = "public.orders", optimizer.OptimizerCategory
		rec.WhatIf, rec.Validated = optimizer.WhatIfVerified, true
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitAccepted}
	}
	if j := judgeOn(t, h, cur, indexProposal(statusWide)); j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	if got == nil || got.Name != "orders_status_idx" || got.OID != 4242 {
		t.Fatalf("replaces = %+v, want orders_status_idx (4242)", got)
	}
}

func TestJudge_PlainCreateReplacesNothing(t *testing.T) {
	h, cur := replaceSetup(t)
	called := false
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		called = true
		if rec.Replaces != nil {
			t.Errorf("a plain create carries replaces %+v", rec.Replaces)
		}
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitInvalid, Reason: "x"}
	}
	judgeOn(t, h, cur, indexProposal(
		"CREATE INDEX CONCURRENTLY orders_c ON public.orders (customer_id, created_at)"))
	if !called {
		t.Fatal("the create never reached Admit")
	}
}
