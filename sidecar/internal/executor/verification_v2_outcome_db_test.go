package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-01c review: the index watch's verdict reaches sage.action_outcome
// with its verifier and the other judge's verdict and evidence, through
// the executor's own outcome path (engineOutcome, then recordVerdict).

func shadowVerdict() verify.Verdict {
	record := map[string]any{"verifier": "v2", "method": "did", "outcome": "neutral",
		"effect_pct": -3.5}
	return verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain",
		Outcome: verify.OutcomeNeutral, Verifier: verify.VerifierV1,
		Evidence: map[string]any{"v2": record},
		Other: &verify.OtherVerdict{Verifier: verify.VerifierV2,
			Outcome: verify.OutcomeNeutral, Reason: verify.ReasonV2NoChange,
			Evidence: map[string]any{"v2": record}}}
}

func TestV2_EngineOutcomeRecordsTheVerifierAndTheOtherVerdict(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	e := New(pool, config.DefaultConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	id := insertVerifiedAction(t, pool, verifiedActionRow{
		sql:        "CREATE INDEX CONCURRENTLY v2_probe ON public.t (a)",
		rollback:   "DROP INDEX CONCURRENTLY IF EXISTS public.v2_probe",
		before:     "{}",
		executedAt: time.Now().Add(-4 * time.Hour)})
	recordVerdict(ctx, pool, e.engineOutcome(ctx, id, shadowVerdict()),
		func(c, format string, a ...any) { t.Logf(c+": "+format, a...) })
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeNeutral || got.Verifier != verify.VerifierV1 ||
		got.OtherVerdict != verify.OutcomeNeutral {
		t.Fatalf("outcome verdict %q verifier %q other %q, want neutral by v1, v2 neutral",
			got.Verdict, got.Verifier, got.OtherVerdict)
	}
	var method string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(other_evidence->'v2'->>'method', '')
		FROM sage.action_outcome WHERE action_log_id = $1`, id).Scan(&method); err != nil {
		t.Fatalf("read other evidence: %v", err)
	}
	if method != "did" {
		t.Fatalf("other_evidence->'v2'->>'method' = %q, want did", method)
	}
}

// An undecided other verdict (v2 still extending) leaves other_verdict
// empty but keeps its evidence; no other verdict at all stores none.
func TestV2_WithVerifierUndecidedAndAbsentOther(t *testing.T) {
	v := shadowVerdict()
	v.Other.Outcome = ""
	var o verify.Outcome
	withVerifier(&o, v)
	if o.Verifier != verify.VerifierV1 || o.OtherVerdict != "" || o.OtherEvidence == nil {
		t.Fatalf("outcome %+v, want v1 with the undecided v2 evidence and no other verdict", o)
	}
	o = verify.Outcome{}
	withVerifier(&o, verify.Verdict{Verifier: verify.VerifierV1})
	if o.Verifier != verify.VerifierV1 || o.OtherVerdict != "" || o.OtherEvidence != nil {
		t.Fatalf("outcome %+v, want v1 alone", o)
	}
}
