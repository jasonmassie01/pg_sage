package tuning

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// indexes-07: a covered drop's prediction was a fixed 0% (MethodRule). It
// is now measured by hiding the index (HypoPG 1.4+): the measured change
// becomes the prediction (MethodModel), and a regression rejects the drop.

// No concurrent access test: one validator judges one case's proposals in
// turn under the agent's mutex.

const coveredDrop = "public.orders_customer_idx"

// dropSnap is validationSnap with orders_customer_idx's identity (OID 777).
func dropSnap() *collector.Snapshot {
	cur := validationSnap()
	for i := range cur.Indexes {
		if cur.Indexes[i].IndexRelName == "orders_customer_idx" {
			cur.Indexes[i].IndexRelID = 777
		}
	}
	return cur
}

func TestJudge_CoveredDropUsesTheMeasuredChange(t *testing.T) {
	h := newHarness(t)
	h.indexes.drop = func(optimizer.IndexRef) optimizer.DropMeasurement {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfVerified, ChangePct: 1.5,
			Measured: 1, Guarded: 1}
	}
	j := judgeOn(t, h, dropSnap(), dropProposal(coveredDrop))
	if j.Verdict != VerdictAdmitted || j.Class != verify.ClassIndexDrop {
		t.Fatalf("judged = %+v", j)
	}
	p := j.Prediction
	if p.Method != verify.MethodModel || p.ExpectedChangePct == nil ||
		*p.ExpectedChangePct != 1.5 || !strings.Contains(p.Note, "hidden") {
		t.Fatalf("prediction = %+v, want the measured +1.5%% (model)", p)
	}
	if got := h.indexes.dropsMeasured; len(got) != 1 || got[0].OID != 777 ||
		got[0].Name != "orders_customer_idx" {
		t.Fatalf("measured drops = %+v, want orders_customer_idx (777)", got)
	}
	if j.Finding.Detail["what_if_verdict"] != optimizer.WhatIfVerified {
		t.Fatalf("detail = %v, want the drop's what-if verdict", j.Finding.Detail)
	}
}

// A true duplicate measures 0%: a measured zero, not the rule's.
func TestJudge_DuplicateDropMeasuresZero(t *testing.T) {
	h := newHarness(t)
	h.indexes.drop = func(optimizer.IndexRef) optimizer.DropMeasurement {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfVerified, Measured: 2}
	}
	j := judgeOn(t, h, dropSnap(), dropProposal(coveredDrop))
	if j.Verdict != VerdictAdmitted || j.Prediction.Method != verify.MethodModel ||
		*j.Prediction.ExpectedChangePct != 0 {
		t.Fatalf("judged = %+v, want a measured 0%% (model)", j)
	}
}

func TestJudge_CoveredDropRegressionRejects(t *testing.T) {
	h := newHarness(t)
	h.indexes.drop = func(optimizer.IndexRef) optimizer.DropMeasurement {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfRejected,
			Reason: "hiding orders_customer_idx raises queryid 101's planned cost by 40.0%"}
	}
	prev, _ := ordersPair()
	cur := dropSnap()
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, h.store.rejected)
	v.prepare(t.Context(), prev, nil)
	j := v.judge(t.Context(), ordersCase(), ordersEvidence(), dropProposal(coveredDrop))
	if j.Verdict != VerdictRejected || j.Reason != ReasonWhatIfRejected || j.Finding != nil ||
		!strings.Contains(j.Detail, "queryid 101") {
		t.Fatalf("judged = %+v, want what-if rejected naming queryid 101", j)
	}
	if v.dropping["orders_customer_idx"] {
		t.Fatal("a rejected drop still marks the index as being dropped")
	}
}

// Unverified (no hide support, HypoPG unavailable): the rule's 0% stands,
// and the note says why it was not measured.
func TestJudge_UnmeasuredDropKeepsTheRule(t *testing.T) {
	h := newHarness(t)
	h.indexes.drop = func(optimizer.IndexRef) optimizer.DropMeasurement {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfUnverified,
			Reason: "HypoPG without hypopg_hide_index (1.4+): the drop was not measured"}
	}
	j := judgeOn(t, h, dropSnap(), dropProposal(coveredDrop))
	p := j.Prediction
	if j.Verdict != VerdictAdmitted || p.Method != verify.MethodRule ||
		*p.ExpectedChangePct != 0 || !strings.Contains(p.Note, "hypopg_hide_index") {
		t.Fatalf("judged = %+v, want the rule's 0%% with the reason", j)
	}
	if j.Finding.Detail["what_if_reason"] == nil {
		t.Fatalf("detail = %v, want the what-if reason", j.Finding.Detail)
	}
}

// Nil/zero: an index without an identity (OID 0) cannot be hidden, so it
// is not measured at all and keeps the rule's prediction.
func TestJudge_DropWithoutIdentityIsNotMeasured(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, dropProposal(coveredDrop))
	if j.Verdict != VerdictAdmitted || j.Prediction.Method != verify.MethodRule {
		t.Fatalf("judged = %+v", j)
	}
	if len(h.indexes.dropsMeasured) != 0 {
		t.Fatalf("an index without an OID was measured: %+v", h.indexes.dropsMeasured)
	}
	if !strings.Contains(j.Prediction.Note, "OID") {
		t.Fatalf("note %q, want why it was not measured", j.Prediction.Note)
	}
}

// A replacement the what-if rejects for a guarded regression is refused
// with the optimizer's reason, which names the queryid.
func TestJudge_ReplacementRejectedForAGuardedRegression(t *testing.T) {
	h, cur := replaceSetup(t)
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitRejected,
			Reason: "replacing orders_status_idx raises the planned cost of queryid 101 " +
				"by 54.2%"}
	}
	j := judgeOn(t, h, cur, indexProposal(statusWide))
	if j.Verdict != VerdictRejected || j.Reason != ReasonWhatIfRejected ||
		!strings.Contains(j.Detail, "queryid 101") || j.Finding != nil {
		t.Fatalf("judged = %+v, want what-if rejected naming queryid 101", j)
	}
}
