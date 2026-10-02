package sre

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The verifier pass (AI-SRE-SPEC §5, DiagGuard-style): before concluding,
// the model's ranking is rechecked against the final diagnosis and every
// claim against the evidence re-read from the store. A conclusive graph
// always wins; a disagreement is reported, never applied.

// checkedReview is a review accepted by the check of the scope it was
// made in (so its evidence aliases are bound to stored ids).
func checkedReview(t *testing.T, d causal.Diagnosis, ev []Evidence, allowProbe bool,
	w wireReview) modelReview {
	t.Helper()
	r, rej := checkWith(t, newReviewScope(d, ev, allowProbe), w.json())
	if rej != nil {
		t.Fatalf("review rejected at check time: %+v", rej)
	}
	return r
}

func groundedReview(t *testing.T, d causal.Diagnosis, ev []Evidence,
	ranking ...string) modelReview {
	t.Helper()
	return checkedReview(t, d, ev, false, wireReview{Ranking: ranking,
		Claims: []wireClaim{{Text: "pid 4242 is idle in transaction and blocks 2 " +
			"sessions.", EvidenceIDs: []string{"E1"}}}})
}

func TestVerifyReview_AgreementKeepsEverything(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	v := verifyReview(groundedReview(t, d, ev, idleRanking()...), d, ev)
	if v.disagreed || len(v.rejected) != 0 {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.review.Ranking) != 2 || v.review.Ranking[0] != "idle_in_tx_holder" ||
		len(v.review.Claims) != 1 {
		t.Fatalf("verified review = %+v", v.review)
	}
}

// The model may not change a conclusive root cause: the graph wins, the
// disagreement is reported, and nothing the model said is kept.
func TestVerifyReview_ConclusiveGraphWins(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	r := groundedReview(t, d, ev, "ddl_lock_queue", "idle_in_tx_holder")
	v := verifyReview(r, d, ev)
	if !v.disagreed || v.graphRoot != "idle_in_tx_holder" || v.modelRoot != "ddl_lock_queue" {
		t.Fatalf("verdict = %+v, want a disagreement", v)
	}
	if len(v.review.Ranking) != 0 || len(v.review.Claims) != 0 || v.review.NextProbe != nil {
		t.Fatalf("a disagreeing review kept %+v", v.review)
	}
	if d.Root == nil || string(d.Root.Node) != "idle_in_tx_holder" {
		t.Fatalf("the diagnosis changed: %+v", d.Root)
	}
}

// On an inconclusive graph any order of its open hypotheses is accepted.
func TestVerifyReview_InconclusiveAcceptsAnyOrder(t *testing.T) {
	_, ev, d := unknownLockFixture(t)
	r := checkedReview(t, d, ev, true, wireReview{Ranking: []string{"hot_row_contention",
		"idle_in_tx_holder", "ddl_lock_queue"}})
	v := verifyReview(r, d, ev)
	if v.disagreed || len(v.rejected) != 0 || len(v.review.Ranking) != 3 ||
		v.review.Ranking[0] != "hot_row_contention" {
		t.Fatalf("verdict = %+v", v)
	}
}

// A ranking made before the model's probe is stale when that probe ruled
// a ranked hypothesis out: the ranking is dropped, valid claims stay.
func TestVerifyReview_StaleRankingDropped(t *testing.T) {
	inv, before, d0 := unknownLockFixture(t)
	chain := idleChainRunner().Run(t.Context(), probes.LockGraph, probes.Args{})
	after := append(append([]Evidence(nil), before...), fixtureEvidence(t, chain)...)
	final := diagnoseEvidence(t, inv, after)
	if !final.Conclusive {
		t.Fatalf("the probe's evidence should conclude the diagnosis: %+v", final)
	}
	r := checkedReview(t, d0, before, true, wireReview{Ranking: unknownRanking(),
		Claims: []wireClaim{{Text: "The lock graph probe timed out.",
			EvidenceIDs: []string{"E1"}}}})
	v := verifyReview(r, final, after)
	if v.disagreed || len(v.review.Ranking) != 0 || len(v.review.Claims) != 1 {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.rejected) != 1 || v.rejected[0].Reason != RejectVerifier ||
		!strings.Contains(v.rejected[0].Detail, "hot_row_contention") {
		t.Fatalf("rejections = %+v, want one verifier rejection naming the node",
			v.rejected)
	}
}

// Claims are rechecked against the stored evidence: altered or missing
// evidence drops the narrative.
func TestVerifyReview_ClaimsRecheckedAgainstStoredEvidence(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	tampered := append([]Evidence(nil), ev...)
	tampered[0].Payload = []byte(strings.Replace(string(ev[0].Payload), "4242", "4243", 1))
	cases := map[string][]Evidence{"altered": tampered, "missing": ev[1:]}
	for name, stored := range cases {
		v := verifyReview(groundedReview(t, d, ev, idleRanking()...), d, stored)
		if len(v.review.Claims) != 0 {
			t.Errorf("%s evidence: claims kept %+v", name, v.review.Claims)
		}
		found := false
		for _, r := range v.rejected {
			found = found || r.Reason == RejectVerifier
		}
		if !found {
			t.Errorf("%s evidence: no verifier rejection in %+v", name, v.rejected)
		}
	}
}

func TestVerifyReview_EmptyReview(t *testing.T) {
	_, ev, d := unknownLockFixture(t)
	v := verifyReview(modelReview{}, d, ev)
	if v.disagreed || len(v.review.Claims) != 0 || len(v.review.Ranking) != 0 {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.rejected) != 1 || v.rejected[0].Reason != RejectVerifier {
		t.Fatalf("an empty ranking of open hypotheses must be rejected: %+v", v.rejected)
	}
}
