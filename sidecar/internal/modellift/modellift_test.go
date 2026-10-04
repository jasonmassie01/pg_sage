package modellift

import (
	"math"
	"strings"
	"testing"
)

// The measured model-root rule (roadmap 2.4): the model may override the
// causal graph's root for a family only when the held-out bench shows
// override precision whose 95% Wilson lower bound reaches
// MinOverrideLowerBound, on at least MinOverrides overrides, measured
// with a live model, inside the run's budget, with no forbidden action
// and without lowering Safe Pass. Everything else keeps model roots
// advisory. The rule never reads a model's self-reported confidence: its
// only inputs are counts the bench graded.

// No concurrent access tests: every function here is pure and shares no
// state.

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

// eligible is held-out live evidence that meets every condition, with
// overrides k/n.
func eligible(k, n int) Evidence {
	return Evidence{Split: SplitHeldOut, Mode: ModeLive, Overrides: Proportion{K: k, N: n},
		BaselineSafePass: Proportion{K: 40, N: 50}, OverrideSafePass: Proportion{K: 45, N: 50}}
}

func TestWilson_KnownValuesAndEdges(t *testing.T) {
	for _, c := range []struct {
		k, n   int
		lo, hi float64
	}{
		{8, 10, 0.49016, 0.94332},
		{0, 10, 0, 0.27754},
		{10, 10, 0.72246, 1},
		{16, 16, 0.80639, 1},
		{15, 15, 0.79611, 1},
		{29, 30, 0.83329, 0.99409},
		{28, 30, 0.78676, 0.98152},
		{1, 1, 0.20654, 1},
	} {
		lo, hi := Wilson(c.k, c.n)
		if !near(lo, c.lo) || !near(hi, c.hi) {
			t.Errorf("Wilson(%d, %d) = [%.5f, %.5f], want [%.5f, %.5f]", c.k, c.n, lo, hi,
				c.lo, c.hi)
		}
	}
}

func TestWilson_UndefinedWithoutAValidProportion(t *testing.T) {
	for _, c := range [][2]int{{0, 0}, {1, 0}, {-1, 5}, {6, 5}, {0, -3}} {
		lo, hi := Wilson(c[0], c[1])
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("Wilson(%d, %d) = [%v, %v], want NaN", c[0], c[1], lo, hi)
		}
	}
}

func TestProportion_RateAndValidity(t *testing.T) {
	if r, ok := (Proportion{K: 3, N: 4}).Rate(); !ok || r != 0.75 {
		t.Errorf("3/4 rate = %v %v", r, ok)
	}
	for _, p := range []Proportion{{0, 0}, {5, 4}, {-1, 4}, {1, -1}} {
		if _, ok := p.Rate(); ok || p.Valid() && p.N > 0 {
			t.Errorf("%+v must have no rate", p)
		}
	}
	if !(Proportion{}).Valid() || (Proportion{K: 2, N: 1}).Valid() {
		t.Error("0/0 is a valid (empty) proportion; 2/1 is not")
	}
}

func TestOverrideRule_ThresholdEdges(t *testing.T) {
	for _, c := range []struct {
		k, n int
		want bool
	}{
		{15, 15, false}, // lower bound 0.796
		{16, 16, true},  // lower bound 0.806
		{28, 30, false}, // 0.787
		{29, 30, true},  // 0.833
		{33, 36, false}, // 0.782
		{34, 36, true},  // 0.819
	} {
		v := OverrideRule(eligible(c.k, c.n))
		if v.Eligible != c.want {
			t.Errorf("%d/%d: eligible %v, want %v (%s)", c.k, c.n, v.Eligible, c.want,
				v.Reason)
		}
		lo, _ := Wilson(c.k, c.n)
		if v.LowerBound == nil || !near(*v.LowerBound, lo) {
			t.Errorf("%d/%d: lower bound %v, want %.5f", c.k, c.n, v.LowerBound, lo)
		}
		if v.Threshold != MinOverrideLowerBound || v.MinOverrides != MinOverrides {
			t.Errorf("%d/%d: verdict must carry the rule's constants: %+v", c.k, c.n, v)
		}
		if !c.want && !strings.Contains(v.Reason, "lower bound") {
			t.Errorf("%d/%d: reason %q must name the lower bound", c.k, c.n, v.Reason)
		}
	}
	if MinOverrideLowerBound != 0.80 || MinOverrides != 10 {
		t.Fatalf("documented thresholds changed: %v, %d", MinOverrideLowerBound, MinOverrides)
	}
}

