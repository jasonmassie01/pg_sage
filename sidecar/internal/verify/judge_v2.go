package verify

import (
	"math"
	"time"
)

// JudgeV2 (lab-01c) turns verification v2's evidence into a verdict with
// the same fields as v1's. In order: an up-front unverifiable step is
// unverifiable; an unmeasured attribution or a contaminated window is
// insufficient evidence; a CI above 0 beyond regress_pct on blocks per
// call, or the time guardrail, is a regression (no mechanism needed to
// revert); without the mechanism nothing else is judged; a CI below 0
// with the effect at or beyond min_gain_pct is improved; a CI inside
// both bars is no change (neutral, kept: v2 does not revert an index for
// being neutral); any other CI is too noisy. Insufficient evidence
// extends the window until the last look, which reverts as unverifiable,
// as v1 does at HardMax.

// V2Evidence is what one v2 look judges.
type V2Evidence struct {
	Attribution   Attribution
	Mechanism     Mechanism
	Contamination Contamination
	Look          Look
	// Preflight is the up-front answer when the step had one (nil: none).
	Preflight *Preflight
}

// Reasons of v2 verdicts.
const (
	ReasonV2NoChange      = "no_change"
	ReasonV2Regression    = "query_regression"
	ReasonV2TimeGuardrail = "time_guardrail"
	ReasonV2TooNoisy      = "too_noisy"
)

// JudgeV2 judges one look of state.
func JudgeV2(state WatchState, ev V2Evidence) Verdict {
	v := judgeV2Outcome(state, ev)
	if v.Window == 0 {
		v.Window = state.Window
	}
	v.Evidence = map[string]any{"v2": v2Record(ev, v)}
	return v
}

func judgeV2Outcome(state WatchState, ev V2Evidence) Verdict {
	a, c := ev.Attribution, state.Criterion
	gain, regress := -math.Abs(c.MinGainPct), math.Abs(c.RegressPct)
	switch {
	case ev.Preflight != nil && ev.Preflight.Blocks():
		return Verdict{Status: "unverifiable", Outcome: OutcomeUnverifiable,
			Reason: ev.Preflight.Reason}
	case !a.Measured:
		return v2Insufficient(state, ev.Look, joinReasons("not measured", []string{a.Reason}))
	case ev.Contamination.Contaminated:
		return v2Insufficient(state, ev.Look, ev.Contamination.Reason)
	case a.CILow > 0 && roundPct(a.EffectPct) > regress:
		return v2Decided(revertVerdict(state.Window, 0, ReasonV2Regression),
			OutcomeRegressed, a)
	case a.Guardrail.Regressed:
		return v2Decided(revertVerdict(state.Window, 0, ReasonV2TimeGuardrail),
			OutcomeRegressed, a)
	case !ev.Mechanism.Passed():
		return v2Insufficient(state, ev.Look, joinReasons(ev.Mechanism.Reason,
			[]string{ev.Mechanism.Detail}))
	case a.CIHigh < 0 && roundPct(a.EffectPct) <= gain:
		return v2Decided(Verdict{Retain: true, Status: "success"}, OutcomeImproved, a)
	case a.CILow >= math.Log1p(gain/100) && a.CIHigh <= math.Log1p(regress/100):
		return v2Decided(Verdict{Retain: true, Status: "success",
			Reason: ReasonV2NoChange}, OutcomeNeutral, a)
	}
	return v2Insufficient(state, ev.Look, ReasonV2TooNoisy)
}

// roundPct rounds to 1e-6 points, so a boundary written as a percentage
// compares exactly (as the attribution's guardrail does).
func roundPct(p float64) float64 { return math.Round(p*1e6) / 1e6 }

func v2Decided(v Verdict, outcome string, a Attribution) Verdict {
	v.Outcome = outcome
	effect := a.EffectPct
	v.ObservedPct = &effect
	return v
}

// v2Insufficient extends the window (doubling, capped at HardMax) or, at
// the last look, reverts as unverifiable.
func v2Insufficient(state WatchState, look Look, reason string) Verdict {
	if look.Final || state.Criterion.HardMax <= 0 || state.Window >= state.Criterion.HardMax {
		v := revertVerdict(state.Window, 0, OutcomeInsufficient+": "+reason)
		v.Status, v.Outcome = "unverifiable", OutcomeInsufficient
		return v
	}
	window := min(state.Window*2, state.Criterion.HardMax)
	return Verdict{Status: "extended", Outcome: OutcomeInsufficient, Reason: reason,
		Window: window, NextEvaluationAt: state.ExecutedAt.Add(window)}
}

// v2Record is the v2 evidence: the attribution, the mechanism, the CUSUM
// scan and the look's alpha, with the verdict they gave.
func v2Record(ev V2Evidence, v Verdict) map[string]any {
	a := ev.Attribution
	pct := func(x float64) any { return finite(math.Expm1(x) * 100) }
	record := map[string]any{"verifier": VerifierV2, "method": a.Method, "basis": a.Basis,
		"measured": a.Measured, "effect_pct": finite(a.EffectPct),
		"ci_low_pct": pct(a.CILow), "ci_high_pct": pct(a.CIHigh), "controls": a.Controls,
		"seasonal_weeks": a.SeasonalWeeks, "attribution_reason": a.Reason,
		"guardrail": map[string]any{"measured": a.Guardrail.Measured,
			"effect_pct": finite(a.Guardrail.EffectPct), "regressed": a.Guardrail.Regressed},
		"mechanism": ev.Mechanism.Evidence(), "cusum": ev.Contamination.Evidence(),
		"alpha": map[string]any{"look": ev.Look.Index, "alpha": ev.Look.Alpha,
			"spent": ev.Look.Spent, "fraction": ev.Look.Fraction, "final": ev.Look.Final},
		"outcome": v.Outcome, "reason": v.Reason, "status": v.Status}
	if ev.Preflight != nil {
		record["preflight"] = ev.Preflight.Evidence()
	}
	if v.Status == "extended" {
		record["next_window_hours"] = v.Window.Hours()
		record["next_evaluation_at"] = v.NextEvaluationAt.UTC().Format(time.RFC3339)
	}
	return record
}
