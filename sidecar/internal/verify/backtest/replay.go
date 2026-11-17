package backtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// assumedMechanism passes the mechanism gate for every placebo, counting
// all its targets. A placebo has no real index whose plan use could be
// checked; assuming the gate passes is the worst case for a false
// positive (the gate can only stop a verdict, never make one).
type assumedMechanism struct{}

func (assumedMechanism) CheckMechanism(_ context.Context, in verify.MechanismInput,
	_ time.Time) (verify.Mechanism, error) {
	return verify.Mechanism{Counted: slices.Clone(in.Targets)}, nil
}

// replayed is one placebo's four replays, each the outcome of the look
// that decided it (insufficient_evidence or unverifiable when none did).
type replayed struct {
	v2, v1, v2Injected, v1Injected string
	excluded                       bool
}

// replay runs v2 and v1 on the placebo, then on its injected copy.
func replay(ctx context.Context, src verify.V2Counters, p Placebo, cycle time.Duration) (
	replayed, error) {
	m := newMemo(src)
	inj := injected{src: m, at: p.At, targets: p.Targets}
	var r replayed
	var err error
	if r.v2, err = replayV2(ctx, m, p, cycle); err != nil {
		return r, err
	}
	if r.v1, err = replayV1(ctx, m, p); err != nil {
		return r, err
	}
	if r.v2Injected, err = replayV2(ctx, inj, p, cycle); err != nil {
		return r, err
	}
	if r.v1Injected, err = replayV1(ctx, inj, p); err != nil {
		return r, err
	}
	r.excluded = m.broke
	return r, nil
}

func idsOf(p Placebo) []int64 { return append(slices.Clone(p.Targets), p.Controls...) }

// lookErr names the look's window in a read failure.
func lookErr(p Placebo, window time.Duration, err error) error {
	return fmt.Errorf("look %s..%s: %w", p.At.Add(-window).Format(time.RFC3339),
		p.At.Add(window).Format(time.RFC3339), err)
}

// replayV2 runs v2's looks (verify.JudgeLookV2, as the index watch and the
// monitor do) until one decides: the outcome of that look.
func replayV2(ctx context.Context, src verify.V2Counters, p Placebo,
	cycle time.Duration) (string, error) {
	c := placeboCriterion
	c.TargetIDs = p.Targets
	state := verify.WatchState{ExecutedAt: p.At, Table: p.Table, Criterion: c,
		Window: c.Window}
	pre, err := src.Window(ctx, idsOf(p), p.At.Add(-c.Window), p.At, 0)
	if err != nil {
		return "", lookErr(p, c.Window, err)
	}
	if w := verify.FrozenWeights(pre.Series, p.Targets); len(w) > 0 {
		state.FrozenWeights = w
	}
	o := verify.V2Options{Counters: src, Mechanisms: assumedMechanism{}, Cycle: cycle,
		Controls: func(context.Context, verify.WatchState) ([]int64, error) {
			return p.Controls, nil
		}}
	class := verify.MechanismIndexCreate
	if p.Kind == verify.AttributionGlobal {
		class = verify.MechanismConfigGUC
	}
	for range len(verify.LookSchedule(c.Window, c.HardMax)) + 1 {
		v, spend, err := verify.JudgeLookV2(ctx, o, verify.LookV2{State: state,
			Now: p.At.Add(state.Window), Class: class, Kind: p.Kind})
		if err != nil {
			return "", lookErr(p, state.Window, err)
		}
		if v.Status != "extended" {
			return v.Outcome, nil
		}
		state.Window, state.AlphaSpend = v.Window, spend
	}
	return "", errors.New("verify: the look schedule did not end")
}

// v1Thresholds are v1's bars (verify.DefaultOptions).
var v1Thresholds = verify.Thresholds{MinSamples: verify.DefaultMinSamples,
	GainPct: verify.DefaultGainPct, RegressPct: verify.DefaultRegressPct,
	Alpha: verify.DefaultAlpha}

// replayV1 runs v1's comparison (verify.DecideTargets on time per call) at
// each window of the schedule until one decides.
func replayV1(ctx context.Context, src verify.V2Counters, p Placebo) (string, error) {
	c := placeboCriterion
	for _, w := range verify.LookSchedule(c.Window, c.HardMax) {
		pre, err := src.Window(ctx, idsOf(p), p.At.Add(-w), p.At, 0)
		if err != nil {
			return "", lookErr(p, w, err)
		}
		post, err := src.Window(ctx, idsOf(p), p.At, p.At.Add(w), 0)
		if err != nil {
			return "", lookErr(p, w, err)
		}
		pooled, _ := verify.DecideTargets(measurements(pre.Series, p.Targets),
			measurements(post.Series, p.Targets), p.Targets, v1Thresholds)
		if pooled.Verdict != verify.OutcomeInsufficient {
			return pooled.Verdict, nil
		}
	}
	return verify.OutcomeInsufficient, nil
}

func measurements(s verify.Series, ids []int64) map[int64]verify.Measurement {
	out := make(map[int64]verify.Measurement, len(ids))
	for _, id := range ids {
		ivs := make([]verify.Interval, 0, len(s[id]))
		for _, b := range s[id] {
			ivs = append(ivs, verify.Interval{Calls: b.Calls, TotalMs: b.TotalMs})
		}
		out[id] = verify.Summarize(ivs)
	}
	return out
}

// falsePositive is a verdict that would have kept or reverted a change.
func falsePositive(outcome string) bool {
	return outcome == verify.OutcomeImproved || outcome == verify.OutcomeRegressed
}

// inconclusive is a watch that never decided.
func inconclusive(outcome string) bool {
	return outcome == verify.OutcomeInsufficient || outcome == verify.OutcomeUnverifiable
}

// tally adds one placebo's outcomes to c.
func tally(c *Counts, excluded bool, null, inj string) {
	c.Placebos++
	switch {
	case excluded:
		c.Excluded++
		return
	case falsePositive(null):
		c.FalsePositives++
	case inconclusive(null):
		c.Inconclusive++
	}
	c.Injected++
	if inj == verify.OutcomeImproved {
		c.Detected++
	}
}
