package shadow

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 1.4: a shadow decision is scored later, deterministically, by
// the best available evidence, in this order:
//   (a) the operator's decision on the same proposal (approved and
//       verified improved: correct; rejected: incorrect),
//   (b) the observed outcome of the same change applied later by anyone,
//       through pg_sage (its real verdict) or outside it (a migration,
//       verified with the same statistics),
//   (c) HypoPG what-if for index creates whose targeted queries still run,
//   (d) unscored when nothing applies.
// Never from LLM text. Only (b)-outside and (c) count as shadow evidence
// in the trust ledger: (a) and (b)-through-pg_sage are already real
// outcomes there, and counting them again would double the evidence.

func indexShadow() Decision {
	return Decision{Family: "tuning", Class: "index_create",
		SQL: `CREATE INDEX CONCURRENTLY i ON public.o (a)`}
}

func dropShadow() Decision {
	return Decision{Family: "hygiene", Class: "index_drop",
		SQL: `DROP INDEX CONCURRENTLY public.i_old`}
}

func gucShadow() Decision {
	return Decision{Family: "tuning", Class: "config_guc",
		SQL: `ALTER SYSTEM SET work_mem = '64MB'`}
}

func TestScoreForVerdictFollowsTheFamilyRule(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		family, class             string
		verdict, lifecycle, score string
		decided                   bool
	}{
		{"tuning improved", "tuning", "index_create", "improved", "success", ScoreCorrect, true},
		{"tuning regressed", "tuning", "index_create", "regressed", "rolled_back",
			ScoreIncorrect, true},
		{"tuning neutral kept", "tuning", "config_guc", "neutral", "success", ScoreNeutral, true},
		{"no-gain revert of a create is neutral", "tuning", "index_create", "neutral",
			"rolled_back", ScoreNeutral, true},
		{"hygiene neutral that held", "hygiene", "index_drop", "neutral", "success",
			ScoreCorrect, true},
		{"hygiene improved", "hygiene", "vacuum", "improved", "success", ScoreCorrect, true},
		{"operator rollback of a held drop", "hygiene", "index_drop", "neutral",
			"rolled_back", ScoreIncorrect, true},
		{"operator rollback of an improvement", "tuning", "config_guc", "improved",
			"rolled_back", ScoreIncorrect, true},
		{"operator rollback before the verdict", "tuning", "config_guc", "",
			"rolled_back", ScoreIncorrect, true},
		{"insufficient evidence", "tuning", "index_create", "insufficient_evidence",
			"unverifiable", ScoreUnscored, true},
		{"unverifiable", "hygiene", "analyze", "unverifiable", "unverifiable",
			ScoreUnscored, true},
		{"verdict still pending", "tuning", "index_create", "pending", "monitoring", "", false},
		{"no verdict yet", "tuning", "index_create", "", "pending", "", false},
		{"unknown verdict", "tuning", "index_create", "great", "success", "", false},
	} {
		score, decided := scoreForVerdict(tc.family, tc.class, tc.verdict, tc.lifecycle)
		if score != tc.score || decided != tc.decided {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tc.name, score, decided, tc.score,
				tc.decided)
		}
	}
}

func TestJudgeOperatorDecisionComesFirst(t *testing.T) {
	o := DefaultOptions()
	ev := facts{
		queue:  &queueFact{ID: 41, Status: "rejected"},
		action: &actionFact{ID: 7, Verdict: "improved", Lifecycle: "success"},
		hypo:   &hypoFact{Verdict: hypoVerified, Improvement: 60},
	}
	j := judge(indexShadow(), ev, 30*24*time.Hour, o)
	if j.Wait || j.Score != ScoreIncorrect || j.Source != SourceOperator ||
		j.RefQueueID != 41 || j.Counted {
		t.Fatalf("rejected proposal: %+v", j)
	}
	ev.queue = &queueFact{ID: 42, Status: "executed", ActionLogID: 9, Verdict: "improved",
		Lifecycle: "success"}
	j = judge(indexShadow(), ev, time.Hour, o)
	if j.Score != ScoreCorrect || j.Source != SourceOperator || j.RefQueueID != 42 ||
		j.RefActionLogID != 9 || j.Counted {
		t.Fatalf("approved and improved: %+v", j)
	}
}

