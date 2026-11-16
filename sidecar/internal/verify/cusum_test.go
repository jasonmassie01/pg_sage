package verify

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// Contamination detection (lab-01b): a two-sided CUSUM on each target's
// and the control median's residual. A changepoint more than one collector
// cycle away from the step is an unrelated break: the window is
// contaminated. Pure functions: concurrency is covered by the package's
// -race run, with no shared state to race on.

var cusumStep = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

// cusumFixture is 24 hourly pre buckets and n post buckets of target 1
// (blocks halved at the step) and controls 11-13 (flat), with 3% log
// noise from seed; from post bucket breakAt (never when < 0) every
// statement's blocks are multiplied by breakFactor (a deploy).
func cusumFixture(seed uint64, n, breakAt int, breakFactor float64) AttributionInput {
	return noisyCusumFixture(seed, n, breakAt, breakFactor, 0.03)
}

// noisyCusumFixture is cusumFixture with noise as the log noise's sd.
func noisyCusumFixture(seed uint64, n, breakAt int, breakFactor,
	noise float64) AttributionInput {
	r := rand.New(rand.NewPCG(seed, 7))
	pre, post := Series{}, Series{}
	bucket := func(at time.Time, f float64) Bucket {
		return Bucket{Start: at, Calls: 100, TotalMs: 200 * math.Exp(noise*r.NormFloat64()),
			Blocks: 5000 * f * math.Exp(noise*r.NormFloat64()), Rows: 100}
	}
	for _, id := range []int64{1, 11, 12, 13} {
		for i := 0; i < 24; i++ {
			pre[id] = append(pre[id], bucket(cusumStep.Add(time.Duration(i-24)*time.Hour), 1))
		}
		for i := 0; i < n; i++ {
			f := 1.0
			if id == 1 {
				f = 0.5
			}
			if breakAt >= 0 && i >= breakAt {
				f *= breakFactor
			}
			post[id] = append(post[id], bucket(cusumStep.Add(time.Duration(i)*time.Hour), f))
		}
	}
	return AttributionInput{Kind: AttributionIndex, Targets: []int64{1},
		Controls: []int64{11, 12, 13}, Pre: pre, Post: post}
}

// Boundary: a CUSUM statistic of exactly h does not fire; above h does.
// With center 0 and scale 1 one bucket of 5.5 adds 5.5-k = 5.0 = h.
func TestCusumStatisticExactlyHDoesNotFire(t *testing.T) {
	for _, sign := range []float64{1, -1} {
		_, stat, fired := cusumRun([]float64{sign * 5.5}, 0, 1)
		if fired || stat != CusumH {
			t.Errorf("sign %+v: statistic %v fired=%v, want exactly h and no alarm", sign,
				stat, fired)
		}
		start, stat, fired := cusumRun([]float64{0, sign * 5.500001}, 0, 1)
		if !fired || !(stat > CusumH) || start != 1 {
			t.Errorf("sign %+v: statistic %v fired=%v start=%d, want an alarm at 1", sign,
				stat, fired, start)
		}
	}
}

func hourlyPoints(vals []float64) []Point {
	out := make([]Point, len(vals))
	for i, v := range vals {
		out[i] = Point{At: cusumStep.Add(time.Duration(i) * time.Hour), V: v}
	}
	return out
}

func levelSeries(n, breakAt int, before, after float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = before + 0.01*math.Sin(float64(i)*1.3)
		if i >= breakAt {
			out[i] = after + 0.01*math.Sin(float64(i)*1.3)
		}
	}
	return out
}

// Happy path: a break early in the window (the minority first) and late
// in it (the majority first) are both located at their bucket.
func TestCusumScanChangepointLocatesEarlyAndLateBreaks(t *testing.T) {
	ref := []float64{0.01, -0.01, 0.02, -0.02, 0, 0.015, -0.015}
	for _, breakAt := range []int{5, 18} {
		cp := ScanChangepoint(hourlyPoints(levelSeries(24, breakAt, 0, 0.4)), ref)
		want := cusumStep.Add(time.Duration(breakAt) * time.Hour)
		if !cp.Scanned || !cp.Fired || !cp.At.Equal(want) {
			t.Errorf("break at %d: %+v, want a changepoint at %s", breakAt, cp, want)
		}
		if !(cp.Stat > CusumH) || !(cp.Scale > 0) {
			t.Errorf("break at %d: statistic %v scale %v", breakAt, cp.Stat, cp.Scale)
		}
	}
	flat := ScanChangepoint(hourlyPoints(levelSeries(24, 99, 0, 0)), ref)
	if !flat.Scanned || flat.Fired || !flat.At.IsZero() {
		t.Fatalf("flat series fired: %+v", flat)
	}
}

