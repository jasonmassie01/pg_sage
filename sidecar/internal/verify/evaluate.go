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

func (e *Engine) decide(state WatchState, observed observations) Verdict {
	if !observed.indexValid {
		return revertVerdict(state.Window, 0, "invalid_index")
	}
	samples, sufficient := e.sampleCount(state.Criterion.TargetIDs, observed)
	if !sufficient {
		return e.insufficientVerdict(state, samples)
	}
	if anyRegression(state.Criterion, observed) {
		return revertVerdict(state.Window, samples, "query_regression")
	}
	if writeRegressed(state.Criterion, observed) {
		return revertVerdict(state.Window, samples, "write_impact")
	}
	if !anyGain(state.Criterion, observed) {
		return revertVerdict(state.Window, samples, "no_gain")
	}
	return Verdict{Retain: true, Status: "success", Samples: samples, Window: state.Window}
}

func (e *Engine) sampleCount(ids []int64, observed observations) (int, bool) {
	common := 0
	for _, id := range ids {
		before, beforeOK := observed.before[id]
		after, afterOK := observed.after[id]
		if common == 0 || after.Samples < common {
			common = after.Samples
		}
		if !beforeOK || !afterOK || before.Samples < e.options.MinSamples ||
			after.Samples < e.options.MinSamples || before.AverageLatency <= 0 {
			return common * len(ids), false
		}
	}
	return common * len(ids), len(ids) > 0
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

func anyRegression(criterion Criterion, observed observations) bool {
	for _, id := range criterion.TargetIDs {
		if exceeds(observed.after[id].AverageLatency, observed.before[id].AverageLatency,
			criterion.RegressPct) {
			return true
		}
	}
	return false
}

func anyGain(criterion Criterion, observed observations) bool {
	for _, id := range criterion.TargetIDs {
		before, after := observed.before[id], observed.after[id]
		if float64(after.AverageLatency)*100 <=
			float64(before.AverageLatency)*(100-criterion.MinGainPct) {
			return true
		}
	}
	return false
}

func writeRegressed(criterion Criterion, observed observations) bool {
	if observed.writeBefore.AverageLatency <= 0 {
		return observed.writeAfter.AverageLatency > 0
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
