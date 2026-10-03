package verify

import (
	"fmt"
	"math"
	"time"
)

// Interval is one sampling interval of a query: the calls it ran and the
// execution time they took (deltas of pg_stat_statements counters).
type Interval struct {
	Calls   int64
	TotalMs float64
}

// Defaults for a zero Thresholds field: a zero never means "no bar".
const (
	DefaultMinSamples = 30
	DefaultMinBuckets = 3
	DefaultGainPct    = 20.0
	DefaultRegressPct = 15.0
	DefaultAlpha      = 0.05
)

// Thresholds are the bars of one comparison. MinSamples is the calls each
// window needs, MinBuckets the time buckets with calls (variance needs
// several), GainPct and RegressPct the practical bars on the
// call-weighted mean, Alpha the two-sided significance level.
type Thresholds struct {
	MinSamples int
	MinBuckets int
	GainPct    float64
	RegressPct float64
	Alpha      float64
}

func (t Thresholds) normalized() Thresholds {
	if t.MinSamples <= 0 {
		t.MinSamples = DefaultMinSamples
	}
	if t.MinBuckets < 2 {
		t.MinBuckets = DefaultMinBuckets
	}
	if !(t.GainPct > 0) {
		t.GainPct = DefaultGainPct
	}
	if !(t.RegressPct > 0) {
		t.RegressPct = DefaultRegressPct
	}
	if !(t.Alpha > 0 && t.Alpha < 1) {
		t.Alpha = DefaultAlpha
	}
	return t
}

// Comparison is the before/after decision on one call-weighted mean:
// the relative change, its confidence interval, the Welch statistic and
// the verdict.
type Comparison struct {
	Verdict   string
	DeltaPct  float64
	CILowPct  float64
	CIHighPct float64
	T         float64
	DF        float64
	Before    Measurement
	After     Measurement
	Reason    string
}

// Summarize returns the call-weighted mean of the intervals and the
// standard error of that mean. Intervals are clusters: the error is the
// linearized variance of the ratio estimator sum(T)/sum(C),
// n/(n-1) * sum((T_i - mean*C_i)^2) / C^2, so a window whose interval
// means wander stays uncertain however many calls it has. Idle and
// malformed intervals are skipped.
func Summarize(intervals []Interval) Measurement {
	var calls int64
	var total float64
	valid := make([]Interval, 0, len(intervals))
	for _, iv := range intervals {
		if iv.Calls <= 0 || !(iv.TotalMs >= 0) || math.IsInf(iv.TotalMs, 0) {
			continue
		}
		valid = append(valid, iv)
		calls += iv.Calls
		total += iv.TotalMs
	}
	if calls == 0 {
		return Measurement{}
	}
	mean := total / float64(calls)
	m := Measurement{Samples: int(calls), AverageLatency: msDuration(mean),
		Buckets: len(valid)}
	if n := len(valid); n >= 2 {
		var ss float64
		for _, iv := range valid {
			d := iv.TotalMs - mean*float64(iv.Calls)
			ss += d * d
		}
		c := float64(calls)
		m.StdErr = msDuration(math.Sqrt(float64(n) / float64(n-1) * ss / (c * c)))
	}
	return m
}

// Pool combines per-query measurements into the call-weighted measurement
// of them all. Queries are treated as independent: the variance is
// sum((C_q/C)^2 * SE_q^2). Buckets is the smallest bucket count of a
// measured query, so the pool is no better resolved than its thinnest
// member.
func Pool(ms []Measurement) Measurement {
	var calls int
	var total float64
	buckets, first := 0, true
	for _, m := range ms {
		if m.Samples <= 0 {
			continue
		}
		calls += m.Samples
		total += float64(m.Samples) * toMs(m.AverageLatency)
		if first || m.Buckets < buckets {
			buckets, first = m.Buckets, false
		}
	}
	if calls == 0 {
		return Measurement{}
	}
	var variance float64
	for _, m := range ms {
		if m.Samples <= 0 {
			continue
		}
		w, se := float64(m.Samples)/float64(calls), toMs(m.StdErr)
		variance += w * w * se * se
	}
	return Measurement{Samples: calls, AverageLatency: msDuration(total / float64(calls)),
		StdErr: msDuration(math.Sqrt(variance)), Buckets: buckets}
}

