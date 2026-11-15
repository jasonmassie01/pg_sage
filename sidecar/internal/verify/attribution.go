package verify

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// Attribution v2 (lab-01a) estimates what one step did to its target
// statements: shared blocks (hit plus read) per call is the primary metric,
// pooled over the targets with weights frozen at the pre-period call
// shares; the change is taken against the same hour-of-week of earlier
// weeks when enough history exists (else the pre-window), and the median
// change of untouched control statements is subtracted. Time per call is a
// guardrail that can veto a gain but never creates one. Pure functions
// over typed inputs: no database access, no shared state.

// Attribution kinds: an index step (controls exist) or a global setting.
const (
	AttributionIndex  = "index"
	AttributionGlobal = "global"
)

// Methods and bases of an attribution.
const (
	MethodDiD      = "did" // treated change minus the median control change
	MethodITS      = "its" // interrupted time series, no control term
	BasisSeasonal  = "seasonal"
	BasisPreWindow = "pre_window"
)

// MinControls is the fewest control statements that give a control term.
const MinControls = 3

// Reasons an attribution gives (Attribution.Reason contains them).
const (
	ReasonNoTargets           = "no targets to attribute"
	ReasonNoComparableTargets = "no target ran both before and after the action"
	ReasonNoPreBlocks         = "no pre-action blocks to compare against"
	ReasonFewControls         = "controls<3"
	ReasonGlobal              = "no control group: global setting"
	ReasonEpochBreak          = "the statistics epoch changed inside the window (a " +
		"reset, or a suspend and resume): no measurement"
)

// Bucket is one statement's counter deltas over one time bucket: calls,
// execution time, shared blocks hit plus read, and rows.
type Bucket struct {
	Start   time.Time
	Calls   int64
	TotalMs float64
	Blocks  float64
	Rows    int64
}

// Series is the buckets of each queryid.
type Series map[int64][]Bucket

// AttributionInput is everything one attribution reads. Seasonal holds the
// hourly history (the rollup's rows, buckets on the hour) of targets and
// controls. Weights are the targets' frozen pre-period call shares, as
// stored with the watch; nil computes them from Pre (FrozenWeights).
type AttributionInput struct {
	Kind       string
	Targets    []int64
	Controls   []int64
	Pre, Post  Series
	Seasonal   Series
	Weights    map[int64]float64
	RegressPct float64
	Alpha      float64
	// EpochBreak reports a statistics epoch change inside a window: no
	// measurement, never a partial one.
	EpochBreak bool
	// postWeights is a test-only switch that weights each side by its own
	// calls (v1 Pool's weighting), to prove the frozen weights matter.
	postWeights bool
}

// Guardrail is the time-per-call result of the same comparison: a rise
// beyond regress_pct with its CI above 0 is a regression whatever blocks
// say.
type Guardrail struct {
	Measured  bool
	EffectPct float64
	CILowPct  float64
	CIHighPct float64
	Regressed bool
	Reason    string
}

// Attribution is the estimated effect in log space with its CI, and the
// terms behind it. EffectPct is exp(EffectLog)-1 in percent. Measured is
// false (and every effect zero) when Reason says why there is none.
type Attribution struct {
	Measured      bool
	EffectLog     float64
	CILow, CIHigh float64
	EffectPct     float64
	TreatedLog    float64 // the targets' change on Basis
	ControlLog    float64 // the median control change on Basis
	SeasonalLog   float64 // the targets' change against the seasonal expectation
	PreWindowLog  float64 // the targets' change against the pre-window
	Guardrail     Guardrail
	Method, Basis string
	SeasonalWeeks int
	Controls      int
	Weights       map[int64]float64
	Reason        string
}

// FrozenWeights are the targets' pre-period call shares: the weights a
// watch stores when it is created. A target without pre calls gets none.
func FrozenWeights(pre Series, targets []int64) map[int64]float64 {
	out := map[int64]float64{}
	var total float64
	for _, id := range targets {
		if c := callsOf(cleanBuckets(pre[id])); c > 0 {
			out[id] = c
			total += c
		}
	}
	for id := range out {
		out[id] /= total
	}
	return out
}

// Attribute estimates the step's effect. It refuses an unknown kind and a
// target listed as its own control; every other gap is a Reason.
func Attribute(in AttributionInput) (Attribution, error) {
	if err := validateAttribution(in); err != nil {
		return Attribution{}, err
	}
	switch {
	case len(in.Targets) == 0:
		return Attribution{Reason: ReasonNoTargets}, nil
	case in.EpochBreak:
		return Attribution{Reason: ReasonEpochBreak}, nil
	}
	pre, post, hist := cleanSeries(in.Pre), cleanSeries(in.Post), cleanSeries(in.Seasonal)
	weights, notes := poolWeights(in, pre, post)
	if len(weights) == 0 {
		return Attribution{Reason: joinReasons(ReasonNoComparableTargets, notes)}, nil
	}
	if blocksOf(pre, weights) <= 0 {
		return Attribution{Reason: joinReasons(ReasonNoPreBlocks, notes)}, nil
	}
	treated := unit{weights: weights, pre: pre, post: post, hist: hist}
	if in.postWeights {
		treated.postW = FrozenWeights(post, sortedKeys(weights))
	}
	out := Attribution{Weights: weights, SeasonalWeeks: treated.seasonalWeeks(),
		Basis: BasisPreWindow, Method: MethodITS}
	if out.SeasonalWeeks >= baseline.MinWeeks {
		out.Basis = BasisSeasonal
	} else if out.SeasonalWeeks > 0 {
		notes = append(notes, fmt.Sprintf("seasonal history covers %d weeks (%d needed): "+
			"pre-window basis", out.SeasonalWeeks, baseline.MinWeeks))
	}
	controls, cnote := pickControls(in, pre, post, hist, out.Basis)
	notes = append(notes, cnote)
	if len(controls) > 0 {
		out.Method, out.Controls = MethodDiD, len(controls)
	}
	return finishAttribution(out, treated, controls, in, notes), nil
}

