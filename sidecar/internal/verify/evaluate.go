package verify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type observations struct {
	before      map[int64]Measurement
	after       map[int64]Measurement
	writeBefore Measurement
	writeAfter  Measurement
	indexValid  bool
}

func (e *Engine) evaluate(ctx context.Context, state WatchState) (Verdict, error) {
	if state.Criterion.Kind != "per_query_latency" {
		return observationFailure(), fmt.Errorf(
			"unsupported verification criterion %q", state.Criterion.Kind,
		)
	}
	observed, err := e.observe(ctx, state)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return contextFailure(), err
		}
		return observationFailure(), err
	}
	return e.decide(state, observed), nil
}

func (e *Engine) observe(ctx context.Context, state WatchState) (observations, error) {
	window := state.Window
	executedAt := state.ExecutedAt
	before, err := e.source.QueryMeasurements(
		ctx, state.Criterion.TargetIDs, executedAt.Add(-window), executedAt,
	)
	if err != nil {
		return observations{}, fmt.Errorf("read pre-action query measurements: %w", err)
	}
	after, err := e.source.QueryMeasurements(
		ctx, state.Criterion.TargetIDs, executedAt, e.options.Now(),
	)
	if err != nil {
		return observations{}, fmt.Errorf("read post-action query measurements: %w", err)
	}
	writeBefore, err := e.source.WriteMeasurements(
		ctx, state.Table, executedAt.Add(-window), executedAt,
	)
	if err != nil {
		return observations{}, fmt.Errorf("read pre-action write measurements: %w", err)
	}
	writeAfter, err := e.source.WriteMeasurements(ctx, state.Table, executedAt, e.options.Now())
	if err != nil {
		return observations{}, fmt.Errorf("read post-action write measurements: %w", err)
	}
	valid, err := e.source.IndexValid(ctx, state.IndexName)
	if err != nil {
		return observations{}, fmt.Errorf("validate index: %w", err)
	}
	return observations{before, after, writeBefore, writeAfter, valid}, nil
}

// decide judges the call-weighted mean of the targeted queries (Phase
// 1.3): a regression of any target, or of write latency, reverts; a
// neutral result reverts as no_gain (an index that does not help still
// costs writes); too little evidence extends the window up to its hard
// maximum; only an improvement is retained.
func (e *Engine) decide(state WatchState, observed observations) Verdict {
	if !observed.indexValid {
		verdict := revertVerdict(state.Window, 0, "invalid_index")
		verdict.Outcome = OutcomeUnverifiable
		return verdict
	}
	pooled, per := DecideTargets(observed.before, observed.after,
		state.Criterion.TargetIDs, e.thresholds(state.Criterion))
	samples := pooled.After.Samples
	var verdict Verdict
	switch {
	case pooled.Verdict == OutcomeInsufficient:
		verdict = e.insufficientVerdict(state, samples)
	case pooled.Verdict == OutcomeRegressed:
		verdict = revertVerdict(state.Window, samples, "query_regression")
	case writeRegressed(state.Criterion, observed):
		verdict = revertVerdict(state.Window, samples, "write_impact")
		pooled.Verdict = OutcomeRegressed
	case pooled.Verdict == OutcomeNeutral:
		verdict = revertVerdict(state.Window, samples, "no_gain")
	default:
		verdict = Verdict{Retain: true, Status: "success", Samples: samples,
			Window: state.Window}
	}
	return withOutcome(verdict, pooled, per)
}

func (e *Engine) thresholds(criterion Criterion) Thresholds {
	return Thresholds{MinSamples: e.options.MinSamples, MinBuckets: DefaultMinBuckets,
		GainPct: criterion.MinGainPct, RegressPct: criterion.RegressPct, Alpha: DefaultAlpha}
}

// withOutcome attaches the outcome verdict and its evidence. An extended
// window has no outcome yet.
func withOutcome(verdict Verdict, pooled Comparison, per []TargetComparison) Verdict {
	verdict.Evidence = map[string]any{"comparison": pooled.Evidence(),
		"targets": TargetsEvidence(per)}
	if verdict.Status == "extended" {
		return verdict
	}
	verdict.Outcome = pooled.Verdict
	if pooled.Verdict != OutcomeInsufficient {
		delta := pooled.DeltaPct
		verdict.ObservedPct = &delta
	}
	return verdict
}

func (e *Engine) insufficientVerdict(state WatchState, samples int) Verdict {
	if state.Window >= state.Criterion.HardMax {
		verdict := revertVerdict(state.Window, samples, "insufficient_samples")
		verdict.Status = "unverifiable"
		return verdict
	}
	window := state.Window * 2
	if window > state.Criterion.HardMax {
		window = state.Criterion.HardMax
	}
	return Verdict{
		Status: "extended", Reason: "insufficient_samples", Samples: samples,
		Window: window, NextEvaluationAt: state.ExecutedAt.Add(window),
	}
}

func writeRegressed(criterion Criterion, observed observations) bool {
	if observed.writeBefore.AverageLatency <= 0 {
		// No pre-action write baseline: insufficient evidence, not a regression.
		return false
	}
	return exceeds(observed.writeAfter.AverageLatency, observed.writeBefore.AverageLatency,
		criterion.WriteImpactPct)
}

func exceeds(after, before time.Duration, percent float64) bool {
	return float64(after)*100 > float64(before)*(100+percent)
}

func revertVerdict(window time.Duration, samples int, reason string) Verdict {
	return Verdict{Revert: true, Status: "reverted", Reason: reason, Samples: samples, Window: window}
}

func observationFailure() Verdict {
	return Verdict{Revert: true, Status: "unverifiable", Reason: "observation_unavailable"}
}
