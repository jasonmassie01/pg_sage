package verify

import (
	"context"
	"errors"
	"fmt"
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
	states, err := e.store.ListDue(ctx, e.options.Now())
	if err != nil {
		return nil, fmt.Errorf("list due verification states: %w", err)
	}
	results := make([]ResumeResult, 0, len(states))
	for _, state := range states {
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
	state.Completed = verdict.Status != "extended"
	state.Window = verdict.Window
	state.NextEvaluationAt = verdict.NextEvaluationAt
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
