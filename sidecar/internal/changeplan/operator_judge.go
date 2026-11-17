package changeplan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// OperatorJudge decides a landed operator step's verdict (plan-02d). It
// returns an ActionVerdict with no Verdict while undecided.
type OperatorJudge interface {
	JudgeOperator(ctx context.Context, s Step, now time.Time) (ActionVerdict, error)
}

// DefaultOperatorWindow is how long a measured operator step is watched
// after it landed before its one look, when no window is configured.
const DefaultOperatorWindow = 30 * time.Minute

// defaultOperatorRegressPct is the regression a measured operator step's
// look tests for when it declares no guardrail on its own metric.
const defaultOperatorRegressPct = 10.0

// itsJudge judges operator steps: a readback step by its landing (the
// landing check read the setting back); a measured step, one window after
// it landed, by verification v2's interrupted time series with no control
// group (a global setting moves every query), which its verdict says.
type itsJudge struct {
	v2     *verify.V2Options
	window time.Duration
}

// NewOperatorJudge judges with v2's readers (nil: measured steps are
// unverifiable) after window (0: DefaultOperatorWindow).
func NewOperatorJudge(v2 *verify.V2Options, window time.Duration) OperatorJudge {
	if window <= 0 {
		window = DefaultOperatorWindow
	}
	return itsJudge{v2: v2, window: window}
}

// JudgeOperator implements OperatorJudge.
func (j itsJudge) JudgeOperator(ctx context.Context, s Step, now time.Time) (ActionVerdict,
	error) {
	if s.Runtime.LandedAt == nil {
		return ActionVerdict{}, errors.New("the operator step has not landed")
	}
	landed := *s.Runtime.LandedAt
	if s.Metric == MetricReadback {
		return ActionVerdict{Verdict: "improved", DecidedAt: &landed,
			Detail: map[string]any{"judged_by": "readback",
				"reason": "the landing check read the setting back as the step expects"}}, nil
	}
	if j.v2 == nil || j.v2.Counters == nil || j.v2.Mechanisms == nil {
		return ActionVerdict{Verdict: "unverifiable", DecidedAt: &now,
			Detail: map[string]any{"judged_by": "none",
				"reason": "no verifier is configured for operator steps"}}, nil
	}
	if now.Before(landed.Add(j.window)) {
		return ActionVerdict{}, nil
	}
	state := verify.WatchState{ExecutedAt: landed, Window: j.window,
		Criterion: verify.Criterion{TargetIDs: s.TargetQueryIDs, RegressPct: regressPct(s),
			Window: j.window, HardMax: j.window}}
	v, _, err := verify.JudgeLookV2(ctx, *j.v2, verify.LookV2{State: state, Now: now,
		Class: verify.MechanismConfigGUC, Kind: verify.AttributionGlobal,
		Metric: string(s.Metric)})
	if err != nil {
		return ActionVerdict{}, fmt.Errorf("judge operator step %d: %w", s.Ordinal, err)
	}
	outcome := v.Outcome
	if outcome == "" {
		outcome = verify.OutcomeInsufficient
	}
	return ActionVerdict{Verdict: outcome, DecidedAt: &now, Detail: map[string]any{
		"judged_by": "interrupted_time_series", "attribution": verify.AttributionGlobal,
		"note": verify.ReasonGlobal, "reason": v.Reason, "observed_pct": v.ObservedPct,
		"window_minutes": int(j.window / time.Minute)}}, nil
}

// regressPct is the step's guardrail on its own metric, else the default.
func regressPct(s Step) float64 {
	for _, g := range s.Guardrails {
		if g.Metric == s.Metric {
			return g.MaxRegressPct
		}
	}
	return defaultOperatorRegressPct
}
