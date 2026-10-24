package receipt

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Decide is pure: the receipt follows from the review, its windows and
// what was observed in them. Harm signals rank: a lock incident or
// lock_chain finding on a touched relation, then confounders (the
// comparison cannot be trusted), then the watched statements' latency.

var (
	t0      = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	windows = Windows{BeforeFrom: t0.Add(-24 * time.Hour), BeforeTo: t0,
		AfterFrom: t0.Add(10 * time.Minute), AfterTo: t0.Add(24*time.Hour + 10*time.Minute)}
)

func meas(calls int, meanMs, seMs float64, buckets int) verify.Measurement {
	return verify.Measurement{Samples: calls,
		AverageLatency: time.Duration(meanMs * float64(time.Millisecond)),
		StdErr:         time.Duration(seMs * float64(time.Millisecond)), Buckets: buckets}
}

// decidedReview watches ids on rv.rv_orders, each predicted not to change.
func decidedReview(ids ...int64) changereview.Review {
	r := changereview.Review{ID: "0f14e45f-ceea-467a-9575-0c9a1d8e7a01",
		Verdict: changereview.VerdictSafe, LandingState: changereview.LandingLanded}
	watching(&r, []string{"rv.rv_orders"}, ids...)
	return r
}

func TestDecideCleanWhenLatencyHolds(t *testing.T) {
	r := decidedReview(100)
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)},
		After: map[int64]verify.Measurement{100: meas(220, 10, 0.1, 20)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptClean || rc.Reason != ReasonNoChange {
		t.Fatalf("verdict %s (%s), want clean (no_change)", rc.Verdict, rc.Reason)
	}
	if rc.Tolerance != verify.ToleranceMet {
		t.Fatalf("tolerance %q, want met for the no-change prediction", rc.Tolerance)
	}
	if len(rc.Targets) != 1 {
		t.Fatalf("targets %+v, want one", rc.Targets)
	}
	tg := rc.Targets[0]
	if tg.QueryID != 100 || tg.Verdict != verify.OutcomeNeutral || tg.BeforeCalls != 200 ||
		tg.AfterCalls != 220 || math.Abs(tg.BeforeMeanMs-10) > 1e-9 ||
		math.Abs(tg.AfterMeanMs-10) > 1e-9 || math.Abs(tg.ChangePct) > 1e-9 {
		t.Fatalf("target %+v", tg)
	}
	if rc.WindowHours != 24 || !rc.BeforeFrom.Equal(windows.BeforeFrom) ||
		!rc.AfterTo.Equal(windows.AfterTo) || !rc.BeforeTo.Equal(t0) {
		t.Fatalf("windows not recorded: %+v", rc)
	}
	if len(rc.ContradictedBy) != 0 || len(rc.Incidents) != 0 {
		t.Fatalf("clean receipt names contradictions: %+v", rc)
	}
}

func TestDecideLowTrafficWithoutHarm(t *testing.T) {
	r := decidedReview(100)
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(12, 10, 0.1, 2)},
		After: map[int64]verify.Measurement{100: meas(9, 30, 0.1, 2)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptCleanLowTraffic || rc.Reason != ReasonLowTraffic {
		t.Fatalf("verdict %s (%s), want clean_low_traffic", rc.Verdict, rc.Reason)
	}
	if rc.Tolerance != verify.ToleranceUnmeasured {
		t.Fatalf("tolerance %q, want unmeasured", rc.Tolerance)
	}
	if rc.Targets[0].Verdict != verify.OutcomeInsufficient {
		t.Fatalf("target verdict %q, want insufficient_evidence", rc.Targets[0].Verdict)
	}
}

func TestDecideNothingWatchedIsLowTraffic(t *testing.T) {
	rc := Decide(decidedReview(), windows, Observations{})
	if rc.Verdict != changereview.ReceiptCleanLowTraffic {
		t.Fatalf("verdict %s, want clean_low_traffic with no watch set", rc.Verdict)
	}
	if rc.Targets == nil || len(rc.Targets) != 0 {
		t.Fatalf("targets %#v, want an empty list", rc.Targets)
	}
	if rc.Tolerance != verify.ToleranceNoPrediction {
		t.Fatalf("tolerance %q, want no_prediction", rc.Tolerance)
	}
}