// Nil/empty: fewer than 3 buckets is a typed reason and no scan.
func TestCusumScanChangepointNeedsThreeBuckets(t *testing.T) {
	for _, pts := range [][]Point{nil, hourlyPoints([]float64{1, 2})} {
		cp := ScanChangepoint(pts, []float64{0, 0, 0})
		if cp.Scanned || cp.Fired || cp.Reason != ReasonCusumFewBuckets {
			t.Errorf("%d points: %+v, want reason %q", len(pts), cp, ReasonCusumFewBuckets)
		}
	}
	cp := ScanChangepoint(hourlyPoints([]float64{0, 0, 0}), nil)
	if !cp.Scanned || cp.Fired {
		t.Fatalf("three flat points without a reference: %+v", cp)
	}
}

// Happy path: a deploy at hour 5 of a 24 h window is named, within one
// bucket, and the next look starts after it.
func TestCusumDetectContaminationNamesTheDeployHour(t *testing.T) {
	c, err := DetectContamination(cusumFixture(1, 24, 5, 1.6), cusumStep, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := cusumStep.Add(5 * time.Hour)
	if !c.Contaminated || c.Outcome != OutcomeInsufficient {
		t.Fatalf("deploy at hour 5 not detected: %+v", c)
	}
	if d := c.At.Sub(want); d < -time.Hour || d > time.Hour {
		t.Fatalf("contaminated at %s, want %s plus or minus one bucket", c.At, want)
	}
	if c.Reason != ReasonContaminatedAt+c.At.Format(time.RFC3339) {
		t.Fatalf("reason %q, want contaminated_at=<time>", c.Reason)
	}
	if c.NextStart.Before(c.At) || !c.NextStart.After(cusumStep) {
		t.Fatalf("next look starts %s, want at or after %s", c.NextStart, c.At)
	}
	if len(c.Scans) != 2 || c.Scans[0].Series != "query 1" || c.Scans[1].Series != "controls" {
		t.Fatalf("scans %+v, want query 1 and the control median", c.Scans)
	}
	if e := c.Evidence(); e["contaminated"] != true || e["reason"] != c.Reason {
		t.Fatalf("evidence %v", e)
	}
}

// The step's own effect is a change at the step, not contamination.
func TestCusumDetectContaminationIgnoresTheStepItself(t *testing.T) {
	c, err := DetectContamination(cusumFixture(2, 24, -1, 1), cusumStep, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c.Contaminated || c.Outcome != "" || c.Reason != "" {
		t.Fatalf("a clean window with a step effect was contaminated: %+v", c)
	}
	if len(c.Scans) != 2 || !c.Scans[0].Scanned || !c.Scans[1].Scanned {
		t.Fatalf("scans %+v, want both series scanned", c.Scans)
	}
}

// Boundary: a changepoint exactly one cycle after the step is the step
// settling; two cycles after is contamination.
func TestCusumDetectContaminationCycleBoundary(t *testing.T) {
	for _, tc := range []struct {
		breakAt      int
		contaminated bool
	}{{1, false}, {2, true}} {
		c, err := DetectContamination(cusumFixture(3, 24, tc.breakAt, 1.6), cusumStep,
			time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if c.Contaminated != tc.contaminated {
			t.Errorf("break at bucket %d with an hourly cycle: contaminated=%v, want %v (%+v)",
				tc.breakAt, c.Contaminated, tc.contaminated, c.Scans)
		}
	}
}

// The share of clean windows marked contaminated, reported: expected
// under 10% on fixtures without an injected break, with hour-to-hour
// noise under the 5% scale floor (3%) and above it (8%).
func TestCusumDetectContaminationNullShare(t *testing.T) {
	const runs = 400
	for _, noise := range []float64{0.03, 0.08} {
		marked := 0
		for seed := uint64(100); seed < 100+runs; seed++ {
			c, err := DetectContamination(noisyCusumFixture(seed, 24, -1, 1, noise),
				cusumStep, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if c.Contaminated {
				marked++
			}
		}
		share := float64(marked) / runs
		t.Logf("contaminated share on %d clean 24 h windows, %.0f%% noise: %.2f%%", runs,
			100*noise, 100*share)
		if share >= 0.10 {
			t.Errorf("%.0f%% noise: clean windows marked contaminated: %.2f%%, want under "+
				"10%%", 100*noise, 100*share)
		}
	}
}

// A deploy is found through noise above the scale floor too.
func TestCusumDetectContaminationNamesTheDeployThroughNoise(t *testing.T) {
	c, err := DetectContamination(noisyCusumFixture(9, 24, 5, 2, 0.08), cusumStep,
		time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := cusumStep.Add(5 * time.Hour)
	if d := c.At.Sub(want); !c.Contaminated || d < -time.Hour || d > time.Hour {
		t.Fatalf("deploy at hour 5 through 8%% noise: %+v", c)
	}
}

// Nil/empty: no targets and too few post buckets give typed reasons, never
// a contamination verdict.
func TestCusumDetectContaminationEmptyInputs(t *testing.T) {
	in := cusumFixture(4, 24, -1, 1)
	in.Targets = nil
	c, err := DetectContamination(in, cusumStep, time.Minute)
	if err != nil || c.Contaminated || c.Outcome != "" || c.Reason != ReasonNoTargets {
		t.Fatalf("no targets: %+v, %v", c, err)
	}
	c, err = DetectContamination(cusumFixture(5, 2, -1, 1), cusumStep, time.Minute)
	if err != nil || c.Contaminated || c.Outcome != "" {
		t.Fatalf("two post buckets: %+v, %v", c, err)
	}
	if !strings.Contains(c.Reason, ReasonCusumFewBuckets) {
		t.Fatalf("two post buckets: reason %q, want %q", c.Reason, ReasonCusumFewBuckets)
	}
	in = cusumFixture(6, 24, -1, 1)
	in.EpochBreak = true
	if c, err = DetectContamination(in, cusumStep, time.Minute); err != nil ||
		c.Contaminated || c.Reason != ReasonEpochBreak {
		t.Fatalf("epoch break: %+v, %v", c, err)
	}
}

// Invalid input: an unknown kind, a target as its own control, a missing
// step and a negative cycle are caller errors.
func TestCusumDetectContaminationRefusesBadInput(t *testing.T) {
	in := cusumFixture(7, 24, -1, 1)
	in.Kind = "nonsense"
	if _, err := DetectContamination(in, cusumStep, time.Minute); err == nil {
		t.Error("unknown kind accepted")
	}
	in = cusumFixture(7, 24, -1, 1)
	in.Controls = append(in.Controls, 1)
	if _, err := DetectContamination(in, cusumStep, time.Minute); err == nil {
		t.Error("target as its own control accepted")
	}
	if _, err := DetectContamination(cusumFixture(7, 24, -1, 1), time.Time{},
		time.Minute); err == nil {
		t.Error("zero step accepted")
	}
	if _, err := DetectContamination(cusumFixture(7, 24, -1, 1), cusumStep,
		-time.Minute); err == nil {
		t.Error("negative cycle accepted")
	}
}

// A global setting has no control group: only the targets are scanned.
func TestCusumDetectContaminationGlobalScansTargetsOnly(t *testing.T) {
	in := cusumFixture(8, 24, 5, 1.6)
	in.Kind, in.Controls = AttributionGlobal, nil
	c, err := DetectContamination(in, cusumStep, time.Minute)
	if err != nil || !c.Contaminated || len(c.Scans) != 1 {
		t.Fatalf("global setting: %+v, %v", c, err)
	}
}

// Boundary: a moderate break (1.3x) through 8% noise is still found; a 3%
// change inside 8% noise is not a break.
func TestCusumDetectContaminationSensitivity(t *testing.T) {
	c, err := DetectContamination(noisyCusumFixture(10, 24, 12, 1.3, 0.08), cusumStep,
		time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d := c.At.Sub(cusumStep.Add(12 * time.Hour)); !c.Contaminated || d < -time.Hour ||
		d > time.Hour {
		t.Fatalf("1.3x break at hour 12 through 8%% noise: %+v", c)
	}
	c, err = DetectContamination(noisyCusumFixture(10, 24, 12, 1.03, 0.08), cusumStep,
		time.Minute)
	if err != nil || c.Contaminated {
		t.Fatalf("a 3%% change inside 8%% noise was a break: %+v, %v", c, err)
	}
}

// The confirmation's standard error: medians of 3 and 3 values one scale
// apart are 1/(1.2533 x sqrt(2/3)) standard errors apart.
func TestCusumLevelShiftStandardError(t *testing.T) {
	shift, z := levelShift([]float64{0, 0, 0, 1, 1, 1}, 3, 1)
	if shift != 1 || math.Abs(z-1/(1.2533*math.Sqrt(2.0/3))) > 1e-12 {
		t.Fatalf("levelShift = %v, %v", shift, z)
	}
	cp := ScanChangepoint(hourlyPoints([]float64{0, 0, 0, 0.6, 0.6, 0.6}), nil)
	if cp.Fired || !(math.Abs(cp.ShiftZ) < CusumConfirmZ) {
		t.Fatalf("an alarm short of %v standard errors fired: %+v", CusumConfirmZ, cp)
	}
}

// Invalid input: a NaN or infinite bucket is left out of the scan. Go's max
// returns NaN when an argument is NaN, so a NaN kept in the CUSUM would
// poison every later statistic and hide the break after it; with fewer
// than three finite buckets left there is no scan.
func TestCusumScanChangepointSkipsNonFiniteBuckets(t *testing.T) {
	ref := []float64{0.01, -0.01, 0.02, -0.02, 0, 0.015, -0.015}
	pts := hourlyPoints(levelSeries(24, 18, 0, 0.4))
	pts[3].V, pts[9].V = math.NaN(), math.Inf(1)
	cp := ScanChangepoint(pts, ref)
	want := cusumStep.Add(18 * time.Hour)
	if !cp.Scanned || !cp.Fired || !cp.At.Equal(want) {
		t.Fatalf("break at 18 behind a NaN bucket: %+v, want a changepoint at %s", cp, want)
	}
	few := hourlyPoints([]float64{0, math.NaN(), 0.4, math.Inf(-1)})
	if cp := ScanChangepoint(few, ref); cp.Scanned || cp.Reason != ReasonCusumFewBuckets {
		t.Fatalf("two finite buckets: %+v, want reason %q", cp, ReasonCusumFewBuckets)
	}
}