func finishAttribution(out Attribution, treated unit, controls []unit,
	in AttributionInput, notes []string) Attribution {
	th := Thresholds{RegressPct: in.RegressPct, Alpha: in.Alpha}.normalized()
	est, reason := estimate(treated, controls, out.Basis, blocksPerCall, th)
	if reason != "" {
		return Attribution{Reason: joinReasons(reason, notes)}
	}
	out.Measured = true
	out.EffectLog, out.CILow, out.CIHigh = est.diff, est.diff-est.half, est.diff+est.half
	out.EffectPct = logPct(est.diff)
	out.TreatedLog, out.ControlLog = est.treated, est.control
	out.PreWindowLog = treated.change(BasisPreWindow, blocksPerCall)
	if out.SeasonalWeeks > 0 {
		out.SeasonalLog = treated.change(BasisSeasonal, blocksPerCall)
	}
	out.Guardrail = guardrail(treated, controls, out.Basis, th)
	out.Reason = strings.Join(nonEmpty(notes), "; ")
	return out
}

func validateAttribution(in AttributionInput) error {
	if in.Kind != AttributionIndex && in.Kind != AttributionGlobal {
		return fmt.Errorf("verify: attribution kind %q is neither %q nor %q", in.Kind,
			AttributionIndex, AttributionGlobal)
	}
	for _, id := range in.Controls {
		if slices.Contains(in.Targets, id) {
			return fmt.Errorf("verify: query %d is a target and cannot be its own control",
				id)
		}
	}
	return nil
}

// poolWeights are the frozen weights of the targets that ran on both
// sides, renormalized; each dropped target leaves a note.
func poolWeights(in AttributionInput, pre, post Series) (map[int64]float64, []string) {
	base := in.Weights
	if base == nil {
		base = FrozenWeights(pre, in.Targets)
	}
	kept, notes := map[int64]float64{}, []string(nil)
	var total float64
	for _, id := range sortedIDs(in.Targets) {
		w := base[id]
		switch {
		case !(w > 0) || math.IsInf(w, 0) || callsOf(pre[id]) == 0:
			notes = append(notes, fmt.Sprintf("query %d: no pre-action calls", id))
		case callsOf(post[id]) == 0:
			notes = append(notes, fmt.Sprintf("query %d: no post-action calls", id))
		default:
			kept[id] = w
			total += w
		}
	}
	for id := range kept {
		kept[id] /= total
	}
	return kept, notes
}

// pickControls are the controls usable on basis; fewer than MinControls
// (or a global setting) give none, with the note saying why.
func pickControls(in AttributionInput, pre, post, hist Series, basis string) (
	[]unit, string) {
	if in.Kind == AttributionGlobal {
		return nil, ReasonGlobal
	}
	var out []unit
	for _, id := range sortedIDs(in.Controls) {
		u := unit{weights: map[int64]float64{id: 1}, pre: pre, post: post, hist: hist}
		s := u.samples(basis, blocksPerCall)
		if len(s.ref) >= DefaultMinBuckets && len(s.post) >= DefaultMinBuckets {
			out = append(out, u)
		}
	}
	if len(out) < MinControls {
		return nil, fmt.Sprintf("%s (%d usable): no control term", ReasonFewControls,
			len(out))
	}
	return out, ""
}

// cleanBuckets drops idle and malformed buckets (like Summarize) and merges
// buckets of one start (in UTC, so map keys compare), ordered by start.
func cleanBuckets(bs []Bucket) []Bucket {
	byStart := map[time.Time]Bucket{}
	for _, b := range bs {
		if b.Calls <= 0 || b.Rows < 0 || !finiteNonNeg(b.TotalMs) || !finiteNonNeg(b.Blocks) {
			continue
		}
		start := b.Start.UTC()
		m := byStart[start]
		m.Start = start
		m.Calls, m.TotalMs, m.Blocks = m.Calls+b.Calls, m.TotalMs+b.TotalMs, m.Blocks+b.Blocks
		m.Rows += b.Rows
		byStart[start] = m
	}
	out := make([]Bucket, 0, len(byStart))
	for _, b := range byStart {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Bucket) int { return a.Start.Compare(b.Start) })
	return out
}

func cleanSeries(s Series) Series {
	out := make(Series, len(s))
	for id, bs := range s {
		if c := cleanBuckets(bs); len(c) > 0 {
			out[id] = c
		}
	}
	return out
}

func finiteNonNeg(v float64) bool { return v >= 0 && !math.IsInf(v, 0) }

func callsOf(bs []Bucket) float64 {
	var c float64
	for _, b := range bs {
		if b.Calls > 0 {
			c += float64(b.Calls)
		}
	}
	return c
}

func blocksOf(s Series, weights map[int64]float64) float64 {
	var total float64
	for id := range weights {
		for _, b := range s[id] {
			total += b.Blocks
		}
	}
	return total
}

func joinReasons(reason string, notes []string) string {
	return strings.Join(nonEmpty(append([]string{reason}, notes...)), "; ")
}

func nonEmpty(ss []string) []string {
	return slices.DeleteFunc(slices.Clone(ss), func(s string) bool { return s == "" })
}

func sortedIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

func sortedKeys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func logPct(v float64) float64 { return (math.Exp(v) - 1) * 100 }
