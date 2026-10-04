package tuning

import (
	"math"
	"testing"
)

// Calibrated confidence (owner decision 4): per action class and
// prediction method, predicted improvement is mapped to what the outcome
// ledger observed. With too few outcomes the answer is "uncalibrated":
// confidence is never invented.

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

func TestCalibrate_NoOutcomesIsUncalibrated(t *testing.T) {
	cal := Calibrate(nil, 5)
	if len(cal.Classes) != 0 || cal.MinOutcomes != 5 {
		t.Fatalf("calibration = %+v", cal)
	}
	c := cal.ConfidenceFor("index_create", "hypopg", -40)
	if c.Status != StatusUncalibrated || c.Value != nil || c.N != 0 || c.Basis != "" {
		t.Fatalf("confidence = %+v, want uncalibrated with no value", c)
	}
}

func TestCalibrate_BelowTheMinimumIsUncalibrated(t *testing.T) {
	cal := Calibrate(improvedOutcomes("index_create", "hypopg", -40, 4, 4), 5)
	c := cal.ConfidenceFor("index_create", "hypopg", -40)
	if c.Status != StatusUncalibrated || c.Value != nil {
		t.Fatalf("4 outcomes < 5: %+v", c)
	}
	if c.N != 4 {
		t.Fatalf("an uncalibrated answer still reports what it has: N = %d", c.N)
	}
}

func TestCalibrate_ExactlyTheMinimumInOneBin(t *testing.T) {
	cal := Calibrate(improvedOutcomes("index_create", "hypopg", -30, 5, 3), 5)
	c := cal.ConfidenceFor("index_create", "hypopg", -35)
	if c.Status != StatusCalibrated || c.Basis != BasisBin || c.Bin != "25-50" {
		t.Fatalf("confidence = %+v, want calibrated on bin 25-50", c)
	}
	if c.Value == nil || !near(*c.Value, 0.6) || c.N != 5 || c.Hits != 3 {
		t.Fatalf("value = %v, n %d hits %d; want 3/5", c.Value, c.N, c.Hits)
	}
	if !near(c.WilsonLow, 0.2307) || !near(c.WilsonHigh, 0.8824) {
		t.Fatalf("wilson = [%v, %v], want [0.2307, 0.8824]", c.WilsonLow, c.WilsonHigh)
	}
}

func TestCalibrate_OtherBinFallsBackToTheClassMethodPool(t *testing.T) {
	cal := Calibrate(improvedOutcomes("index_create", "hypopg", -30, 6, 6), 5)
	c := cal.ConfidenceFor("index_create", "hypopg", -80)
	if c.Status != StatusCalibrated || c.Basis != BasisClassMethod || c.Bin != "50+" {
		t.Fatalf("confidence = %+v, want calibrated on the class/method pool", c)
	}
	if c.Value == nil || *c.Value != 1 || c.N != 6 {
		t.Fatalf("pool = %v n %d", c.Value, c.N)
	}
}

func TestCalibrate_AllWrongIsCalibratedAtZero(t *testing.T) {
	samples := improvedOutcomes("guc", "model", -50, 5, 0)
	samples[0].Verdict = "regressed"
	cal := Calibrate(samples, 5)
	c := cal.ConfidenceFor("guc", "model", -50)
	if c.Status != StatusCalibrated || c.Value == nil || *c.Value != 0 {
		t.Fatalf("all wrong is evidence too: %+v", c)
	}
	if c.WilsonLow != 0 || !near(c.WilsonHigh, 0.4345) {
		t.Fatalf("wilson = [%v, %v], want [0, 0.4345]", c.WilsonLow, c.WilsonHigh)
	}
	cc := cal.Classes[0]
	if cc.Regressed != 1 || cc.Hits != 0 || cc.N != 5 {
		t.Fatalf("class = %+v", cc)
	}
}

func TestCalibrate_AllRightWilsonBounds(t *testing.T) {
	cal := Calibrate(improvedOutcomes("index_create", "hypopg", -40, 5, 5), 5)
	c := cal.ConfidenceFor("index_create", "hypopg", -40)
	if *c.Value != 1 || !near(c.WilsonLow, 0.5655) || c.WilsonHigh != 1 {
		t.Fatalf("5/5: value %v wilson [%v, %v]", *c.Value, c.WilsonLow, c.WilsonHigh)
	}
}

func TestCalibrate_OnlyDecidedPredictedOutcomesCount(t *testing.T) {
	samples := improvedOutcomes("index_create", "hypopg", -40, 5, 5)
	for _, v := range []string{"insufficient_evidence", "unverifiable", "pending", "bogus"} {
		samples = append(samples, OutcomeSample{Class: "index_create", Method: "hypopg",
			PredictedPct: -40, Verdict: v})
	}
	samples = append(samples, OutcomeSample{Class: "index_create", Method: "none",
		Verdict: "improved"})
	cal := Calibrate(samples, 5)
	if cal.Excluded != 5 {
		t.Fatalf("excluded = %d, want 5 (4 undecided + 1 without a prediction)",
			cal.Excluded)
	}
	if len(cal.Classes) != 1 || cal.Classes[0].N != 5 {
		t.Fatalf("classes = %+v", cal.Classes)
	}
}

