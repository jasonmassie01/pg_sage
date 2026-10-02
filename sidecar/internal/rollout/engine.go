package rollout

import (
	"context"
	"fmt"
)

type Engine struct {
	verifier ReVerifier
	applier  Applier
}

func NewEngine(verifier ReVerifier, applier Applier) *Engine { return &Engine{verifier, applier} }

// applied is one changed instance, kept for a rollback.
type applied struct {
	instance Instance
	change   AppliedChange
}

// rolloutRun is one rollout's progress.
type rolloutRun struct {
	request        Request
	result         Result
	canaryOutcomes []Outcome
	changed        []applied
}

// Rollout applies the prior to the canary instances first, measures them,
// then widens one instance at a time. A failed verification, a canary
// aggregate regression or (after the canary) an instance regression past
// the limit halts it and rolls back every instance it changed, newest
// first. The blast-radius limit halts without a rollback.
func (engine *Engine) Rollout(ctx context.Context, request Request) (Result, error) {
	run := &rolloutRun{request: request, result: Result{Instances: map[string]InstanceResult{}}}
	if request.Prior.EvidenceID == "" || request.Prior.ValidatedInstanceID == "" ||
		len(request.Prior.Intent) == 0 {
		return run.result, ErrPriorRequired
	}
	if err := validatePolicy(request.Policy); err != nil {
		return run.result, err
	}
	for _, instance := range request.Instances {
		if err := ctx.Err(); err != nil {
			return run.result, err
		}
		if run.result.AppliedInstances >= request.Policy.MaxAffectedInstances {
			run.result.Halted, run.result.HaltReason = true, "blast_radius_limit"
			return run.result, nil
		}
		halt, err := engine.step(ctx, run, instance)
		if err != nil {
			return run.result, err
		}
		if halt != "" {
			run.result.Halted, run.result.HaltReason = true, halt
			engine.rollbackAll(ctx, run)
			return run.result, nil
		}
	}
	return run.result, nil
}

// step re-verifies, applies and measures one instance; it returns the
// halt reason when the instance must stop the rollout.
func (engine *Engine) step(ctx context.Context, run *rolloutRun, instance Instance) (string,
	error) {
	candidate, err := engine.verifier.Reverify(ctx, instance, run.request.Prior)
	if err != nil {
		return "", fmt.Errorf("local verifier for %s: %w", instance.ID, err)
	}
	if !candidate.Eligible {
		run.result.Instances[instance.ID] = InstanceResult{Status: StatusNotLocallyVerified,
			Detail: candidate.Reason}
		return "", nil
	}
	change, err := engine.applier.Apply(ctx, instance, candidate)
	if err != nil {
		return "", fmt.Errorf("apply to %s: %w", instance.ID, err)
	}
	run.result.AppliedInstances++
	run.changed = append(run.changed, applied{instance, change})
	outcome, err := engine.applier.Measure(ctx, instance, change)
	if err != nil {
		return "", fmt.Errorf("measure %s: %w", instance.ID, err)
	}
	run.result.Instances[instance.ID] = InstanceResult{Status: StatusApplied,
		EvidenceID: outcome.EvidenceID, RegressionPct: outcome.RegressionPct,
		Detail: outcome.Detail}
	return run.judge(instance, outcome), nil
}

// judge decides whether an instance's outcome halts the rollout.
func (run *rolloutRun) judge(instance Instance, outcome Outcome) string {
	policy := run.request.Policy
	if outcome.Failed {
		return "verification_failed"
	}
	if len(run.result.CanaryInstanceIDs) < policy.CanaryInstances {
		run.result.CanaryInstanceIDs = append(run.result.CanaryInstanceIDs, instance.ID)
		run.canaryOutcomes = append(run.canaryOutcomes, outcome)
		if len(run.canaryOutcomes) == policy.CanaryInstances {
			run.result.AggregateRegressionPct = averageRegression(run.canaryOutcomes)
			if run.result.AggregateRegressionPct > policy.AggregateRegressionLimitPct {
				return "aggregate_regression"
			}
		}
		return ""
	}
	if outcome.RegressionPct > policy.AggregateRegressionLimitPct {
		return "instance_regression"
	}
	return ""
}

// rollbackAll undoes every changed instance, newest first, on a context
// that outlives the caller's: a halt must not leave a partial rollout.
func (engine *Engine) rollbackAll(ctx context.Context, run *rolloutRun) {
	rollbacker, ok := engine.applier.(Rollbacker)
	rollbackCtx := context.WithoutCancel(ctx)
	for i := len(run.changed) - 1; i >= 0; i-- {
		c := run.changed[i]
		res := run.result.Instances[c.instance.ID]
		switch {
		case !ok:
			res.Status = StatusRollbackUnavailable
		default:
			if err := rollbacker.Rollback(rollbackCtx, c.instance, c.change); err != nil {
				res.Status = StatusRollbackFailed
				run.result.RollbackErrors = append(run.result.RollbackErrors,
					fmt.Sprintf("%s: %v", c.instance.ID, err))
			} else {
				res.Status = StatusRolledBack
				run.result.RolledBack++
			}
		}
		run.result.Instances[c.instance.ID] = res
	}
}

func averageRegression(outcomes []Outcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	var total float64
	for _, outcome := range outcomes {
		total += outcome.RegressionPct
	}
	return total / float64(len(outcomes))
}