func TestDecideRegressedNamesTheQueryID(t *testing.T) {
	r := decidedReview(100, 101)
	ev, err := changereview.NewEvidence("queryid", "101", "Q2", map[string]any{"calls": 5})
	if err != nil {
		t.Fatal(err)
	}
	r.Evidence = []changereview.Evidence{ev}
	obs := Observations{
		Before: map[int64]verify.Measurement{100: meas(500, 10, 0.1, 20),
			101: meas(500, 10, 0.1, 20)},
		After: map[int64]verify.Measurement{100: meas(500, 10, 0.1, 20),
			101: meas(500, 18, 0.1, 20)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptRegressed || rc.Reason != ReasonLatency {
		t.Fatalf("verdict %s (%s), want regressed (latency)", rc.Verdict, rc.Reason)
	}
	if !reflect.DeepEqual(rc.ContradictedBy, []string{"queryid:101"}) {
		t.Fatalf("contradicted_by %v, want [queryid:101]", rc.ContradictedBy)
	}
	var tg changereview.ReceiptTarget
	for _, x := range rc.Targets {
		if x.QueryID == 101 {
			tg = x
		}
	}
	if tg.Verdict != verify.OutcomeRegressed || math.Abs(tg.ChangePct-80) > 0.01 ||
		!(tg.PValue > 0 && tg.PValue < 0.05) {
		t.Fatalf("regressed target %+v, want +80%% with a significant p-value", tg)
	}
	if !contains(rc.EvidenceIDs, "queryid:101") {
		t.Fatalf("evidence ids %v do not cite the queryid's evidence", rc.EvidenceIDs)
	}
	if rc.Tolerance != verify.ToleranceMissed {
		t.Fatalf("tolerance %q, want missed (predicted no change)", rc.Tolerance)
	}
}

// Two targets each too noisy to call alone (Bonferroni) but regressed
// together: the receipt still names a queryid, the worse one.
func TestDecidePooledRegressionNamesTheWorstTarget(t *testing.T) {
	r := decidedReview(100, 101)
	obs := Observations{
		Before: map[int64]verify.Measurement{100: meas(400, 10, 1, 20),
			101: meas(400, 10, 1, 20)},
		After: map[int64]verify.Measurement{100: meas(400, 12.5, 1, 20),
			101: meas(400, 12.6, 1, 20)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptRegressed {
		t.Fatalf("verdict %s, want regressed from the pooled comparison", rc.Verdict)
	}
	for _, tg := range rc.Targets {
		if tg.Verdict == verify.OutcomeRegressed {
			t.Fatalf("target %d regressed alone; the fixture must need the pool", tg.QueryID)
		}
	}
	if !reflect.DeepEqual(rc.ContradictedBy, []string{"queryid:101"}) {
		t.Fatalf("contradicted_by %v, want the worse target 101", rc.ContradictedBy)
	}
}

func TestDecideLockHarmRegressesEvenWithCleanLatency(t *testing.T) {
	clean := Observations{Before: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)},
		After: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)}}
	cases := map[string]struct {
		obs  Observations
		want []string
	}{
		"incident": {withHarm(clean, []string{"7d0c"}, nil, nil), []string{"incident:7d0c"}},
		"finding":  {withHarm(clean, nil, []int64{42}, nil), []string{"finding:42"}},
		"over a confounder": {withHarm(clean, []string{"7d0c"}, nil,
			[]string{"restart at 2026-10-20T12:05:00Z"}), []string{"incident:7d0c"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := decidedReview(100)
			ev, err := changereview.NewEvidence("table", "rv.rv_orders", "rv_orders",
				map[string]any{"reltuples": 1})
			if err != nil {
				t.Fatal(err)
			}
			r.Evidence = []changereview.Evidence{ev}
			rc := Decide(r, windows, tc.obs)
			if rc.Verdict != changereview.ReceiptRegressed || rc.Reason != ReasonLock {
				t.Fatalf("verdict %s (%s), want regressed (lock)", rc.Verdict, rc.Reason)
			}
			if !reflect.DeepEqual(rc.ContradictedBy, tc.want) {
				t.Fatalf("contradicted_by %v, want %v", rc.ContradictedBy, tc.want)
			}
			if !contains(rc.EvidenceIDs, "table:rv.rv_orders") {
				t.Fatalf("evidence ids %v do not cite the touched table", rc.EvidenceIDs)
			}
		})
	}
}

func withHarm(o Observations, incidents []string, findings []int64,
	confounders []string) Observations {
	o.Incidents, o.LockFindings, o.Confounders = incidents, findings, confounders
	return o
}

func TestDecideConfoundedIsUnverifiable(t *testing.T) {
	r := decidedReview(100)
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)},
		After:       map[int64]verify.Measurement{100: meas(200, 30, 0.1, 20)},
		Confounders: []string{"stats_reset at 2026-10-20T08:00:00Z"}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptUnverifiable || rc.Reason != ReasonConfounded {
		t.Fatalf("verdict %s (%s), want unverifiable (confounded)", rc.Verdict, rc.Reason)
	}
	if !reflect.DeepEqual(rc.Confounders, obs.Confounders) {
		t.Fatalf("confounders %v, want %v", rc.Confounders, obs.Confounders)
	}
	if len(rc.ContradictedBy) != 0 {
		t.Fatalf("a confounded receipt blames %v; no penalty without a clean window",
			rc.ContradictedBy)
	}
}

