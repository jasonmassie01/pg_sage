package verify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (e *Engine) Watch(ctx context.Context, request WatchRequest) (Verdict, error) {
	// Before Create no durable watch exists, so nothing would ever verify the
	// action: revert (ErrStateNotPersisted). After Create, every failure
	// leaves the stored watch for the next resume to observe again.
	if err := ctx.Err(); err != nil {
		return contextFailure(), fmt.Errorf("%w: %w", ErrStateNotPersisted, err)
	}
	state := WatchStateFromRequest(request)
	e.applyDefaults(&state)
	e.freezeWeights(ctx, &state)
	if err := e.store.Create(ctx, state); err != nil {
		return persistenceFailure(), fmt.Errorf("create verification state: %w: %w",
			ErrStateNotPersisted, err)
	}
	return e.evaluateAndPersist(ctx, state)
}

func (e *Engine) ResumeDue(ctx context.Context) ([]Verdict, error) {
	results, err := e.ResumeDueResults(ctx)
	if results == nil {
		return nil, err
	}
	verdicts := make([]Verdict, 0, len(results))
	for _, result := range results {
		verdicts = append(verdicts, result.Verdict)
	}
	return verdicts, err
}

// ResumeDueResults evaluates due watches while preserving their durable IDs.
// One watch's error does not end the pass: the errors are joined at the end.
func (e *Engine) ResumeDueResults(ctx context.Context) ([]ResumeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := e.options.Now()
	listed, err := e.store.ListDue(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("list due verification states: %w", err)
	}
	results := make([]ResumeResult, 0, len(listed))
	var errs []error
	for _, candidate := range listed {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		result, claimed, err := e.resumeOne(ctx, candidate, now)
		if claimed {
			results = append(results, result)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return results, errors.Join(errs...)
}

func (e *Engine) resumeOne(
	ctx context.Context, candidate WatchState, now time.Time,
) (ResumeResult, bool, error) {
	// A listed row is only a candidate: claim it durably before acting so
	// concurrent workers (in this or another process) act on it once.
	state, claimed, err := e.store.Claim(ctx, candidate, now, now.Add(RevertRetryInterval))
	if err != nil {
		return ResumeResult{}, false, fmt.Errorf("claim verification for action %d: %w",
			candidate.ActionID, err)
	}
	if !claimed {
		return ResumeResult{}, false, nil
	}
	result := ResumeResult{WatchID: state.ID, ActionID: state.ActionID}
	if revertPending(state) {
		result.Verdict = pendingRevertVerdict(state)
		return result, true, nil
	}
	result.Verdict, err = e.evaluateAndPersist(ctx, state)
	return result, true, err
}

func (e *Engine) evaluateAndPersist(
	ctx context.Context, state WatchState,
) (Verdict, error) {
	verdict, evaluateErr := e.evaluate(ctx, state)
	next := state
	next.Status = verdict.Status
	next.Reason = verdict.Reason
	// A revert verdict is a decision, not a completed effect: the watch stays
	// open (and is retried) until the executor confirms the revert ran.
	next.Completed = verdict.Status != "extended" && !verdict.Revert
	next.Window = verdict.Window
	next.NextEvaluationAt = verdict.NextEvaluationAt
	next.ObservationFailures = verdict.ObservationFailures
	if verdict.AlphaSpend != nil {
		next.AlphaSpend = *verdict.AlphaSpend
	}
	if verdict.Other != nil {
		next.Other = verdict.Other
	}
	if verdict.Revert {
		next.NextEvaluationAt = e.options.Now().Add(RevertRetryInterval)
	}
	if err := e.store.Update(ctx, next); err != nil {
		// The watch row exists and is unchanged, so the next resume observes
		// it again: neither revert nor retain on a verdict that is not stored.
		return updateFailure(state, verdict), errors.Join(evaluateErr,
			fmt.Errorf("update verification state for action %d: %w", state.ActionID, err))
	}
	return verdict, evaluateErr
}

func (e *Engine) applyDefaults(state *WatchState) {
	criterion := &state.Criterion
	if criterion.Window <= 0 {
		criterion.Window = e.options.InitialWindow
	}
	if criterion.HardMax <= 0 {
		criterion.HardMax = e.options.HardMax
	}
	if criterion.MinGainPct <= 0 {
		criterion.MinGainPct = e.options.MinGainPct
	}
	if criterion.RegressPct <= 0 {
		criterion.RegressPct = e.options.RegressPct
	}
	if criterion.WriteImpactPct <= 0 {
		criterion.WriteImpactPct = e.options.WriteImpactPct
	}
	if state.Window <= 0 {
		state.Window = criterion.Window
	}
	state.NextEvaluationAt = state.ExecutedAt.Add(state.Window)
}

func contextFailure() Verdict {
	return Verdict{Revert: true, Status: "unverifiable", Reason: "context_canceled"}
}

func persistenceFailure() Verdict {
	return Verdict{Revert: true, Status: "unverifiable", Reason: "state_persist_failed"}
}

func updateFailure(stored WatchState, computed Verdict) Verdict {
	return Verdict{
		Status: "extended", Reason: "state_update_failed", Samples: computed.Samples,
		Window: stored.Window, NextEvaluationAt: stored.NextEvaluationAt,
		ObservationFailures: stored.ObservationFailures,
	}
}

// RevertRetryInterval is how long a revert verdict waits before it is
// handed back to the executor again when its revert has not completed. It is
// also the claim lease: a worker owns a claimed watch until then.
const RevertRetryInterval = 5 * time.Minute

func revertPending(state WatchState) bool {
	if state.Completed {
		return false
	}
	switch state.Status {
	case "revert", "reverted", "unverifiable":
		return true
	default:
		return false
	}
}

// pendingRevertVerdict returns the stored revert decision without observing
// again. The claim already rescheduled the next retry (its lease) in case
// this attempt fails too.
func pendingRevertVerdict(state WatchState) Verdict {
	verdict := Verdict{
		Revert: true, Status: "reverted", Reason: state.Reason, Window: state.Window,
	}
	if state.Status == "unverifiable" {
		verdict.Status = "unverifiable"
	}
	return verdict
}
