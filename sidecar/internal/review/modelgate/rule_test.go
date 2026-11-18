package modelgate

import (
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/modellift"
)

// The rules are pure functions of the evidence and the thresholds. No
// concurrent access tests here: they share nothing.

func p(k, n int) modellift.Proportion { return modellift.Proportion{K: k, N: n} }

// passing is the plan's happy path: 50 reviews, the model 40 of 50
// improved and the generators 45 of 50, no model reverts.
func passing() Evidence {
	return Evidence{Model: "m1", Reviews: p(50, 50), ModelImproved: p(40, 50),
		GeneratorImproved: p(45, 50), ModelReverts: p(0, 50), ModelOnlyAccepted: p(3, 20)}
}

func TestEvaluateM1_HappyPathIsEligibleWithTheNumbers(t *testing.T) {
	v := EvaluateM1(passing(), DefaultThresholds())
	if !v.Eligible || v.Rule != RuleM1 {
		t.Fatalf("verdict = %+v, want G-M1 eligible", v)
	}
	for _, want := range []string{"50 of 50 reviews", "model 40/50 improved (80.0%)",
		"generators 45/50 (90.0%)", "gap 10.00 points <= 10.00",
		"reverts 0/50 (0.00%) <= 2.00%"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q lacks %q", v.Reason, want)
		}
	}
	if v.ModelRate == nil || *v.ModelRate != 0.8 || v.GeneratorRate == nil ||
		*v.GeneratorRate != 0.9 || v.RevertRate == nil || *v.RevertRate != 0 {
		t.Fatalf("rates = %v %v %v", v.ModelRate, v.GeneratorRate, v.RevertRate)
	}
	lo, hi := modellift.Wilson(40, 50)
	if v.ModelInterval == nil || v.ModelInterval.Lo != lo || v.ModelInterval.Hi != hi {
		t.Fatalf("model interval = %+v, want the Wilson interval beside the rate",
			v.ModelInterval)
	}
}

func TestEvaluateM1_InvalidCountsAreNeverEligible(t *testing.T) {
	for name, mutate := range map[string]func(*Evidence){
		"model K > N":     func(e *Evidence) { e.ModelImproved = p(51, 50) },
		"generator K < 0": func(e *Evidence) { e.GeneratorImproved = p(-1, 50) },
		"reverts K > N":   func(e *Evidence) { e.ModelReverts = p(2, 1) },
		"reviews N < 0":   func(e *Evidence) { e.Reviews = p(0, -1) },
		"model only bad":  func(e *Evidence) { e.ModelOnlyAccepted = p(4, 3) },
	} {
		e := passing()
		mutate(&e)
		v := EvaluateM1(e, DefaultThresholds())
		if v.Eligible || !strings.Contains(v.Reason, "invalid counts") {
			t.Errorf("%s: %+v, want not eligible with invalid counts", name, v)
		}
		if v2 := EvaluateM2(e, DefaultThresholds()); v2.Eligible {
			t.Errorf("%s: G-M2 dropped the model on invalid counts", name)
		}
	}
}

func TestEvaluateM1_InvalidThresholdsAreNeverEligible(t *testing.T) {
	for name, th := range map[string]Thresholds{
		"zero reviews":    {MinReviews: 0, MaxGapPoints: 10, MaxRevertPct: 2},
		"negative gap":    {MinReviews: 50, MaxGapPoints: -1, MaxRevertPct: 2},
		"NaN revert":      {MinReviews: 50, MaxGapPoints: 10, MaxRevertPct: math.NaN()},
		"over 100 revert": {MinReviews: 50, MaxGapPoints: 10, MaxRevertPct: 101},
	} {
		if v := EvaluateM1(passing(), th); v.Eligible ||
			!strings.Contains(v.Reason, "invalid thresholds") {
			t.Errorf("%s: %+v, want invalid thresholds", name, v)
		}
	}
}

