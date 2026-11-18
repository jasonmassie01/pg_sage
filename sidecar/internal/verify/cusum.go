package verify

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// Contamination detection of verification v2 (lab-01b). A deploy at hour
// 5 of a 24-hour window is not the step's doing, yet a window average
// takes it in. Each target's residual (its log change against its own
// reference level, the seasonal expectation or the pre-window, as the
// attribution reads it) and the control median's residual are scanned
// after the step by a two-sided CUSUM with k = 0.5 and h = 5 robust
// deviations (baseline.Robust, the scale of baseline.Judge, floored at
// 0.05 in log units as Judge floors at 5% of the median). The step's
// own effect is the post-window level, so a departure from it that begins
// more than one collector cycle after the step is an unrelated break: the
// window is contaminated and the next look starts after the changepoint.
// Pure functions over typed inputs.

// CUSUM parameters in robust deviations, and the fewest buckets scanned.
const (
	CusumK          = 0.5
	CusumH          = 5.0
	MinCusumBuckets = 3
	// CusumConfirmZ is how many standard errors the levels either side of
	// an alarm must differ by for it to be a changepoint.
	CusumConfirmZ = 3.0
	// cusumScaleFloor floors the scale at 0.05 in log units: Judge floors
	// its MAD at 5% of the median, and a residual is a log ratio to its
	// level, so 5% of the level is 0.05. Without it a quiet reference
	// makes ordinary hour-to-hour noise look like breaks.
	cusumScaleFloor = 0.05
)

// Contamination reasons.
const (
	ReasonCusumFewBuckets = "too few buckets for a changepoint scan"
	ReasonContaminatedAt  = "contaminated_at="
)

// Point is one bucket of a residual series.
type Point struct {
	At time.Time
	V  float64
}

// Changepoint is one series' scan. At is the first bucket of the new
// regime when Fired; Stat is the largest CUSUM statistic reached and
// Scale the robust deviation it is counted in; Shift is the level change
// at At and ShiftZ that change over its standard error.
type Changepoint struct {
	Scanned       bool
	Fired         bool
	At            time.Time
	Stat, Scale   float64
	Shift, ShiftZ float64
	Reason        string
}

// ScanChangepoint scans post for one departure from its level (the
// median); a NaN or infinite bucket is left out. The scale is ref's robust deviation (post's when ref has fewer
// than MinWeeks values), floored at cusumScaleFloor. A departure from
// the first bucket is the step itself and is found from the other end:
// the forward scan locates a break with the old level in the majority, the
// backward scan one with the new level in the majority. An alarm is a
// changepoint only when the medians either side of it differ by at least
// CusumConfirmZ standard errors: a CUSUM on an estimated level drifts, and
// a drift is not a break.
func ScanChangepoint(post []Point, ref []float64) Changepoint {
	pts := slices.DeleteFunc(slices.Clone(post), func(p Point) bool {
		return math.IsNaN(p.V) || math.IsInf(p.V, 0)
	})
	if len(pts) < MinCusumBuckets {
		return Changepoint{Reason: ReasonCusumFewBuckets}
	}
	slices.SortStableFunc(pts, func(a, b Point) int { return a.At.Compare(b.At) })
	x := make([]float64, len(pts))
	for i, p := range pts {
		x[i] = p.V
	}
	own, spread := baseline.Robust(x, cusumScaleFloor), baseline.Robust(ref, cusumScaleFloor)
	if !spread.OK {
		spread = own
	}
	scale := max(spread.Scale, cusumScaleFloor)
	cp := Changepoint{Scanned: true, Scale: scale}
	start, fstat, fired := cusumRun(x, own.Median, scale)
	back, bstat, bfired := cusumRun(reversed(x), own.Median, scale)
	cp.Stat = max(fstat, bstat)
	at := -1
	switch {
	case fired && start > 0:
		at = start
	case bfired && len(x)-back < len(x):
		at = len(x) - back
	}
	if at > 0 {
		cp.Shift, cp.ShiftZ = levelShift(x, at, scale)
		cp.Fired = math.Abs(cp.ShiftZ) >= CusumConfirmZ
	}
	if cp.Fired {
		cp.At = pts[at].At
	}
	return cp
}

// levelShift is the median after x[at] minus the median before it, and
// that over its standard error (1.2533 x scale for a median, per side).
func levelShift(x []float64, at int, scale float64) (float64, float64) {
	shift := median(x[at:]) - median(x[:at])
	se := 1.2533 * scale * math.Sqrt(1/float64(at)+1/float64(len(x)-at))
	return shift, shift / se
}

// cusumRun is a two-sided CUSUM of x against center in units of sd. It
// returns the first alarm's departure start (the bucket after the alarming
// side last stood at zero), the largest statistic, and whether a statistic
// exceeded CusumH; a statistic of exactly CusumH does not fire.
func cusumRun(x []float64, center, sd float64) (int, float64, bool) {
	var hi, lo, stat float64
	zeroHi, zeroLo := -1, -1
	for i, v := range x {
		z := (v - center) / sd
		hi, lo = max(0, hi+z-CusumK), max(0, lo-z-CusumK)
		if hi == 0 {
			zeroHi = i
		}
		if lo == 0 {
			zeroLo = i
		}
		stat = max(stat, hi, lo)
		switch {
		case hi > CusumH:
			return zeroHi + 1, stat, true
		case lo > CusumH:
			return zeroLo + 1, stat, true
		}
	}
	return -1, stat, false
}

