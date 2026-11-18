// Package modelgate is Sage Review's model gate (review-05a), shaped like
// modellift's measured authority rule. G-M1 ("give the model more") passes
// when, over the latest min_reviews reviews on real databases whose model
// pass finished on the running model, the model's verified-improved rate is
// within max_gap_points of the generators' and its revert rate is at most
// max_revert_pct. A pass never promotes by itself: it files one autonomy
// promotion in the inbox, and only a person's approval sets review-03b's
// authority flag for that model id. A failed window or a model change
// revokes it at once. G-M2 ("drop the model pass") stops review.model_pass
// auto from calling a model that added no accepted step of its own over the
// same reviews; it only narrows. Rates are decided steps over decided
// steps; Wilson intervals are reported beside them, never instead of them.
package modelgate

import (
	"fmt"
	"math"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/modellift"
)

// The rules.
const (
	RuleM1 = "G-M1"
	RuleM2 = "G-M2"
)

// epsilon absorbs float rounding at an exact boundary (10.00 points, 2%).
const epsilon = 1e-9

// Thresholds are review.gate's (decision D11's proposals by default).
type Thresholds struct {
	MinReviews   int     `json:"min_reviews"`
	MaxGapPoints float64 `json:"max_gap_points"`
	MaxRevertPct float64 `json:"max_revert_pct"`
}

// DefaultThresholds is 50 reviews, 10 points and 2%.
func DefaultThresholds() Thresholds {
	return Thresholds{MinReviews: config.DefaultReviewGateMinReviews,
		MaxGapPoints: config.DefaultReviewGateMaxGapPoints,
		MaxRevertPct: config.DefaultReviewGateMaxRevertPct}
}

// ThresholdsFrom reads review.gate.
func ThresholdsFrom(c config.ReviewGateConfig) Thresholds {
	return Thresholds{MinReviews: c.MinReviews, MaxGapPoints: c.MaxGapPoints,
		MaxRevertPct: c.MaxRevertPct}
}

func (t Thresholds) valid() bool {
	in := func(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 100 }
	return t.MinReviews >= 1 && in(t.MaxGapPoints) && in(t.MaxRevertPct)
}

// Evidence is one model's measurement over the window. Reviews is the
// counted reviews (k) of the min_reviews needed (n); ModelImproved and
// GeneratorImproved are improved over decided steps; ModelReverts is
// reverted over decided model steps; ModelOnlyAccepted is accepted model
// steps no generator also proposed, over accepted model steps.
type Evidence struct {
	Model             string               `json:"model"`
	Reviews           modellift.Proportion `json:"reviews"`
	ModelImproved     modellift.Proportion `json:"model_improved"`
	GeneratorImproved modellift.Proportion `json:"generator_improved"`
	ModelReverts      modellift.Proportion `json:"model_reverts"`
	ModelOnlyAccepted modellift.Proportion `json:"model_only_accepted"`
}

func (e Evidence) valid() bool {
	for _, p := range []modellift.Proportion{e.Reviews, e.ModelImproved, e.GeneratorImproved,
		e.ModelReverts, e.ModelOnlyAccepted} {
		if !p.Valid() {
			return false
		}
	}
	return true
}

// Interval is a Wilson 95% interval.
type Interval struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// Verdict is a rule's decision with what it read. For G-M2, Eligible means
// "drop the model pass".
type Verdict struct {
	Rule              string    `json:"rule"`
	Eligible          bool      `json:"eligible"`
	Reason            string    `json:"reason"`
	ModelRate         *float64  `json:"model_rate,omitempty"`
	GeneratorRate     *float64  `json:"generator_rate,omitempty"`
	RevertRate        *float64  `json:"revert_rate,omitempty"`
	ModelInterval     *Interval `json:"model_interval,omitempty"`
	GeneratorInterval *Interval `json:"generator_interval,omitempty"`
	RevertInterval    *Interval `json:"revert_interval,omitempty"`
}

