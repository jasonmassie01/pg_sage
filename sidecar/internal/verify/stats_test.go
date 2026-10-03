package verify

import (
	"math"
	"testing"
	"time"
)

// No concurrent access tests: the statistics are pure functions of their
// arguments and share no state.

func steadyIntervals(n int, calls int64, meanMs float64) []Interval {
	out := make([]Interval, n)
	for i := range out {
		out[i] = Interval{Calls: calls, TotalMs: float64(calls) * meanMs}
	}
	return out
}

func TestSummarizeIsCallWeighted(t *testing.T) {
	// 10 calls at 1 ms and 90 calls at 11 ms: the call-weighted mean is
	// 10 ms, the unweighted mean of the interval means would be 6 ms.
	m := Summarize([]Interval{{Calls: 10, TotalMs: 10}, {Calls: 90, TotalMs: 990}})
	if m.Samples != 100 || m.Buckets != 2 {
		t.Fatalf("samples/buckets = %d/%d, want 100/2", m.Samples, m.Buckets)
	}
	if m.AverageLatency != 10*time.Millisecond {
		t.Fatalf("mean = %s, want 10ms (call-weighted)", m.AverageLatency)
	}
	if m.StdErr <= 0 {
		t.Fatalf("stderr = %s, want > 0 for differing interval means", m.StdErr)
	}
}

func TestSummarizeStdErrMatchesClusterFormula(t *testing.T) {
	intervals := []Interval{{10, 100}, {20, 400}, {30, 300}}
	// mean = 800/60; var = n/(n-1) * sum((T_i - mean*C_i)^2) / C^2
	mean := 800.0 / 60
	var ss float64
	for _, iv := range intervals {
		d := iv.TotalMs - mean*float64(iv.Calls)
		ss += d * d
	}
	wantMs := math.Sqrt(3.0 / 2.0 * ss / (60 * 60))
	got := Summarize(intervals)
	gotMs := float64(got.StdErr) / float64(time.Millisecond)
	if math.Abs(gotMs-wantMs) > 1e-3 {
		t.Fatalf("stderr = %.6f ms, want %.6f ms", gotMs, wantMs)
	}
}

