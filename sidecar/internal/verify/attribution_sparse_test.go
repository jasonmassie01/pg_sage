package verify

import (
	"testing"
	"time"
)

// A target that runs in only some buckets (a low-traffic statement at a
// fine bucket width) must not move the pooled metric: the frozen weights
// fix the mix only if a missing statement leaves both sides of a bucket.

// everyOther keeps the even buckets of bs.
func everyOther(bs []Bucket) []Bucket {
	var out []Bucket
	for i, b := range bs {
		if i%2 == 0 {
			out = append(out, b)
		}
	}
	return out
}

func TestAttribution_TargetMissingFromBucketsIsNoChange(t *testing.T) {
	for _, sparse := range []int64{1, 2} { // the expensive target, then the cheap one
		in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
		in.Post[sparse] = everyOther(in.Post[sparse])
		a := mustAttribute(t, in)
		if !a.Measured {
			t.Fatalf("query %d sparse: not measured: %s", sparse, a.Reason)
		}
		within(t, "EffectPct", a.EffectPct, 0, 3)
		within(t, "TreatedLog", a.TreatedLog, 0, 0.03)
		if a.Guardrail.Regressed || !a.Guardrail.Measured {
			t.Fatalf("query %d sparse: guardrail %+v, want measured and no regression",
				sparse, a.Guardrail)
		}
	}
}

// A real 40% drop in the expensive target still shows when the cheap one
// runs in only half the post buckets.
func TestAttribution_RealDropSurvivesSparseTarget(t *testing.T) {
	stmts := trafficShift()
	stmts[len(stmts)-2].blockFactor = 0.6 // query 1, the expensive target
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, stmts)
	dense := mustAttribute(t, in)
	in.Post[2] = everyOther(in.Post[2])
	a := mustAttribute(t, in)
	if !a.Measured || !(a.CIHigh < 0) {
		t.Fatalf("measured %v CI [%.3f, %.3f]: want a gain with its CI below 0 (%s)",
			a.Measured, a.CILow, a.CIHigh, a.Reason)
	}
	if !(a.EffectPct < -10) || !(dense.EffectPct < -10) {
		t.Fatalf("EffectPct sparse %.1f dense %.1f, want both a clear gain", a.EffectPct,
			dense.EffectPct)
	}
}

// The same on the seasonal basis: two targets at different levels with
// four weeks of history, one of them missing from half the judged buckets.
func TestAttribution_SeasonalSparseTargetIsNoChange(t *testing.T) {
	stmts := []stmt{{id: 1, preCalls: 50, blocks: 200, ms: 8, noise: 0.02},
		{id: 2, preCalls: 50, blocks: 10, ms: 0.5, noise: 0.02}}
	in := AttributionInput{Kind: AttributionIndex, Targets: []int64{1, 2},
		Pre:        fxSide(stmts, seasonStep.Add(-time.Hour), 12, false),
		Post:       fxSide(stmts, seasonPost, 12, false),
		Seasonal:   Series{},
		RegressPct: DefaultRegressPct}
	for _, s := range stmts {
		for k := 1; k <= 4; k++ {
			w := wobble(0.01, s.id, 5, k)
			in.Seasonal[s.id] = append(in.Seasonal[s.id], Bucket{
				Start: seasonPost.Add(-time.Duration(k) * week), Calls: 600,
				Blocks: s.blocks * 600 * w, TotalMs: s.ms * 600 * w, Rows: 600})
		}
	}
	for _, sparse := range []int64{1, 2} {
		in := in
		in.Post = Series{1: in.Post[1], 2: in.Post[2]}
		in.Post[sparse] = everyOther(in.Post[sparse])
		a := mustAttribute(t, in)
		if !a.Measured || a.Basis != BasisSeasonal {
			t.Fatalf("query %d sparse: measured %v basis %q (%s)", sparse, a.Measured,
				a.Basis, a.Reason)
		}
		within(t, "EffectPct", a.EffectPct, 0, 3)
		if a.Guardrail.Regressed {
			t.Fatalf("query %d sparse: guardrail %+v", sparse, a.Guardrail)
		}
	}
}
