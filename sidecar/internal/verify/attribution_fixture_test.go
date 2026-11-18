package verify

import (
	"math"
	"testing"
	"time"
)

// Fixtures of the attribution tests (lab-01a): deterministic per-bucket
// counters with a small multiplicative wobble, so every comparison has a
// finite, non-zero spread and a known true effect.

// stmt is one statement of a fixture: calls per bucket and per-call blocks
// and time before the step, and the post/pre multipliers after it (0 means
// unchanged). noise is the wobble's amplitude (0.02 is ±2%).
type stmt struct {
	id                    int64
	preCalls, postCalls   int64
	blocks, ms            float64
	blockFactor, msFactor float64
	noise                 float64
}

const fxWidth = 5 * time.Minute

// fxStep is the step of the plain fixtures, a Sunday noon (UTC).
var fxStep = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func factor(f float64) float64 {
	if f == 0 {
		return 1
	}
	return f
}

// wobble is a deterministic multiplier in [1-amp, 1+amp], different per
// statement, side and bucket.
func wobble(amp float64, id int64, side, i int) float64 {
	return 1 + amp*math.Sin(float64(i)*1.7+float64(id)*0.9+float64(side)*2.3)
}

// fxSide builds n buckets from start for every statement; post applies the
// statements' factors and post calls.
func fxSide(stmts []stmt, start time.Time, n int, post bool) Series {
	out := Series{}
	side := 0
	if post {
		side = 1
	}
	for _, s := range stmts {
		calls, bf, mf := s.preCalls, 1.0, 1.0
		if post {
			calls, bf, mf = s.postCalls, factor(s.blockFactor), factor(s.msFactor)
		}
		if calls == 0 {
			continue
		}
		for i := 0; i < n; i++ {
			b := s.blocks * bf * wobble(s.noise, s.id, side, i)
			m := s.ms * mf * wobble(s.noise, s.id, side+2, i)
			out[s.id] = append(out[s.id], Bucket{Start: start.Add(time.Duration(i) * fxWidth),
				Calls: calls, Blocks: b * float64(calls), TotalMs: m * float64(calls),
				Rows: calls})
		}
	}
	return out
}

// fxInput is an index-kind input with 24 buckets on each side of fxStep.
func fxInput(targets, controls []int64, stmts []stmt) AttributionInput {
	return AttributionInput{Kind: AttributionIndex, Targets: targets, Controls: controls,
		Pre:  fxSide(stmts, fxStep.Add(-24*fxWidth), 24, false),
		Post: fxSide(stmts, fxStep, 24, true), RegressPct: DefaultRegressPct}
}

// flatControls are three untouched statements.
func flatControls() []stmt {
	return []stmt{
		{id: 11, preCalls: 40, postCalls: 40, blocks: 50, ms: 2, noise: 0.02},
		{id: 12, preCalls: 30, postCalls: 30, blocks: 8, ms: 0.4, noise: 0.02},
		{id: 13, preCalls: 60, postCalls: 60, blocks: 300, ms: 9, noise: 0.02},
	}
}

func mustAttribute(t *testing.T, in AttributionInput) Attribution {
	t.Helper()
	a, err := Attribute(in)
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	return a
}

func within(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %.3f, want %.3f ± %.1f", name, got, want, tol)
	}
}

// A real 40% drop in one target's blocks per call, flat controls: the
// effect is -40% and the CI is below 0.
func TestAttribution_RealDropWithFlatControls(t *testing.T) {
	stmts := append(flatControls(), stmt{id: 1, preCalls: 50, postCalls: 50, blocks: 100,
		ms: 4, blockFactor: 0.6, msFactor: 0.6, noise: 0.02})
	a := mustAttribute(t, fxInput([]int64{1}, []int64{11, 12, 13}, stmts))
	if !a.Measured {
		t.Fatalf("not measured: %s", a.Reason)
	}
	within(t, "EffectPct", a.EffectPct, -40, 2)
	if !(a.CIHigh < 0) || !(a.CILow < a.EffectLog) || !(a.EffectLog < a.CIHigh) {
		t.Fatalf("CI [%.4f, %.4f] around %.4f must lie below 0", a.CILow, a.CIHigh,
			a.EffectLog)
	}
	if a.Method != MethodDiD || a.Basis != BasisPreWindow || a.Controls != 3 {
		t.Fatalf("method %q basis %q controls %d, want did, pre_window, 3", a.Method,
			a.Basis, a.Controls)
	}
	within(t, "EffectPct vs exp(EffectLog)-1", a.EffectPct, (math.Exp(a.EffectLog)-1)*100,
		1e-9)
	within(t, "ControlLog", a.ControlLog, 0, 0.02)
	within(t, "TreatedLog", a.TreatedLog, math.Log(0.6), 0.02)
	if !a.Guardrail.Measured || a.Guardrail.Regressed {
		t.Fatalf("guardrail %+v: time fell, must be measured and not a regression",
			a.Guardrail)
	}
}

// trafficShift moves the call mix 80% toward a cheap target with no
// change in any query.
func trafficShift() []stmt {
	return append(flatControls(),
		stmt{id: 1, preCalls: 50, postCalls: 10, blocks: 200, ms: 10, noise: 0.02},
		stmt{id: 2, preCalls: 50, postCalls: 90, blocks: 10, ms: 0.5, noise: 0.02})
}

func toIntervals(bs []Bucket) []Interval {
	out := make([]Interval, len(bs))
	for i, b := range bs {
		out[i] = Interval{Calls: b.Calls, TotalMs: b.TotalMs}
	}
	return out
}