func TestJudgeWaitsForTheVerdictOfAnApprovedAction(t *testing.T) {
	o := DefaultOptions()
	for _, q := range []*queueFact{
		{ID: 1, Status: "approved"},
		{ID: 2, Status: "executed", ActionLogID: 3, Verdict: "pending", Lifecycle: "monitoring"},
	} {
		ev := facts{queue: q, hypo: &hypoFact{Verdict: hypoVerified, Improvement: 50}}
		// Past the horizon, a matched proposal still waits for its verdict:
		// the operator's evidence outranks the what-if.
		if j := judge(indexShadow(), ev, o.Horizon+time.Hour, o); !j.Wait {
			t.Fatalf("queue %+v: %+v, want wait", q, j)
		}
		// ...but not forever.
		j := judge(indexShadow(), ev, o.MaxWait, o)
		if j.Wait || j.Score != ScoreUnscored || j.Source != SourceNone || j.Counted {
			t.Fatalf("queue %+v past max wait: %+v", q, j)
		}
	}
}

func TestJudgeInconclusiveOperatorEvidenceIsUnscored(t *testing.T) {
	ev := facts{queue: &queueFact{ID: 5, Status: "executed", ActionLogID: 6,
		Verdict: "insufficient_evidence", Lifecycle: "unverifiable"},
		hypo: &hypoFact{Verdict: hypoVerified, Improvement: 50}}
	j := judge(indexShadow(), ev, 2*24*time.Hour, DefaultOptions())
	if j.Wait || j.Score != ScoreUnscored || j.Source != SourceOperator || j.Counted {
		t.Fatalf("inconclusive approval: %+v", j)
	}
}

func TestJudgeAppliedChangeBeatsTheWhatIf(t *testing.T) {
	o := DefaultOptions()
	ev := facts{action: &actionFact{ID: 12, Verdict: "regressed", Lifecycle: "rolled_back"},
		hypo: &hypoFact{Verdict: hypoVerified, Improvement: 80}}
	j := judge(indexShadow(), ev, 3*24*time.Hour, o)
	if j.Score != ScoreIncorrect || j.Source != SourceApplied || j.RefActionLogID != 12 ||
		j.Counted {
		t.Fatalf("applied and regressed: %+v", j)
	}
	ev.action = &actionFact{ID: 13, Verdict: "", Lifecycle: "monitoring"}
	if j := judge(indexShadow(), ev, 3*24*time.Hour, o); !j.Wait {
		t.Fatalf("applied, verdict pending: %+v, want wait", j)
	}
}

func TestJudgeExternallyAppliedChangeCountsAsShadowEvidence(t *testing.T) {
	o := DefaultOptions()
	j := judge(dropShadow(), facts{external: &verifiedFact{Verdict: "neutral", Final: true}},
		8*24*time.Hour, o)
	if j.Score != ScoreCorrect || j.Source != SourceExternal || !j.Counted {
		t.Fatalf("migration dropped the index, reads held: %+v", j)
	}
	j = judge(indexShadow(), facts{external: &verifiedFact{Verdict: "neutral", Final: true}},
		2*24*time.Hour, o)
	if j.Score != ScoreNeutral || j.Source != SourceExternal || !j.Counted {
		t.Fatalf("migration created the index, no gain: %+v", j)
	}
	j = judge(indexShadow(), facts{external: &verifiedFact{Verdict: "improved", Final: true}},
		time.Hour, o)
	if j.Score != ScoreCorrect || !j.Counted {
		t.Fatalf("migration created the index, it helped: %+v", j)
	}
	j = judge(indexShadow(), facts{external: &verifiedFact{Verdict: "insufficient_evidence",
		Final: true}}, time.Hour, o)
	if j.Score != ScoreUnscored || j.Source != SourceExternal || j.Counted {
		t.Fatalf("migration applied, verification inconclusive: %+v", j)
	}
	pending := facts{external: &verifiedFact{Verdict: "insufficient_evidence"},
		hypo: &hypoFact{Verdict: hypoVerified, Improvement: 50}}
	if j := judge(indexShadow(), pending, 2*24*time.Hour, o); !j.Wait {
		t.Fatalf("external verification still accruing: %+v, want wait", j)
	}
}