func TestCalibrate_BinBoundaries(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		want string
	}{{0, "0-10"}, {-9.999, "0-10"}, {10, "10-25"}, {-10, "10-25"}, {24.99, "10-25"},
		{-25, "25-50"}, {-49.99, "25-50"}, {50, "50+"}, {-50, "50+"}, {-150, "50+"}} {
		if got := binLabel(tc.pct); got != tc.want {
			t.Errorf("binLabel(%v) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

func TestCalibrate_MethodsAndClassesAreNotPooled(t *testing.T) {
	samples := append(improvedOutcomes("index_create", "hypopg", -40, 5, 5),
		improvedOutcomes("index_create", "model", -40, 5, 0)...)
	samples = append(samples, improvedOutcomes("guc", "hypopg", -40, 5, 1)...)
	cal := Calibrate(samples, 5)
	if len(cal.Classes) != 3 {
		t.Fatalf("classes = %d, want 3 (class x method)", len(cal.Classes))
	}
	if v := *cal.ConfidenceFor("index_create", "hypopg", -40).Value; v != 1 {
		t.Fatalf("hypopg = %v", v)
	}
	if v := *cal.ConfidenceFor("index_create", "model", -40).Value; v != 0 {
		t.Fatalf("model = %v", v)
	}
	if v := *cal.ConfidenceFor("guc", "hypopg", -40).Value; !near(v, 0.2) {
		t.Fatalf("guc = %v", v)
	}
	// Deterministic order: class, then method.
	if cal.Classes[0].Class != "guc" || cal.Classes[1].Method != "hypopg" ||
		cal.Classes[2].Method != "model" {
		t.Fatalf("order = %+v", cal.Classes)
	}
}

func TestCalibrate_ReliabilityBins(t *testing.T) {
	obs := func(v float64) *float64 { return &v }
	samples := []OutcomeSample{
		{Class: "index_create", Method: "hypopg", PredictedPct: -40, Verdict: "improved",
			Tolerance: "met", ObservedPct: obs(-30)},
		{Class: "index_create", Method: "hypopg", PredictedPct: -30, Verdict: "improved",
			Tolerance: "partial", ObservedPct: obs(-10)},
		{Class: "index_create", Method: "hypopg", PredictedPct: -30, Verdict: "neutral",
			Tolerance: "missed", ObservedPct: obs(2)},
		{Class: "index_create", Method: "hypopg", PredictedPct: -5, Verdict: "regressed",
			Tolerance: "missed", ObservedPct: obs(20)},
	}
	cc := Calibrate(samples, 1).Classes[0]
	if len(cc.Bins) != 4 {
		t.Fatalf("bins = %d, want all four in order", len(cc.Bins))
	}
	b := cc.Bins[2] // 25-50
	if b.Label != "25-50" || b.N != 3 || b.Hits != 2 || b.ToleranceMet != 1 {
		t.Fatalf("bin 25-50 = %+v", b)
	}
	// Predicted and observed are improvements (a fall of the metric is
	// positive): predicted mean (40+30+30)/3, observed (30+10-2)/3.
	if !near(b.MeanPredicted, 100.0/3) || !near(b.MeanObserved, 38.0/3) {
		t.Fatalf("means = %v / %v", b.MeanPredicted, b.MeanObserved)
	}
	if lo := cc.Bins[0]; lo.N != 1 || lo.Regressed != 1 || !near(lo.MeanObserved, -20) {
		t.Fatalf("bin 0-10 = %+v", lo)
	}
	if empty := cc.Bins[3]; empty.N != 0 || empty.Rate != 0 || empty.WilsonHigh != 0 {
		t.Fatalf("an empty bin reports zeros, not NaN: %+v", empty)
	}
}

func TestCalibrate_MinimumBelowOneMeansTheDefault(t *testing.T) {
	cal := Calibrate(improvedOutcomes("guc", "model", -40, 4, 4), 0)
	if cal.MinOutcomes != DefaultMinOutcomes {
		t.Fatalf("min = %d, want the default %d", cal.MinOutcomes, DefaultMinOutcomes)
	}
	if c := cal.ConfidenceFor("guc", "model", -40); c.Status != StatusUncalibrated {
		t.Fatalf("4 outcomes below the default 5: %+v", c)
	}
}

func TestWilson(t *testing.T) {
	lo, hi := wilson(3, 10)
	if !near(lo, 0.1078) || !near(hi, 0.6032) {
		t.Fatalf("wilson(3,10) = [%v, %v]", lo, hi)
	}
	if lo, hi := wilson(0, 0); lo != 0 || hi != 0 {
		t.Fatalf("wilson(0,0) = [%v, %v], want zeros", lo, hi)
	}
}

// No concurrent-access test: Calibrate and ConfidenceFor are pure.