// Compare decides whether after differs from before: Welch's test on the
// two call-weighted means, then the practical bars. A change counts only
// when it is both significant and beyond its bar; "neutral" needs the
// confidence interval inside the bars; anything else (too few calls or
// buckets, or an interval too wide to tell) is insufficient evidence.
func Compare(before, after Measurement, th Thresholds) Comparison {
	th = th.normalized()
	c := Comparison{Before: before, After: after}
	if reason := insufficiency(before, after, th); reason != "" {
		c.Verdict, c.Reason = OutcomeInsufficient, reason
		return c
	}
	b, a := toMs(before.AverageLatency), toMs(after.AverageLatency)
	seB, seA := toMs(before.StdErr), toMs(after.StdErr)
	diff := a - b
	se := math.Sqrt(seB*seB + seA*seA)
	c.DeltaPct = diff * 100 / b
	half := 0.0
	if se > 0 {
		c.T = diff / se
		c.DF = welchDF(seB, seA, before.Buckets, after.Buckets)
		half = TCritical(c.DF, th.Alpha) * se
	}
	c.CILowPct, c.CIHighPct = (diff-half)*100/b, (diff+half)*100/b
	c.Verdict, c.Reason = decide(c, th)
	return c
}

func insufficiency(before, after Measurement, th Thresholds) string {
	switch {
	case before.Samples < th.MinSamples || after.Samples < th.MinSamples:
		return fmt.Sprintf("too few calls (%d before, %d after; %d needed)",
			before.Samples, after.Samples, th.MinSamples)
	case before.Buckets < th.MinBuckets || after.Buckets < th.MinBuckets:
		return fmt.Sprintf("too few sampling intervals to estimate noise (%d before, "+
			"%d after; %d needed)", before.Buckets, after.Buckets, th.MinBuckets)
	case before.AverageLatency <= 0:
		return "no pre-action latency to compare against"
	}
	return ""
}

func decide(c Comparison, th Thresholds) (string, string) {
	span := fmt.Sprintf("%+.1f%% (95%% CI %+.1f%%..%+.1f%%)", c.DeltaPct, c.CILowPct,
		c.CIHighPct)
	switch {
	case c.CILowPct > 0 && c.DeltaPct > th.RegressPct:
		return OutcomeRegressed, "call-weighted mean rose " + span
	case c.CIHighPct < 0 && -c.DeltaPct >= th.GainPct:
		return OutcomeImproved, "call-weighted mean fell " + span
	case c.CILowPct >= -th.GainPct && c.CIHighPct <= th.RegressPct:
		return OutcomeNeutral, "no change beyond the bars: " + span
	}
	return OutcomeInsufficient, "too noisy to tell: " + span
}

// welchDF is the Welch-Satterthwaite degrees of freedom, with each side's
// sample count being its buckets; 0 when it cannot be computed.
func welchDF(seB, seA float64, nB, nA int) float64 {
	vB, vA := seB*seB, seA*seA
	den := 0.0
	if nB > 1 {
		den += vB * vB / float64(nB-1)
	}
	if nA > 1 {
		den += vA * vA / float64(nA-1)
	}
	if den == 0 {
		return 0
	}
	df := (vB + vA) * (vB + vA) / den
	if math.IsNaN(df) || math.IsInf(df, 0) {
		return 0
	}
	return df
}

// TCritical is the two-sided critical value of Student's t at alpha with
// df degrees of freedom: exact for df < 3 (rounded down, so conservative),
// the Cornish-Fisher expansion otherwise. An unknown df is +Inf (never
// significant); a nonsense alpha is DefaultAlpha.
func TCritical(df, alpha float64) float64 {
	if !(df > 0) {
		return math.Inf(1)
	}
	if !(alpha > 0 && alpha < 1) {
		alpha = DefaultAlpha
	}
	p := 1 - alpha/2
	switch {
	case df < 2:
		return math.Tan(math.Pi * (p - 0.5))
	case df < 3:
		return (2*p - 1) / math.Sqrt(2*p*(1-p))
	}
	z := math.Sqrt2 * math.Erfinv(2*p-1)
	z2 := z * z
	g1 := (z2 + 1) * z / 4
	g2 := ((5*z2+16)*z2 + 3) * z / 96
	g3 := (((3*z2+19)*z2+17)*z2 - 15) * z / 384
	g4 := ((((79*z2+776)*z2+1482)*z2-1920)*z2 - 945) * z / 92160
	return z + g1/df + g2/(df*df) + g3/(df*df*df) + g4/(df*df*df*df)
}

func toMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func msDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

// Evidence is the comparison as JSON-safe data for the outcome ledger.
func (c Comparison) Evidence() map[string]any {
	return map[string]any{
		"verdict": c.Verdict, "reason": c.Reason, "delta_pct": finite(c.DeltaPct),
		"ci_low_pct": finite(c.CILowPct), "ci_high_pct": finite(c.CIHighPct),
		"t": finite(c.T), "df": finite(c.DF),
		"before": measurementEvidence(c.Before), "after": measurementEvidence(c.After),
	}
}

func measurementEvidence(m Measurement) map[string]any {
	return map[string]any{"calls": m.Samples, "mean_ms": toMs(m.AverageLatency),
		"stderr_ms": toMs(m.StdErr), "buckets": m.Buckets}
}

// finite drops NaN and infinities, which JSON cannot encode.
func finite(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}