func TestJudgeWhatIfOnlyForIndexCreatesAfterTheSettleDelay(t *testing.T) {
	o := DefaultOptions()
	verified := facts{hypo: &hypoFact{Verdict: hypoVerified, Improvement: 45}}
	if j := judge(indexShadow(), verified, o.ScoreAfter-time.Second, o); !j.Wait {
		t.Fatalf("before score_after: %+v, want wait", j)
	}
	j := judge(indexShadow(), verified, o.ScoreAfter, o)
	if j.Wait || j.Score != ScoreCorrect || j.Source != SourceHypoPG || !j.Counted {
		t.Fatalf("at score_after: %+v", j)
	}
	rejected := facts{hypo: &hypoFact{Verdict: hypoRejected, Improvement: 2}}
	j = judge(indexShadow(), rejected, o.ScoreAfter, o)
	if j.Score != ScoreIncorrect || j.Source != SourceHypoPG || !j.Counted {
		t.Fatalf("what-if rejects it: %+v", j)
	}
	unverified := facts{hypo: &hypoFact{Verdict: hypoUnverified}}
	if j := judge(indexShadow(), unverified, o.ScoreAfter, o); !j.Wait {
		t.Fatalf("what-if inconclusive before the horizon: %+v, want wait", j)
	}
	// A what-if is never evidence for another class.
	if j := judge(gucShadow(), verified, o.ScoreAfter, o); !j.Wait {
		t.Fatalf("what-if for a GUC: %+v, want wait", j)
	}
}

func TestJudgeUnscoredAtTheHorizon(t *testing.T) {
	o := DefaultOptions()
	if j := judge(gucShadow(), facts{}, o.Horizon-time.Second, o); !j.Wait {
		t.Fatalf("before the horizon: %+v", j)
	}
	j := judge(gucShadow(), facts{}, o.Horizon, o)
	if j.Wait || j.Score != ScoreUnscored || j.Source != SourceNone || j.Counted {
		t.Fatalf("at the horizon: %+v", j)
	}
	j = judge(indexShadow(), facts{hypo: &hypoFact{Verdict: hypoUnverified}}, o.Horizon, o)
	if j.Wait || j.Score != ScoreUnscored || j.Source != SourceNone {
		t.Fatalf("inconclusive what-if at the horizon: %+v", j)
	}
}

func TestJudgeIgnoresAnUndecidedQueueItem(t *testing.T) {
	o := DefaultOptions()
	// An expired or superseded proposal says nothing about the decision.
	for _, status := range []string{"expired", "superseded", "pending", "failed"} {
		ev := facts{queue: &queueFact{ID: 3, Status: status},
			hypo: &hypoFact{Verdict: hypoVerified, Improvement: 30}}
		j := judge(indexShadow(), ev, o.ScoreAfter, o)
		if j.Source != SourceHypoPG || j.Score != ScoreCorrect {
			t.Errorf("queue %s: %+v, want the what-if", status, j)
		}
	}
}

func TestDefaultOptionsAndNormalization(t *testing.T) {
	o := DefaultOptions()
	if o.ScoreAfter != 24*time.Hour || o.Horizon != 7*24*time.Hour ||
		o.MaxWait != 21*24*time.Hour || o.HypoPGBudget != 10 || o.Batch != 200 ||
		o.DedupeWindow != 24*time.Hour {
		t.Fatalf("defaults = %+v", o)
	}
	var zero Options
	n := zero.normalized()
	if n.ScoreAfter != o.ScoreAfter || n.Horizon != o.Horizon || n.MaxWait != o.MaxWait ||
		n.HypoPGBudget != o.HypoPGBudget || n.Batch != o.Batch ||
		n.VerifyWindow <= 0 || n.VerifyMaxWindow < n.VerifyWindow || n.DropWindow <= 0 ||
		n.HypoPGMinPct <= 0 || n.DedupeWindow != o.DedupeWindow {
		t.Fatalf("zero options normalized to %+v", n)
	}
	// A horizon shorter than the settle delay would score nothing by what-if.
	bad := Options{ScoreAfter: 48 * time.Hour, Horizon: time.Hour}
	if n := bad.normalized(); n.Horizon < n.ScoreAfter || n.MaxWait < n.Horizon {
		t.Fatalf("inconsistent windows kept: %+v", n)
	}
}

// An externally applied change's verdict is final: a regression at once,
// an improvement or neutral once the class's minimum window elapsed, too
// little evidence only at the cap.
func TestFinalVerdictBoundaries(t *testing.T) {
	lo, hi := time.Hour, 3*time.Hour
	for _, tc := range []struct {
		verdict string
		win     time.Duration
		final   bool
	}{
		{verify.OutcomeRegressed, time.Minute, true},
		{verify.OutcomeImproved, lo - time.Second, false},
		{verify.OutcomeImproved, lo, true},
		{verify.OutcomeNeutral, lo, true},
		{verify.OutcomeInsufficient, hi - time.Second, false},
		{verify.OutcomeInsufficient, hi, true},
		{verify.OutcomeUnverifiable, hi, true},
	} {
		if got := finalVerdict(tc.verdict, tc.win, lo, hi); got != tc.final {
			t.Errorf("%s at %s: final=%v, want %v", tc.verdict, tc.win, got, tc.final)
		}
	}
}
