package verify

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// No concurrent access tests: the outcome model is pure data and pure
// functions; persistence is covered by outcome_store_db_test.go.

func pct(v float64) *float64 { return &v }

func TestPredictionPredicts(t *testing.T) {
	cases := []struct {
		name string
		p    Prediction
		want bool
	}{
		{"hypopg with change", Prediction{Method: MethodHypoPG, ExpectedChangePct: pct(-40)},
			true},
		{"rule with zero change", Prediction{Method: MethodRule, ExpectedChangePct: pct(0)},
			true},
		{"none", Prediction{Method: MethodNone, ExpectedChangePct: pct(-40)}, false},
		{"empty method", Prediction{ExpectedChangePct: pct(-40)}, false},
		{"no expected change", Prediction{Method: MethodModel}, false},
		{"unknown method", Prediction{Method: "guess", ExpectedChangePct: pct(-1)}, false},
	}
	for _, tc := range cases {
		if got := tc.p.Predicts(); got != tc.want {
			t.Errorf("%s: Predicts() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNoPredictionIsExplicit(t *testing.T) {
	p := NoPrediction(ClassGUC, "advisor gave no expected effect")
	if p.Method != MethodNone || p.Class != ClassGUC || p.Predicts() {
		t.Fatalf("NoPrediction = %+v", p)
	}
	if p.Note == "" {
		t.Fatal("NoPrediction must say why there is no prediction")
	}
}

func TestToleranceVerdict(t *testing.T) {
	predicted := Prediction{Method: MethodHypoPG, ExpectedChangePct: pct(-40)}
	zero := Prediction{Method: MethodRule, ExpectedChangePct: pct(0)}
	cases := []struct {
		name    string
		p       Prediction
		verdict string
		obs     *float64
		want    string
	}{
		{"no prediction", NoPrediction(ClassGUC, "x"), OutcomeImproved, pct(-40),
			ToleranceNoPrediction},
		{"insufficient", predicted, OutcomeInsufficient, pct(-40), ToleranceUnmeasured},
		{"unverifiable", predicted, OutcomeUnverifiable, nil, ToleranceUnmeasured},
		{"no observation", predicted, OutcomeImproved, nil, ToleranceUnmeasured},
		{"met exactly", predicted, OutcomeImproved, pct(-40), ToleranceMet},
		{"met beyond", predicted, OutcomeImproved, pct(-70), ToleranceMet},
		{"met at half", predicted, OutcomeImproved, pct(-20), ToleranceMet},
		{"partial", predicted, OutcomeNeutral, pct(-19), TolerancePartial},
		{"missed no change", predicted, OutcomeNeutral, pct(0), ToleranceMissed},
		{"missed wrong way", predicted, OutcomeRegressed, pct(25), ToleranceMissed},
		{"zero held", zero, OutcomeImproved, pct(4), ToleranceMet},
		{"zero at band", zero, OutcomeImproved, pct(-NoChangeBandPct), ToleranceMet},
		{"zero broken", zero, OutcomeRegressed, pct(NoChangeBandPct + 1), ToleranceMissed},
		{"increase predicted", Prediction{Method: MethodRule, ExpectedChangePct: pct(5)},
			OutcomeImproved, pct(6), ToleranceMet},
	}
	for _, tc := range cases {
		if got := ToleranceVerdict(tc.p, tc.verdict, tc.obs); got != tc.want {
			t.Errorf("%s: ToleranceVerdict = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestValidVerdicts(t *testing.T) {
	for _, v := range []string{OutcomeImproved, OutcomeNeutral, OutcomeRegressed,
		OutcomeInsufficient, OutcomeUnverifiable} {
		if !DecidedVerdict(v) {
			t.Errorf("DecidedVerdict(%q) = false", v)
		}
	}
	for _, v := range []string{"", OutcomePending, "success", "IMPROVED"} {
		if DecidedVerdict(v) {
			t.Errorf("DecidedVerdict(%q) = true", v)
		}
	}
}

func TestDecideTargetsPoolsCallWeighted(t *testing.T) {
	// q1: 400 calls 100->60 ms; q2: 100 calls 10->10.5 ms. Pooled before
	// = (40000+1000)/500 = 82 ms, after = (24000+1050)/500 = 50.1 ms. The
	// unweighted mean of the per-query changes (-40% and +5%) would be
	// -17.5%: below the gain bar.
	before := map[int64]Measurement{1: meas(400, 100, 1, 12), 2: meas(100, 10, 0.2, 12)}
	after := map[int64]Measurement{1: meas(400, 60, 1, 12), 2: meas(100, 10.5, 0.2, 12)}
	pooled, per := DecideTargets(before, after, []int64{1, 2}, th())
	if pooled.Verdict != OutcomeImproved {
		t.Fatalf("pooled verdict = %s (%s), want improved", pooled.Verdict, pooled.Reason)
	}
	if math.Abs(pooled.DeltaPct-(50.1/82-1)*100) > 0.01 {
		t.Fatalf("pooled delta = %.3f, want call-weighted %.3f", pooled.DeltaPct,
			(50.1/82-1)*100)
	}
	if len(per) != 2 || per[0].QueryID != 1 || per[1].QueryID != 2 {
		t.Fatalf("per-target comparisons = %+v", per)
	}
	if per[1].Verdict == OutcomeRegressed {
		t.Fatalf("q2 (+5%%) = %s, must not count as a regression", per[1].Verdict)
	}
}

func TestDecideTargetsAnyRegressionIsRegressed(t *testing.T) {
	before := map[int64]Measurement{1: meas(4000, 100, 1, 24), 2: meas(400, 10, 0.1, 24)}
	after := map[int64]Measurement{1: meas(4000, 70, 1, 24), 2: meas(400, 20, 0.1, 24)}
	pooled, _ := DecideTargets(before, after, []int64{1, 2}, th())
	if pooled.Verdict != OutcomeRegressed {
		t.Fatalf("verdict = %s, want regressed when a target doubled", pooled.Verdict)
	}
	if pooled.Reason == "" {
		t.Fatal("regressed verdict must name the regressed target")
	}
}

// With many targets the per-query test is Bonferroni-corrected: a change
// that is significant for one query at 5% is not enough among 20.
func TestDecideTargetsCorrectsForMultipleTargets(t *testing.T) {
	ids := make([]int64, 20)
	before, after := map[int64]Measurement{}, map[int64]Measurement{}
	for i := range ids {
		ids[i] = int64(i + 1)
		before[ids[i]] = meas(200, 100, 5, 30)
		after[ids[i]] = meas(200, 100, 5, 30)
	}
	// +20% with t = 20/sqrt(50) ~ 2.83: significant alone (p ~ 0.006),
	// not after correcting for 20 targets (needs p < 0.0025).
	after[7] = meas(200, 120, 5, 30)
	alone := Compare(before[7], after[7], th())
	if alone.Verdict != OutcomeRegressed {
		t.Fatalf("precondition: single-target verdict = %s, want regressed", alone.Verdict)
	}
	pooled, _ := DecideTargets(before, after, ids, th())
	if pooled.Verdict == OutcomeRegressed {
		t.Fatal("one marginal target among 20 flipped the verdict to regressed")
	}
}

func TestDecideTargetsMissingDataIsInsufficient(t *testing.T) {
	pooled, per := DecideTargets(map[int64]Measurement{}, map[int64]Measurement{},
		[]int64{1}, th())
	if pooled.Verdict != OutcomeInsufficient || len(per) != 1 {
		t.Fatalf("verdict = %s per=%d, want insufficient_evidence", pooled.Verdict, len(per))
	}
	pooled, per = DecideTargets(nil, nil, nil, th())
	if pooled.Verdict != OutcomeInsufficient || len(per) != 0 {
		t.Fatalf("no targets = %s, want insufficient_evidence", pooled.Verdict)
	}
}

func TestComparisonEvidenceIsJSONSafe(t *testing.T) {
	// Zero variance gives an infinite statistic internally; the evidence
	// must still encode (NaN/Inf break json.Marshal).
	c := Compare(meas(500, 100, 0, 12), meas(500, 50, 0, 12), th())
	raw, err := json.Marshal(c.Evidence())
	if err != nil {
		t.Fatalf("evidence does not encode: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil || back["verdict"] != OutcomeImproved {
		t.Fatalf("evidence = %s (%v)", raw, err)
	}
	for _, key := range []string{"delta_pct", "ci_low_pct", "ci_high_pct", "before",
		"after"} {
		if _, ok := back[key]; !ok {
			t.Errorf("evidence lacks %q: %s", key, raw)
		}
	}
}

func TestOutcomeJSONRoundTrip(t *testing.T) {
	start := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	o := Outcome{ActionLogID: 7, Class: ClassIndexDrop, Verdict: OutcomeRegressed,
		Tolerance: ToleranceMissed,
		Predicted: Prediction{Class: ClassIndexDrop, Method: MethodRule,
			Metric: MetricMeanExecTime, TargetQueryIDs: []int64{9},
			ExpectedChangePct: pct(0)},
		Observed: Observed{Metric: MetricMeanExecTime, Before: 10, After: 30,
			ChangePct: pct(200)},
		Evidence: map[string]any{"soft_drop": true}, Reason: "q9 regressed",
		WindowStart: &start}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Outcome
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Verdict != o.Verdict || back.Predicted.TargetQueryIDs[0] != 9 ||
		*back.Observed.ChangePct != 200 || !back.WindowStart.Equal(start) {
		t.Fatalf("round trip lost data: %+v", back)
	}
}