func TestDecideToleranceAgainstAHypoPGPrediction(t *testing.T) {
	r := decidedReview(100)
	gain := -30.0
	r.Expectations.Predictions = []verify.Prediction{{Class: "change_review",
		Method: verify.MethodHypoPG, Metric: verify.MetricMeanExecTime,
		TargetQueryIDs: []int64{100}, ExpectedChangePct: &gain}}
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(500, 10, 0.05, 20)},
		After: map[int64]verify.Measurement{100: meas(500, 7, 0.05, 20)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptClean || rc.Tolerance != verify.ToleranceMet {
		t.Fatalf("verdict %s tolerance %s, want clean and met", rc.Verdict, rc.Tolerance)
	}
	if rc.Targets[0].Verdict != verify.OutcomeImproved {
		t.Fatalf("target verdict %q, want improved", rc.Targets[0].Verdict)
	}
}

// A statement that moved 12% (inside the regression bar) keeps the
// receipt clean but misses its no-change prediction.
func TestDecideCleanButToleranceMissed(t *testing.T) {
	r := decidedReview(100)
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(500, 10, 0.05, 20)},
		After: map[int64]verify.Measurement{100: meas(500, 11.2, 0.05, 20)}}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptClean || rc.Tolerance != verify.ToleranceMissed {
		t.Fatalf("verdict %s tolerance %s, want clean and missed", rc.Verdict, rc.Tolerance)
	}
}

func TestDecideListsBornQueriesWithoutAVerdict(t *testing.T) {
	r := decidedReview(100)
	born := []changereview.BornQuery{{QueryID: 900, FirstSeen: t0.Add(time.Hour),
		MeanMs: 4.5, Calls: 80, Relations: []string{"rv.rv_orders"}}}
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)},
		After: map[int64]verify.Measurement{100: meas(200, 10, 0.1, 20)}, Born: born}
	rc := Decide(r, windows, obs)
	if rc.Verdict != changereview.ReceiptClean {
		t.Fatalf("verdict %s: a born query must not change the verdict", rc.Verdict)
	}
	if !reflect.DeepEqual(rc.BornQueryIDs, born) {
		t.Fatalf("born %+v, want %+v", rc.BornQueryIDs, born)
	}
	for _, tg := range rc.Targets {
		if tg.QueryID == 900 {
			t.Fatal("a born query was judged as a target")
		}
	}
}

// Nil maps and an empty review decide without panicking.
func TestDecideZeroInputs(t *testing.T) {
	rc := Decide(changereview.Review{}, Windows{}, Observations{})
	if rc.Verdict != changereview.ReceiptCleanLowTraffic || rc.Targets == nil {
		t.Fatalf("zero inputs decided %+v", rc)
	}
	r := decidedReview(100)
	rc = Decide(r, windows, Observations{})
	if rc.Verdict != changereview.ReceiptCleanLowTraffic || len(rc.Targets) != 1 ||
		rc.Targets[0].BeforeCalls != 0 {
		t.Fatalf("unmeasured target decided %+v", rc)
	}
}

func TestWindowsOf(t *testing.T) {
	r := changereview.Review{LandingState: changereview.LandingLanded}
	lower, at := t0.Add(-10*time.Minute), t0
	r.LandedLower, r.LandedAt = &lower, &at
	w, err := WindowsOf(r, 24)
	if err != nil {
		t.Fatal(err)
	}
	want := Windows{BeforeFrom: lower.Add(-24 * time.Hour), BeforeTo: lower, AfterFrom: at,
		AfterTo: at.Add(24 * time.Hour)}
	if w != want {
		t.Fatalf("windows %+v, want %+v", w, want)
	}
	bad := map[string]func() (changereview.Review, int){
		"awaiting": func() (changereview.Review, int) {
			x := r
			x.LandingState = changereview.LandingAwaiting
			return x, 24
		},
		"no bracket": func() (changereview.Review, int) {
			x := r
			x.LandedLower = nil
			return x, 24
		},
		"zero hours":     func() (changereview.Review, int) { return r, 0 },
		"too many hours": func() (changereview.Review, int) { return r, 169 },
	}
	for name, mk := range bad {
		x, h := mk()
		if _, err := WindowsOf(x, h); !errors.Is(err, changereview.ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
}

func TestPValue(t *testing.T) {
	cases := []struct {
		t, df, want, tol float64
	}{{0, 10, 1, 1e-9}, {2.228, 10, 0.05, 5e-4}, {-2.228, 10, 0.05, 5e-4},
		{1.96, 1e6, 0.05, 5e-4}, {12.706, 1, 0.05, 5e-4}, {40, 30, 0, 1e-12}}
	for _, c := range cases {
		if got := pValue(c.t, c.df); math.Abs(got-c.want) > c.tol {
			t.Errorf("pValue(%v, %v) = %v, want %v", c.t, c.df, got, c.want)
		}
	}
	for _, df := range []float64{0, -1, math.NaN()} {
		if got := pValue(1, df); !math.IsNaN(got) {
			t.Errorf("pValue(1, %v) = %v, want NaN", df, got)
		}
	}
	if got := pValue(math.Inf(1), 5); got != 0 {
		t.Errorf("pValue(+Inf, 5) = %v, want 0", got)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
