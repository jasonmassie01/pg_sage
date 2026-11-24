package verify

import (
	"context"
	"fmt"
	"time"
)

type observations struct {
	before      map[int64]Measurement
	after       map[int64]Measurement
	writeBefore WriteCost
	writeAfter  WriteCost
	// writeErr is a failed write-cost read: recorded in the evidence, it
	// never fails the observation (the write evidence is then insufficient).
	writeErr   error
	indexValid bool
}

func (e *Engine) evaluate(ctx context.Context, state WatchState) (Verdict, error) {
	if state.Criterion.Kind != "per_query_latency" {
		return observationFailure(), fmt.Errorf(
			"unsupported verification criterion %q", state.Criterion.Kind,
		)
	}
	observed, err := e.observe(ctx, state)
	if err != nil {
		// A failed read is not evidence about the index: the error is
		// carried in the verdict, never returned for a caller to revert on.
		return e.retryVerdict(ctx, state, err), nil
	}
	return e.withV2(ctx, state, observed, e.decide(state, observed)), nil
}

// observationRetryDelays is the backoff after the 1st, 2nd and 3rd or later
// consecutive failed observation.
var observationRetryDelays = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour}

func observationRetryDelay(failures int) time.Duration {
	index := failures - 1
	if index < 0 {
		index = 0
	}
	if index >= len(observationRetryDelays) {
		index = len(observationRetryDelays) - 1
	}
	return observationRetryDelays[index]
}

// retryVerdict keeps a watch whose evidence could not be read open
// (observation_retry), next evaluated after a backoff that never passes the
// hard deadline. At the deadline it reverts (observation_unavailable): the
// change was never verified. A cancelled evaluation (shutdown) is not a
// failed observation and never reverts.
func (e *Engine) retryVerdict(ctx context.Context, state WatchState, cause error) Verdict {
	now := e.options.Now()
	failures := state.ObservationFailures
	if ctx.Err() == nil {
		failures++
		deadline := state.ExecutedAt.Add(state.Criterion.HardMax)
		if !now.Before(deadline) {
			verdict := revertVerdict(state.Window, 0, "observation_unavailable")
			verdict.Status = "unverifiable"
			verdict.ObservationFailures = failures
			return verdict
		}
		next := now.Add(observationRetryDelay(failures))
		if next.After(deadline) {
			next = deadline
		}
		return observationRetry(state.Window, cause, next, failures)
	}
	return observationRetry(state.Window, cause, now.Add(observationRetryDelay(1)), failures)
}

func observationRetry(
	window time.Duration, cause error, next time.Time, failures int,
) Verdict {
	return Verdict{
		Status: "extended", Reason: "observation_retry: " + cause.Error(), Window: window,
		NextEvaluationAt: next, ObservationFailures: failures,
	}
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
	writeBefore, writeAfter, writeErr := e.observeWrites(ctx, state)
	valid, err := e.source.IndexValid(ctx, state.IndexName)
	if err != nil {
		return observations{}, fmt.Errorf("validate index: %w", err)
	}
	return observations{before: before, after: after, writeBefore: writeBefore,
		writeAfter: writeAfter, writeErr: writeErr, indexValid: valid}, nil
}

// decide judges the call-weighted mean of the targeted queries (Phase
// 1.3): a regression of any target reverts; a neutral result reverts as
// no_gain (an index that does not help still costs writes); too little
// evidence extends the window up to its hard maximum; only an improvement
// is retained, unless its table's write cost rose above write_impact_pct
// (write_impact, executor-03b). The write signal is attached to every
// decided or extended verdict (write_signal).
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
	switch pooled.Verdict {
	case OutcomeInsufficient:
		verdict = e.insufficientVerdict(state, samples)
	case OutcomeRegressed:
		verdict = revertVerdict(state.Window, samples, "query_regression")
	case OutcomeNeutral:
		verdict = revertVerdict(state.Window, samples, "no_gain")
	default:
		verdict = Verdict{Retain: true, Status: "success", Samples: samples,
			Window: state.Window}
	}
	verdict = withOutcome(verdict, pooled, per)
	decisive := verdict.Retain &&
		judgeObservedWrites(state.Criterion, observed).Outcome == writeRegressed
	if decisive {
		verdict = writeImpactVerdict(verdict)
	}
	verdict.Evidence["write_signal"] = writeSignal(state, observed, decisive)
	return verdict
}

// writeImpactVerdict reverts an improving index whose table's writes now
// cost more than write_impact_pct: the action regressed the workload it
// shares a table with. ObservedPct stays the queries' change.
func writeImpactVerdict(retained Verdict) Verdict {
	verdict := revertVerdict(retained.Window, retained.Samples, "write_impact")
	verdict.Outcome = OutcomeRegressed
	verdict.ObservedPct, verdict.Evidence = retained.ObservedPct, retained.Evidence
	return verdict
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

func revertVerdict(window time.Duration, samples int, reason string) Verdict {
	return Verdict{Revert: true, Status: "reverted", Reason: reason, Samples: samples, Window: window}
}

func observationFailure() Verdict {
	return Verdict{Revert: true, Status: "unverifiable", Reason: "observation_unavailable"}
}
