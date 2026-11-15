package verify

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// The estimate of an attribution (lab-01a): per-bucket log values of a
// unit (the pooled targets, or one control), each statement read against
// its own reference level (the pre-window's, or each post hour's seasonal
// expectation; attribution_levels.go), then Welch's comparison of the
// reference and post means.

const week = 7 * 24 * time.Hour

// metric is a bucket's per-call value.
type metric func(Bucket) float64

func blocksPerCall(b Bucket) float64 { return b.Blocks / float64(b.Calls) }
func msPerCall(b Bucket) float64     { return b.TotalMs / float64(b.Calls) }

// unit is a pooled group of statements: weights over queryids (postW, when
// set, replaces them on the post side) and the series they are read from.
type unit struct {
	weights, postW  map[int64]float64
	pre, post, hist Series
}

// samples are normalized log values keyed by bucket start: ref (the
// reference: pre-window buckets, or the seasonal weeks' hours) and post.
type samples struct{ ref, post map[time.Time]float64 }

// samples reads the unit's reference and post buckets against each
// statement's own level on basis (see pooled).
func (u unit) samples(basis string, m metric) samples {
	lv, ref := u.preLevels(m), u.pre
	refKey := func(time.Time) time.Time { return time.Time{} }
	postKey := refKey
	if basis != BasisPreWindow {
		var hourOf map[time.Time]time.Time
		lv, hourOf = u.seasonalLevels(m)
		ref, refKey = u.hist, func(t time.Time) time.Time { return hourOf[t] }
		postKey = func(t time.Time) time.Time { return t.Truncate(time.Hour) }
	}
	postW, postLevel := u.weights, lv.own(postKey)
	if u.postW != nil {
		postW, postLevel = u.postW, lv.pooledOf(u.weights, postKey)
	}
	return samples{ref: pooled(ref, u.weights, m, lv.own(refKey)),
		post: pooled(u.post, postW, m, postLevel)}
}

// change is the unit's mean log change on basis (0 without samples).
func (u unit) change(basis string, m metric) float64 {
	s := u.samples(basis, m)
	if len(s.ref) == 0 || len(s.post) == 0 {
		return 0
	}
	return mean(values(s.post)) - mean(values(s.ref))
}

// estimated is one comparison: the effect, its CI half-width, and the
// treated and control changes behind it.
type estimated struct{ diff, half, treated, control float64 }

// estimate compares the treated unit, minus the per-bucket median of the
// controls when there are any, between reference and post.
func estimate(treated unit, controls []unit, basis string, m metric, th Thresholds) (
	estimated, string) {
	t := treated.samples(basis, m)
	d, k := t, samples{}
	if len(controls) > 0 {
		cs := make([]samples, len(controls))
		for i, c := range controls {
			cs[i] = c.samples(basis, m)
		}
		k = samples{ref: medianAt(cs, true), post: medianAt(cs, false)}
		d = samples{ref: minus(t.ref, k.ref), post: minus(t.post, k.post)}
	}
	if len(d.ref) < th.MinBuckets || len(d.post) < th.MinBuckets {
		return estimated{}, fmt.Sprintf("too few buckets to estimate noise (%d reference, "+
			"%d after; %d needed)", len(d.ref), len(d.post), th.MinBuckets)
	}
	diff, half := welchMeans(values(d.ref), values(d.post), th.Alpha)
	e := estimated{diff: diff, half: half,
		treated: mean(values(t.post)) - mean(values(t.ref))}
	if len(controls) > 0 {
		e.control = mean(values(k.post)) - mean(values(k.ref))
	}
	return e, ""
}

// guardrail is the time-per-call comparison; it can veto a gain only.
func guardrail(treated unit, controls []unit, basis string, th Thresholds) Guardrail {
	est, reason := estimate(treated, controls, basis, msPerCall, th)
	if reason != "" {
		return Guardrail{Reason: "time per call: " + reason}
	}
	g := Guardrail{Measured: true, EffectPct: logPct(est.diff),
		CILowPct: logPct(est.diff - est.half), CIHighPct: logPct(est.diff + est.half)}
	span := fmt.Sprintf("%+.1f%% (95%% CI %+.1f%%..%+.1f%%)", g.EffectPct, g.CILowPct,
		g.CIHighPct)
	// A rise of exactly regress_pct is not a regression; rounding to a
	// millionth of a point absorbs the float error of exp(log(x)).
	if math.Round(g.EffectPct*1e6)/1e6 > th.RegressPct && g.CILowPct > 0 {
		g.Regressed = true
		g.Reason = fmt.Sprintf("time per call rose %s, beyond regress_pct %.1f%%: "+
			"vetoes any gain", span, th.RegressPct)
		return g
	}
	g.Reason = "time per call " + span + ": within the guardrail"
	return g
}

// welchMeans is the difference of the post and reference means and the
// half-width of its CI (Welch's statistic, TCritical): 0 with no spread.
func welchMeans(ref, post []float64, alpha float64) (float64, float64) {
	diff := mean(post) - mean(ref)
	seR, seP := stdErr(ref), stdErr(post)
	se := math.Sqrt(seR*seR + seP*seP)
	if !(se > 0) {
		return diff, 0
	}
	return diff, TCritical(welchDF(seR, seP, len(ref), len(post)), alpha) * se
}

func stdErr(vs []float64) float64 {
	n := len(vs)
	if n < 2 {
		return 0
	}
	mu := mean(vs)
	var ss float64
	for _, v := range vs {
		ss += (v - mu) * (v - mu)
	}
	return math.Sqrt(ss / float64(n-1) / float64(n))
}

// medianAt is, per bucket start, the median of the controls' ref (or post)
// samples that have the start.
func medianAt(cs []samples, ref bool) map[time.Time]float64 {
	at := map[time.Time][]float64{}
	for _, c := range cs {
		m := c.post
		if ref {
			m = c.ref
		}
		for _, t := range sortedTimes(m) {
			at[t] = append(at[t], m[t])
		}
	}
	out := make(map[time.Time]float64, len(at))
	for t, vs := range at {
		out[t] = median(vs)
	}
	return out
}

// minus is a - b at the starts both have.
func minus(a, b map[time.Time]float64) map[time.Time]float64 {
	out := map[time.Time]float64{}
	for t, v := range a {
		if w, ok := b[t]; ok {
			out[t] = v - w
		}
	}
	return out
}

func shifted(m map[time.Time]float64, by float64) map[time.Time]float64 {
	out := make(map[time.Time]float64, len(m))
	for t, v := range m {
		out[t] = v + by
	}
	return out
}

func sortedTimes(m map[time.Time]float64) []time.Time {
	out := make([]time.Time, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// values are m's values in start order, so sums are deterministic.
func values(m map[time.Time]float64) []float64 { return valuesAt(m, sortedTimes(m)) }

func valuesAt(m map[time.Time]float64, ts []time.Time) []float64 {
	out := make([]float64, len(ts))
	for i, t := range ts {
		out[i] = m[t]
	}
	return out
}

func mean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var s float64
	for _, v := range vs {
		s += v
	}
	return s / float64(len(vs))
}