// precheck is why the evidence cannot be judged at all, or "".
func precheck(e Evidence, t Thresholds) string {
	switch {
	case !t.valid():
		return fmt.Sprintf("invalid thresholds %+v: not eligible", t)
	case !e.valid():
		return "invalid counts in the evidence: not eligible"
	case modellift.NormalizeModel(e.Model) == "":
		return "no model to judge: not eligible"
	case e.Reviews.K < t.MinReviews:
		return fmt.Sprintf("%d of %d reviews on real databases with a finished model pass",
			e.Reviews.K, t.MinReviews)
	}
	return ""
}

// EvaluateM1 decides G-M1. Every condition is required; the reason names
// every number it read.
func EvaluateM1(e Evidence, t Thresholds) Verdict {
	v := Verdict{Rule: RuleM1}
	if why := precheck(e, t); why != "" {
		v.Reason = why
		return v
	}
	m, g, r := e.ModelImproved, e.GeneratorImproved, e.ModelReverts
	switch {
	case m.N == 0:
		v.Reason = fmt.Sprintf("%d of %d reviews; no decided model steps", e.Reviews.K,
			t.MinReviews)
		return v
	case g.N == 0:
		v.Reason = fmt.Sprintf("%d of %d reviews; no decided generator steps to compare "+
			"with", e.Reviews.K, t.MinReviews)
		return v
	}
	v.ModelRate, v.ModelInterval = rate(m)
	v.GeneratorRate, v.GeneratorInterval = rate(g)
	v.RevertRate, v.RevertInterval = rate(r)
	gap := 100 * float64(g.K*m.N-m.K*g.N) / float64(g.N*m.N)
	reverts := 0.0
	if r.N > 0 {
		reverts = 100 * float64(r.K) / float64(r.N)
	}
	gapOK, revertOK := gap <= t.MaxGapPoints+epsilon, reverts <= t.MaxRevertPct+epsilon
	v.Eligible = gapOK && revertOK
	v.Reason = fmt.Sprintf("%d of %d reviews; model %d/%d improved (%.1f%%) vs generators "+
		"%d/%d (%.1f%%), gap %.2f points %s %.2f; reverts %d/%d (%.2f%%) %s %.2f%%",
		e.Reviews.K, t.MinReviews, m.K, m.N, 100**v.ModelRate, g.K, g.N, 100**v.GeneratorRate,
		math.Max(gap, 0), cmp(gapOK), t.MaxGapPoints, r.K, r.N, reverts, cmp(revertOK),
		t.MaxRevertPct)
	return v
}

func cmp(ok bool) string {
	if ok {
		return "<="
	}
	return ">"
}

func rate(p modellift.Proportion) (*float64, *Interval) {
	r, ok := p.Rate()
	if !ok {
		return nil, nil
	}
	lo, hi := modellift.Wilson(p.K, p.N)
	return &r, &Interval{Lo: lo, Hi: hi}
}

// EvaluateM2 decides G-M2: Eligible (drop the model pass) only when the
// window is full and no accepted step was the model's alone.
func EvaluateM2(e Evidence, t Thresholds) Verdict {
	v := Verdict{Rule: RuleM2}
	if why := precheck(e, t); why != "" {
		v.Reason = why + "; the model pass stays on"
		return v
	}
	n := e.ModelOnlyAccepted.K
	if n > 0 {
		v.Reason = fmt.Sprintf("%d accepted %s only the model proposed in %d reviews; the "+
			"model pass stays on", n, plural(n, "step", "steps"), e.Reviews.K)
		return v
	}
	v.Eligible = true
	v.Reason = fmt.Sprintf("0 accepted steps only the model proposed in %d reviews (%d "+
		"accepted model steps a generator also proposed): review.model_pass auto stops "+
		"calling %s", e.Reviews.K, e.ModelOnlyAccepted.N, modellift.NormalizeModel(e.Model))
	return v
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// summary is a verdict's one line for the inbox and the digest.
func summary(v Verdict) string {
	return strings.TrimSpace(v.Rule + ": " + v.Reason)
}