func reversed(x []float64) []float64 {
	out := slices.Clone(x)
	slices.Reverse(out)
	return out
}

// SeriesScan is one scanned series: "query <id>" or "controls".
type SeriesScan struct {
	Series string
	Changepoint
}

// Contamination is the window's result. When Contaminated, At is the
// earliest break more than one cycle after the step, NextStart the latest
// (where the next look's window starts), Outcome insufficient_evidence and
// Reason contaminated_at=<At, RFC 3339>. Otherwise Reason may say why a
// series was not scanned.
type Contamination struct {
	Contaminated    bool
	At, NextStart   time.Time
	Scans           []SeriesScan
	Outcome, Reason string
}

// DetectContamination scans the attribution's residual series for breaks
// unrelated to the step at step. It refuses what Attribute refuses, a
// zero step and a negative cycle; every gap is a Reason.
func DetectContamination(in AttributionInput, step time.Time, cycle time.Duration) (
	Contamination, error) {
	if err := validateAttribution(in); err != nil {
		return Contamination{}, err
	}
	if step.IsZero() || cycle < 0 {
		return Contamination{}, fmt.Errorf("verify: contamination scan needs the step "+
			"time and a non-negative collector cycle (step %s, cycle %s)",
			step.Format(time.RFC3339), cycle)
	}
	switch {
	case len(in.Targets) == 0:
		return Contamination{Reason: ReasonNoTargets}, nil
	case in.EpochBreak:
		return Contamination{Reason: ReasonEpochBreak}, nil
	}
	return contamination(residualSeries(in), step.UTC(), cycle), nil
}

// contamination classifies each series' changepoint against the step.
func contamination(series []seriesResidual, step time.Time, cycle time.Duration) Contamination {
	var c Contamination
	var reasons []string
	for _, s := range series {
		cp := ScanChangepoint(s.post, s.ref)
		c.Scans = append(c.Scans, SeriesScan{Series: s.name, Changepoint: cp})
		if !cp.Scanned {
			reasons = append(reasons, s.name+": "+cp.Reason)
			continue
		}
		if !cp.Fired || cp.At.Sub(step) <= cycle {
			continue
		}
		if !c.Contaminated || cp.At.Before(c.At) {
			c.At = cp.At
		}
		if !c.Contaminated || cp.At.After(c.NextStart) {
			c.NextStart = cp.At
		}
		c.Contaminated = true
	}
	if c.Contaminated {
		c.Outcome = OutcomeInsufficient
		c.Reason = ReasonContaminatedAt + c.At.Format(time.RFC3339)
		return c
	}
	c.Reason = joinReasons("", reasons)
	return c
}

// seriesResidual is one series' post residuals and reference values.
type seriesResidual struct {
	name string
	post []Point
	ref  []float64
}

// residualSeries are each target's residuals and the control median's on
// the attribution's basis (seasonal with enough weeks, else pre-window).
func residualSeries(in AttributionInput) []seriesResidual {
	pre, post, hist := cleanSeries(in.Pre), cleanSeries(in.Post), cleanSeries(in.Seasonal)
	weights, _ := poolWeights(in, pre, post)
	basis := BasisPreWindow
	all := unit{weights: weights, pre: pre, post: post, hist: hist}
	if len(weights) > 0 && all.seasonalWeeks() >= baseline.MinWeeks {
		basis = BasisSeasonal
	}
	var out []seriesResidual
	for _, id := range sortedIDs(in.Targets) {
		u := unit{weights: map[int64]float64{id: 1}, pre: pre, post: post, hist: hist}
		s := u.samples(basis, blocksPerCall)
		out = append(out, seriesResidual{name: fmt.Sprintf("query %d", id),
			post: points(s.post), ref: values(s.ref)})
	}
	controls, _ := pickControls(in, pre, post, hist, basis)
	if len(controls) > 0 {
		cs := make([]samples, len(controls))
		for i, c := range controls {
			cs[i] = c.samples(basis, blocksPerCall)
		}
		out = append(out, seriesResidual{name: "controls",
			post: points(medianAt(cs, false)), ref: values(medianAt(cs, true))})
	}
	return out
}

func points(m map[time.Time]float64) []Point {
	out := make([]Point, 0, len(m))
	for _, t := range sortedTimes(m) {
		out = append(out, Point{At: t, V: m[t]})
	}
	return out
}

// Evidence is the scan as JSON-safe data.
func (c Contamination) Evidence() map[string]any {
	scans := make([]map[string]any, 0, len(c.Scans))
	for _, s := range c.Scans {
		e := map[string]any{"series": s.Series, "scanned": s.Scanned, "fired": s.Fired,
			"stat": finite(s.Stat), "scale": finite(s.Scale), "reason": s.Reason}
		if s.Fired {
			e["at"] = s.At.Format(time.RFC3339)
		}
		scans = append(scans, e)
	}
	e := map[string]any{"contaminated": c.Contaminated, "scans": scans,
		"outcome": c.Outcome, "reason": c.Reason}
	if c.Contaminated {
		e["at"], e["next_start"] = c.At.Format(time.RFC3339), c.NextStart.Format(time.RFC3339)
	}
	return e
}