// v1 Pool weights each side by its own calls, so the shift reads as a
// gain (pinned: the bug v2 fixes); v2's frozen weights see no change.
func TestAttribution_TrafficShiftIsNoChange(t *testing.T) {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	before := Pool([]Measurement{Summarize(toIntervals(in.Pre[1])),
		Summarize(toIntervals(in.Pre[2]))})
	after := Pool([]Measurement{Summarize(toIntervals(in.Post[1])),
		Summarize(toIntervals(in.Post[2]))})
	v1 := Compare(before, after, Thresholds{})
	if v1.Verdict != OutcomeImproved || v1.DeltaPct > -50 {
		t.Fatalf("v1 pin: Pool must show the mix shift as a gain, got %s %+.1f%%",
			v1.Verdict, v1.DeltaPct)
	}
	a := mustAttribute(t, in)
	if !a.Measured {
		t.Fatalf("not measured: %s", a.Reason)
	}
	within(t, "EffectPct", a.EffectPct, 0, 3)
	within(t, "weight of query 1", a.Weights[1], 0.5, 1e-9)
	within(t, "weight of query 2", a.Weights[2], 0.5, 1e-9)
}

// CHECK-02: the same fixture judged with post-period weights (the
// test-only switch) shows a large false gain, so the frozen weights are
// what keeps the traffic shift from reading as an improvement.
func TestAttribution_PostPeriodWeightsWouldFakeAGain(t *testing.T) {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	in.postWeights = true
	a := mustAttribute(t, in)
	if !a.Measured || a.EffectPct > -50 {
		t.Fatalf("post-period weights: EffectPct %.1f (measured %v), want a false gain "+
			"below -50%%", a.EffectPct, a.Measured)
	}
}

// Every statement's blocks and time rise 30% at the step (a deploy or a
// noisy neighbour): treated minus control is no change.
func TestAttribution_NoisyNeighbourIsNoChange(t *testing.T) {
	stmts := append(flatControls(), stmt{id: 1, preCalls: 50, postCalls: 50, blocks: 100,
		ms: 4, noise: 0.02})
	for i := range stmts {
		stmts[i].blockFactor, stmts[i].msFactor = 1.3, 1.3
	}
	a := mustAttribute(t, fxInput([]int64{1}, []int64{11, 12, 13}, stmts))
	if !a.Measured {
		t.Fatalf("not measured: %s", a.Reason)
	}
	within(t, "EffectPct", a.EffectPct, 0, 3)
	within(t, "TreatedLog", a.TreatedLog, math.Log(1.3), 0.02)
	within(t, "ControlLog", a.ControlLog, math.Log(1.3), 0.02)
	if a.Guardrail.Regressed {
		t.Fatalf("guardrail %+v: a rise shared by the controls is not the step's",
			a.Guardrail)
	}
}

// The seasonal fixture: a target whose Monday 09:00 is always 2x its
// Sunday 22:00. The step is Sunday 23:00; the pre-window is Sunday
// 22:00-23:00 and the judged window Monday 09:00-10:00. weeks earlier
// weeks of hourly history hold Monday 09:00 at 2x.
var (
	seasonStep = time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC) // a Sunday
	seasonPost = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)  // the Monday after
)

func seasonalInput(weeks int) AttributionInput {
	pre := []stmt{{id: 1, preCalls: 50, blocks: 100, ms: 4, noise: 0.02}}
	post := []stmt{{id: 1, preCalls: 50, blocks: 200, ms: 8, noise: 0.02}}
	hist := Series{}
	for k := 1; k <= weeks; k++ {
		w := wobble(0.01, 1, 5, k)
		hist[1] = append(hist[1], Bucket{Start: seasonPost.Add(-time.Duration(k) * week),
			Calls: 600, Blocks: 200 * 600 * w, TotalMs: 8 * 600 * w, Rows: 600})
	}
	return AttributionInput{Kind: AttributionIndex, Targets: []int64{1},
		Pre:  fxSide(pre, seasonStep.Add(-time.Hour), 12, false),
		Post: fxSide(post, seasonPost, 12, false), Seasonal: hist,
		RegressPct: DefaultRegressPct}
}

func TestAttribution_SeasonalMatch(t *testing.T) {
	a := mustAttribute(t, seasonalInput(4))
	if !a.Measured || a.Basis != BasisSeasonal || a.SeasonalWeeks != 4 {
		t.Fatalf("4 weeks: measured %v basis %q weeks %d (%s), want seasonal over 4",
			a.Measured, a.Basis, a.SeasonalWeeks, a.Reason)
	}
	within(t, "EffectPct", a.EffectPct, 0, 3)
	within(t, "SeasonalLog", a.SeasonalLog, 0, 0.03)
	if a.Guardrail.Regressed {
		t.Fatalf("guardrail %+v: Monday's usual 2x is not a regression", a.Guardrail)
	}
}

func TestAttribution_SeasonalFallsBackToPreWindow(t *testing.T) {
	a := mustAttribute(t, seasonalInput(2))
	if !a.Measured || a.Basis != BasisPreWindow || a.SeasonalWeeks != 2 {
		t.Fatalf("2 weeks: measured %v basis %q weeks %d, want pre_window with 2 weeks",
			a.Measured, a.Basis, a.SeasonalWeeks)
	}
	within(t, "EffectPct", a.EffectPct, 100, 5)
	if !a.Guardrail.Regressed {
		t.Fatalf("guardrail %+v: against the pre-window time per call doubled",
			a.Guardrail)
	}
}