func TestEvaluateM1_NothingToJudge(t *testing.T) {
	th := DefaultThresholds()
	v := EvaluateM1(Evidence{Model: "m1", Reviews: p(0, 50)}, th)
	if v.Eligible || !strings.Contains(v.Reason, "0 of 50 reviews") {
		t.Fatalf("no reviews: %+v", v)
	}
	e := passing()
	e.ModelImproved, e.ModelReverts = p(0, 0), p(0, 0)
	if v := EvaluateM1(e, th); v.Eligible ||
		!strings.Contains(v.Reason, "no decided model steps") || v.ModelRate != nil {
		t.Fatalf("no decided model steps: %+v", v)
	}
	e = passing()
	e.GeneratorImproved = p(0, 0)
	if v := EvaluateM1(e, th); v.Eligible ||
		!strings.Contains(v.Reason, "no decided generator steps") {
		t.Fatalf("no decided generator steps: %+v", v)
	}
	e = passing()
	e.Model = "  "
	if v := EvaluateM1(e, th); v.Eligible || !strings.Contains(v.Reason, "no model") {
		t.Fatalf("no model: %+v", v)
	}
}

func TestEvaluateM1_ReviewBoundary(t *testing.T) {
	th := DefaultThresholds()
	e := passing()
	e.Reviews = p(49, 50)
	if v := EvaluateM1(e, th); v.Eligible || !strings.Contains(v.Reason, "49 of 50 reviews") {
		t.Fatalf("49 reviews: %+v", v)
	}
	e.Reviews = p(50, 50)
	if v := EvaluateM1(e, th); !v.Eligible {
		t.Fatalf("50 reviews: %+v", v)
	}
}

func TestEvaluateM1_GapBoundary(t *testing.T) {
	th := DefaultThresholds()
	e := passing()
	e.ModelImproved, e.GeneratorImproved = p(8000, 10000), p(9000, 10000) // 10.00 points
	e.ModelReverts = p(0, 10000)
	if v := EvaluateM1(e, th); !v.Eligible {
		t.Fatalf("gap 10.00: %+v", v)
	}
	e.GeneratorImproved = p(9001, 10000) // 10.01 points
	if v := EvaluateM1(e, th); v.Eligible ||
		!strings.Contains(v.Reason, "gap 10.01 points > 10.00") {
		t.Fatalf("gap 10.01: %+v", v)
	}
	// A model better than the generators is no gap at all.
	e.ModelImproved = p(9500, 10000)
	if v := EvaluateM1(e, th); !v.Eligible {
		t.Fatalf("model ahead: %+v", v)
	}
}

func TestEvaluateM1_RevertBoundary(t *testing.T) {
	th := DefaultThresholds()
	e := passing()
	e.ModelImproved, e.GeneratorImproved = p(45, 50), p(45, 50)
	e.ModelReverts = p(1, 50) // exactly 2%
	if v := EvaluateM1(e, th); !v.Eligible {
		t.Fatalf("2%% reverts: %+v", v)
	}
	e.ModelImproved, e.GeneratorImproved = p(9000, 10000), p(9000, 10000)
	e.ModelReverts = p(201, 10000) // 2.01%
	if v := EvaluateM1(e, th); v.Eligible || !strings.Contains(v.Reason, "reverts 201/10000") {
		t.Fatalf("2.01%% reverts: %+v", v)
	}
}

func TestEvaluateM2_DropsOnlyWithNoModelOnlyAcceptedSteps(t *testing.T) {
	th := DefaultThresholds()
	e := passing()
	e.ModelOnlyAccepted = p(0, 7)
	v := EvaluateM2(e, th)
	if !v.Eligible || v.Rule != RuleM2 || !strings.Contains(v.Reason, "0 accepted steps") {
		t.Fatalf("no model-only steps: %+v, want the pass dropped", v)
	}
	e.ModelOnlyAccepted = p(1, 7)
	if v := EvaluateM2(e, th); v.Eligible || !strings.Contains(v.Reason, "1 accepted step") {
		t.Fatalf("one model-only step: %+v, want the model pass kept", v)
	}
	e.ModelOnlyAccepted, e.Reviews = p(0, 0), p(49, 50)
	if v := EvaluateM2(e, th); v.Eligible || !strings.Contains(v.Reason, "49 of 50 reviews") {
		t.Fatalf("49 reviews: %+v, want the model pass kept", v)
	}
}

func TestThresholdsFrom_DefaultConfigIsTheProposal(t *testing.T) {
	got := ThresholdsFrom(config.DefaultConfig().Review.Gate)
	if got != DefaultThresholds() || got.MinReviews != 50 || got.MaxGapPoints != 10 ||
		got.MaxRevertPct != 2 {
		t.Fatalf("thresholds = %+v, want 50, 10 and 2", got)
	}
}
