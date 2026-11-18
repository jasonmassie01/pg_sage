package verify

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Repeated looks of verification v2 (lab-01b). A watch is evaluated at a
// window that doubles from the configured one up to HardMax (2, 4, 8, 16,
// 32, 64 and 72 h with the defaults, insufficientVerdict's schedule), and
// seven tests each at alpha 0.05 call about one null step in six a
// change. Each look instead spends part of alpha by an O'Brien-Fleming-
// type Lan-DeMets function of the information fraction t (calls so far
// over the calls expected by HardMax): the cumulative alpha by t is
// 2-2*Phi(z_{alpha/2}/sqrt(t)), and the last look spends what is left.
// Each look's test runs at its increment, so by the union bound the looks
// together reject a null step at most alpha of the time. Bonferroni across
// targets stays on top (TargetAlpha). The spend is stored with the watch
// (WatchState.AlphaSpend, in its baseline JSON), so a restart continues it
// instead of starting again from zero.

// ErrNoLooks reports a look schedule without looks: a window or HardMax
// that is not positive.
var ErrNoLooks = errors.New("verify: no looks: the window and hard max must be positive")

// AlphaSpend is the alpha a watch has spent: the looks taken, the
// information fraction of the last one, the cumulative alpha spent and
// each look's share.
type AlphaSpend struct {
	Looks    int       `json:"looks"`
	Fraction float64   `json:"fraction"`
	Spent    float64   `json:"spent"`
	PerLook  []float64 `json:"per_look,omitempty"`
}

// Look is one look's spend: its 1-based index, information fraction, the
// alpha its test runs at, the cumulative spend after it, and whether it
// is the last.
type Look struct {
	Index    int
	Fraction float64
	Alpha    float64
	Spent    float64
	Final    bool
}

// LookSchedule is the windows of the looks: initial, doubled while below
// hardMax, then hardMax. A non-positive window or hardMax has none.
func LookSchedule(initial, hardMax time.Duration) []time.Duration {
	if initial <= 0 || hardMax <= 0 {
		return nil
	}
	var out []time.Duration
	w := initial
	for ; w < hardMax; w *= 2 {
		out = append(out, w)
	}
	if len(out) == 0 {
		return []time.Duration{w}
	}
	return append(out, hardMax)
}

// LookIndex is the 1-based look a window is (the first scheduled window at
// or beyond it) and whether it is the last; a window past the schedule is
// the last look.
func LookIndex(schedule []time.Duration, window time.Duration) (int, bool, error) {
	if len(schedule) == 0 {
		return 0, false, ErrNoLooks
	}
	for i, w := range schedule {
		if w >= window {
			return i + 1, i == len(schedule)-1, nil
		}
	}
	return len(schedule), true, nil
}

// CumulativeAlpha is the O'Brien-Fleming-type spending function: the
// alpha spent by information fraction t, 0 at t <= 0 and alpha at t >= 1.
func CumulativeAlpha(alpha, t float64) float64 {
	switch {
	case !(t > 0):
		return 0
	case t >= 1:
		return alpha
	}
	z := math.Sqrt2 * math.Erfinv(1-alpha)
	return math.Min(alpha, math.Erfc(z/math.Sqrt(t)/math.Sqrt2))
}

// InformationFraction is calls over the calls expected by HardMax, in
// [0, 1]. Without a usable expectation or count it is the time fraction
// window/hardMax (1 without a hard max: a single look).
func InformationFraction(calls, expected float64, window, hardMax time.Duration) float64 {
	f := calls / expected
	if !(expected > 0) || !(calls >= 0) || math.IsInf(calls, 0) || math.IsInf(expected, 0) {
		if hardMax <= 0 {
			return 1
		}
		f = float64(window) / float64(hardMax)
	}
	return math.Max(0, math.Min(1, f))
}

// SpendLook spends the next look's alpha from prev. A fraction below the
// last look's adds no information and spends nothing; the final look
// spends exactly what is left of alpha. A nonsense alpha, fraction or
// stored spend is an error with nothing spent.
func SpendLook(prev AlphaSpend, alpha, fraction float64, final bool) (Look, AlphaSpend,
	error) {
	switch {
	case !(alpha > 0 && alpha < 1):
		return Look{}, AlphaSpend{}, fmt.Errorf("verify: alpha %v is not in (0, 1)", alpha)
	case !(fraction >= 0) || math.IsInf(fraction, 0):
		return Look{}, AlphaSpend{}, fmt.Errorf("verify: information fraction %v is not "+
			"a non-negative number", fraction)
	case !(prev.Spent >= 0 && prev.Spent <= alpha) || prev.Looks < 0:
		return Look{}, AlphaSpend{}, fmt.Errorf("verify: stored alpha spend %v over %d "+
			"looks is outside [0, %v]: the watch state is corrupt", prev.Spent, prev.Looks,
			alpha)
	}
	f := math.Max(prev.Fraction, math.Min(1, fraction))
	cum := math.Max(prev.Spent, CumulativeAlpha(alpha, f))
	if final {
		f, cum = 1, alpha
	}
	look := Look{Index: prev.Looks + 1, Fraction: f, Alpha: cum - prev.Spent, Spent: cum,
		Final: final}
	next := AlphaSpend{Looks: look.Index, Fraction: f, Spent: cum,
		PerLook: append(append([]float64(nil), prev.PerLook...), look.Alpha)}
	return look, next, nil
}

// TargetAlpha is a look's alpha split over the targets (Bonferroni).
func TargetAlpha(lookAlpha float64, targets int) float64 {
	if targets <= 1 {
		return lookAlpha
	}
	return lookAlpha / float64(targets)
}