func TestSummarizeIgnoresIdleAndInvalidIntervals(t *testing.T) {
	m := Summarize([]Interval{{Calls: 0, TotalMs: 0}, {Calls: -3, TotalMs: 5},
		{Calls: 4, TotalMs: -1}, {Calls: 5, TotalMs: 50}})
	if m.Samples != 5 || m.Buckets != 1 || m.AverageLatency != 10*time.Millisecond {
		t.Fatalf("summary = %+v, want only the one valid interval", m)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	for name, in := range map[string][]Interval{"nil": nil, "empty": {}} {
		m := Summarize(in)
		if m != (Measurement{}) {
			t.Fatalf("%s: summary = %+v, want zero measurement", name, m)
		}
	}
}

func TestSummarizeSingleBucketHasNoStdErr(t *testing.T) {
	m := Summarize(steadyIntervals(1, 100, 5))
	if m.Buckets != 1 || m.StdErr != 0 {
		t.Fatalf("single bucket = %+v, want buckets=1 stderr=0", m)
	}
}

func TestPoolCombinesCallWeighted(t *testing.T) {
	a := Measurement{Samples: 100, AverageLatency: 10 * time.Millisecond,
		StdErr: time.Millisecond, Buckets: 10}
	b := Measurement{Samples: 300, AverageLatency: 30 * time.Millisecond,
		StdErr: 2 * time.Millisecond, Buckets: 6}
	p := Pool([]Measurement{a, b})
	if p.Samples != 400 || p.AverageLatency != 25*time.Millisecond {
		t.Fatalf("pool = %+v, want 400 calls at 25ms", p)
	}
	// var = (100/400)^2 * 1 + (300/400)^2 * 4 = 0.0625 + 2.25
	wantSE := math.Sqrt(0.0625 + 2.25)
	gotSE := float64(p.StdErr) / float64(time.Millisecond)
	if math.Abs(gotSE-wantSE) > 1e-3 {
		t.Fatalf("pooled stderr = %.4f, want %.4f", gotSE, wantSE)
	}
	if p.Buckets != 6 {
		t.Fatalf("pooled buckets = %d, want the smallest (6)", p.Buckets)
	}
}

func TestPoolSkipsEmptyMeasurements(t *testing.T) {
	a := Measurement{Samples: 50, AverageLatency: 4 * time.Millisecond, Buckets: 5}
	p := Pool([]Measurement{{}, a, {}})
	if p.Samples != 50 || p.AverageLatency != 4*time.Millisecond || p.Buckets != 5 {
		t.Fatalf("pool = %+v, want only the measured query", p)
	}
	if got := Pool(nil); got != (Measurement{}) {
		t.Fatalf("pool(nil) = %+v, want zero", got)
	}
}

func TestTCriticalKnownValues(t *testing.T) {
	tests := []struct {
		df, alpha, want, tol float64
	}{
		{1, 0.05, 12.706, 0.01},
		{2, 0.05, 4.303, 0.01},
		{5, 0.05, 2.571, 0.02},
		{10, 0.05, 2.228, 0.01},
		{30, 0.05, 2.042, 0.01},
		{1e6, 0.05, 1.960, 0.01},
		{10, 0.01, 3.169, 0.03},
	}
	for _, tt := range tests {
		if got := TCritical(tt.df, tt.alpha); math.Abs(got-tt.want) > tt.tol {
			t.Errorf("TCritical(%v, %v) = %.4f, want %.3f", tt.df, tt.alpha, got, tt.want)
		}
	}
}

func TestTCriticalInvalidInputIsConservative(t *testing.T) {
	for _, df := range []float64{0, -1, math.NaN()} {
		if got := TCritical(df, 0.05); !math.IsInf(got, 1) {
			t.Errorf("TCritical(df=%v) = %v, want +Inf (never significant)", df, got)
		}
	}
	// A nonsense alpha falls back to 0.05.
	if got := TCritical(10, 0); math.Abs(got-2.228) > 0.01 {
		t.Errorf("TCritical(alpha=0) = %v, want the 0.05 value", got)
	}
}

func th() Thresholds {
	return Thresholds{MinSamples: 30, MinBuckets: 3, GainPct: 20, RegressPct: 15,
		Alpha: 0.05}
}

func meas(calls int, meanMs, seMs float64, buckets int) Measurement {
	return Measurement{Samples: calls,
		AverageLatency: time.Duration(meanMs * float64(time.Millisecond)),
		StdErr:         time.Duration(seMs * float64(time.Millisecond)), Buckets: buckets}
}

func TestCompareImproved(t *testing.T) {
	c := Compare(meas(500, 100, 2, 12), meas(500, 60, 2, 12), th())
	if c.Verdict != OutcomeImproved {
		t.Fatalf("verdict = %s (%s), want improved", c.Verdict, c.Reason)
	}
	if math.Abs(c.DeltaPct+40) > 1e-6 {
		t.Fatalf("delta = %v, want -40", c.DeltaPct)
	}
	if !(c.CIHighPct < 0) || !(c.CILowPct < c.CIHighPct) {
		t.Fatalf("CI = [%v, %v], want an interval below zero", c.CILowPct, c.CIHighPct)
	}
}

func TestCompareRegressed(t *testing.T) {
	c := Compare(meas(500, 100, 2, 12), meas(500, 150, 3, 12), th())
	if c.Verdict != OutcomeRegressed {
		t.Fatalf("verdict = %s (%s), want regressed", c.Verdict, c.Reason)
	}
	if c.T <= 0 || c.DF <= 0 {
		t.Fatalf("t/df = %v/%v, want a positive statistic", c.T, c.DF)
	}
}

func TestCompareNeutralWhenConfidentlyWithinTolerance(t *testing.T) {
	c := Compare(meas(5000, 100, 0.5, 48), meas(5000, 101, 0.5, 48), th())
	if c.Verdict != OutcomeNeutral {
		t.Fatalf("verdict = %s (%s), want neutral", c.Verdict, c.Reason)
	}
}

func TestCompareSignificantButSmallChangeIsNeutral(t *testing.T) {
	// -8% and significant, but below the 20% gain bar.
	c := Compare(meas(5000, 100, 0.5, 48), meas(5000, 92, 0.5, 48), th())
	if c.Verdict != OutcomeNeutral {
		t.Fatalf("verdict = %s, want neutral for a gain below the bar", c.Verdict)
	}
	// +12% and significant, below the 15% regression bar.
	c = Compare(meas(5000, 100, 0.5, 48), meas(5000, 112, 0.5, 48), th())
	if c.Verdict != OutcomeNeutral {
		t.Fatalf("verdict = %s, want neutral for a slowdown below the bar", c.Verdict)
	}
}

// Noisy samples must not flip the verdict: a large apparent change whose
// confidence interval spans zero is not evidence either way.
func TestCompareNoisySamplesDoNotFlipVerdict(t *testing.T) {
	cases := map[string]Measurement{
		"apparent regression": meas(400, 160, 60, 6),
		"apparent gain":       meas(400, 50, 40, 6),
	}
	for name, after := range cases {
		c := Compare(meas(400, 100, 30, 6), after, th())
		if c.Verdict == OutcomeRegressed || c.Verdict == OutcomeImproved {
			t.Fatalf("%s: noisy verdict = %s (delta %.1f%%, CI [%.1f, %.1f])", name,
				c.Verdict, c.DeltaPct, c.CILowPct, c.CIHighPct)
		}
		if c.Verdict != OutcomeInsufficient {
			t.Fatalf("%s: verdict = %s, want insufficient_evidence", name, c.Verdict)
		}
	}
}

func TestCompareInsufficientCalls(t *testing.T) {
	for name, pair := range map[string][2]Measurement{
		"before short": {meas(29, 100, 1, 12), meas(500, 50, 1, 12)},
		"after short":  {meas(500, 100, 1, 12), meas(29, 50, 1, 12)},
		"no data":      {{}, {}},
	} {
		c := Compare(pair[0], pair[1], th())
		if c.Verdict != OutcomeInsufficient {
			t.Fatalf("%s: verdict = %s, want insufficient_evidence", name, c.Verdict)
		}
	}
}

func TestCompareInsufficientBuckets(t *testing.T) {
	c := Compare(meas(500, 100, 0, 2), meas(500, 10, 0, 12), th())
	if c.Verdict != OutcomeInsufficient {
		t.Fatalf("verdict = %s, want insufficient_evidence with 2 buckets", c.Verdict)
	}
}

func TestCompareBoundaries(t *testing.T) {
	// Zero variance: significance is certain, the bars decide.
	cases := []struct {
		name  string
		after float64
		want  string
	}{
		{"gain exactly at bar", 80, OutcomeImproved},
		{"gain just below bar", 80.5, OutcomeNeutral},
		{"regression exactly at bar", 115, OutcomeNeutral},
		{"regression just above bar", 115.5, OutcomeRegressed},
	}
	for _, tc := range cases {
		c := Compare(meas(500, 100, 0, 12), meas(500, tc.after, 0, 12), th())
		if c.Verdict != tc.want {
			t.Errorf("%s: verdict = %s, want %s", tc.name, c.Verdict, tc.want)
		}
	}
}

func TestCompareZeroBaselineIsInsufficient(t *testing.T) {
	c := Compare(meas(500, 0, 0, 12), meas(500, 10, 0, 12), th())
	if c.Verdict != OutcomeInsufficient {
		t.Fatalf("verdict = %s, want insufficient_evidence for a zero baseline", c.Verdict)
	}
}

func TestCompareDefaultsZeroThresholds(t *testing.T) {
	// Zero thresholds must not mean "no bar": they take the defaults.
	c := Compare(meas(500, 100, 0, 12), meas(500, 99, 0, 12), Thresholds{})
	if c.Verdict != OutcomeNeutral {
		t.Fatalf("1%% change with default bars = %s, want neutral", c.Verdict)
	}
	c = Compare(meas(10, 100, 0, 12), meas(10, 10, 0, 12), Thresholds{})
	if c.Verdict != OutcomeInsufficient {
		t.Fatalf("10 calls with default MinSamples = %s, want insufficient", c.Verdict)
	}
}

func TestCompareRecordsMeasurements(t *testing.T) {
	before, after := meas(500, 100, 2, 12), meas(600, 60, 2, 10)
	c := Compare(before, after, th())
	if c.Before != before || c.After != after || c.Reason == "" {
		t.Fatalf("comparison lost its evidence: %+v", c)
	}
}
