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

func (engine *Engine) Rollout(ctx context.Context, request Request) (Result, error) {
	result := Result{Instances: map[string]InstanceResult{}}
	if request.Prior.EvidenceID == "" || request.Prior.ValidatedInstanceID == "" ||
		len(request.Prior.Intent) == 0 {
		return result, ErrPriorRequired
	}
	if err := validatePolicy(request.Policy); err != nil {
		return result, err
	}
	canaryOutcomes := make([]Outcome, 0, request.Policy.CanaryInstances)
	for _, instance := range request.Instances {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.AppliedInstances >= request.Policy.MaxAffectedInstances {
			result.Halted, result.HaltReason = true, "blast_radius_limit"
			return result, nil
		}
		candidate, err := engine.verifier.Reverify(ctx, instance, request.Prior)
		if err != nil {
			return result, fmt.Errorf("local verifier for %s: %w", instance.ID, err)
		}
		if !candidate.Eligible {
			result.Instances[instance.ID] = InstanceResult{Status: "not_locally_verified"}
			continue
		}
		change, err := engine.applier.Apply(ctx, instance, candidate)
		if err != nil {
			return result, fmt.Errorf("apply to %s: %w", instance.ID, err)
		}
		result.AppliedInstances++
		outcome, err := engine.applier.Measure(ctx, instance, change)
		if err != nil {
			return result, fmt.Errorf("measure %s: %w", instance.ID, err)
		}
		result.Instances[instance.ID] = InstanceResult{Status: "applied", EvidenceID: outcome.EvidenceID}
		if len(result.CanaryInstanceIDs) < request.Policy.CanaryInstances {
			result.CanaryInstanceIDs = append(result.CanaryInstanceIDs, instance.ID)
			canaryOutcomes = append(canaryOutcomes, outcome)
			if len(canaryOutcomes) == request.Policy.CanaryInstances {
				result.AggregateRegressionPct = averageRegression(canaryOutcomes)
				if result.AggregateRegressionPct > request.Policy.AggregateRegressionLimitPct {
					result.Halted, result.HaltReason = true, "aggregate_regression"
					return result, nil
				}
			}
		}
	}
	return result, nil
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
