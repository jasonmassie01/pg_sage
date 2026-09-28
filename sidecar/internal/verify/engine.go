package verify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (e *Engine) Watch(ctx context.Context, request WatchRequest) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return contextFailure(), err
	}
	state := WatchStateFromRequest(request)
	e.applyDefaults(&state)
	if err := e.store.Create(ctx, state); err != nil {
		return persistenceFailure(), fmt.Errorf("create verification state: %w", err)
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
	for _, candidate := range listed {
		// A listed row is only a candidate: claim it durably before acting so
		// concurrent workers (in this or another process) act on it once.
		state, claimed, err := e.store.Claim(ctx, candidate, now, now.Add(RevertRetryInterval))
		if err != nil {
			return results, fmt.Errorf("claim verification for action %d: %w",
				candidate.ActionID, err)
		}
		if !claimed {
			continue
		}
		if revertPending(state) {
			results = append(results, ResumeResult{
				WatchID: state.ID, ActionID: state.ActionID,
				Verdict: pendingRevertVerdict(state),
			})
			continue
		}
		verdict, evaluateErr := e.evaluateAndPersist(ctx, state)
		results = append(results, ResumeResult{
			WatchID: state.ID, ActionID: state.ActionID, Verdict: verdict,
		})
		if evaluateErr != nil {
			return results, evaluateErr
		}
	}
	return results, nil
}

func (e *Engine) evaluateAndPersist(
	ctx context.Context, state WatchState,
) (Verdict, error) {
	verdict, evaluateErr := e.evaluate(ctx, state)
	state.Status = verdict.Status
	state.Reason = verdict.Reason
	// A revert verdict is a decision, not a completed effect: the watch stays
	// open (and is retried) until the executor confirms the revert ran.
	state.Completed = verdict.Status != "extended" && !verdict.Revert
	state.Window = verdict.Window
	state.NextEvaluationAt = verdict.NextEvaluationAt
	if verdict.Revert {
		state.NextEvaluationAt = e.options.Now().Add(RevertRetryInterval)
	}
	if err := e.store.Update(ctx, state); err != nil {
		return persistenceFailure(), errors.Join(
			evaluateErr, fmt.Errorf("update verification state: %w", err),
		)
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