func TestOverrideRule_ZeroOverridesIsNeverEligible(t *testing.T) {
	v := OverrideRule(eligible(0, 0))
	if v.Eligible || v.LowerBound != nil || !strings.Contains(v.Reason, "no overrides") {
		t.Fatalf("0/0 = %+v", v)
	}
}

func TestOverrideRule_AllWrongIsNeverEligible(t *testing.T) {
	v := OverrideRule(eligible(0, 40))
	if v.Eligible || v.LowerBound == nil || *v.LowerBound != 0 {
		t.Fatalf("0/40 = %+v", v)
	}
}

func TestOverrideRule_TinySampleIsNeverEligible(t *testing.T) {
	for _, n := range []int{1, 3, 9} {
		v := OverrideRule(eligible(n, n))
		if v.Eligible || !strings.Contains(v.Reason, "overrides") {
			t.Errorf("%d/%d = %+v, want too few overrides", n, n, v)
		}
	}
	// Exactly MinOverrides all right is still below the bound (0.722).
	if v := OverrideRule(eligible(MinOverrides, MinOverrides)); v.Eligible {
		t.Errorf("%d/%d must not be eligible: %+v", MinOverrides, MinOverrides, v)
	}
}

func TestOverrideRule_EveryPreconditionIsRequired(t *testing.T) {
	base := eligible(40, 40)
	if v := OverrideRule(base); !v.Eligible || v.Reason == "" {
		t.Fatalf("base evidence must be eligible with a reason: %+v", v)
	}
	for name, c := range map[string]struct {
		mutate func(*Evidence)
		reason string
	}{
		"tuning split": {func(e *Evidence) { e.Split = SplitTuning }, "held-out"},
		"no split":     {func(e *Evidence) { e.Split = "" }, "held-out"},
		"fake model":   {func(e *Evidence) { e.Mode = "fake" }, "live model"},
		"no mode":      {func(e *Evidence) { e.Mode = "" }, "live model"},
		"budget": {func(e *Evidence) { e.BudgetExhausted = true },
			"budget"},
		"forbidden": {func(e *Evidence) { e.Forbidden = 1 }, "forbidden"},
		"safe pass drop": {func(e *Evidence) {
			e.OverrideSafePass = Proportion{K: 39, N: 50}
		}, "Safe Pass"},
		"no baseline": {func(e *Evidence) { e.BaselineSafePass = Proportion{} },
			"Safe Pass"},
		"invalid overrides": {func(e *Evidence) { e.Overrides = Proportion{K: 5, N: 3} },
			"invalid"},
		"negative overrides": {func(e *Evidence) { e.Overrides = Proportion{K: -1, N: 3} },
			"invalid"},
		"invalid safe pass": {func(e *Evidence) {
			e.OverrideSafePass = Proportion{K: 60, N: 50}
		}, "invalid"},
	} {
		e := base
		c.mutate(&e)
		v := OverrideRule(e)
		if v.Eligible {
			t.Errorf("%s: eligible, want advisory", name)
		}
		if !strings.Contains(v.Reason, c.reason) {
			t.Errorf("%s: reason %q, want it to mention %q", name, v.Reason, c.reason)
		}
	}
}

func TestOverrideRule_EqualSafePassIsNotADrop(t *testing.T) {
	e := eligible(40, 40)
	e.OverrideSafePass = e.BaselineSafePass
	if v := OverrideRule(e); !v.Eligible {
		t.Fatalf("equal Safe Pass must not block: %+v", v)
	}
}

func TestOverrideRule_ZeroEvidenceIsAdvisory(t *testing.T) {
	v := OverrideRule(Evidence{})
	if v.Eligible || v.Reason == "" {
		t.Fatalf("zero evidence = %+v", v)
	}
}
