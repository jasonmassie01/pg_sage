package rollout

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

type PriorEvidence struct {
	Prior       Prior
	Verdict     string
	CompletedAt time.Time
}

type RunRecord struct {
	EvidenceID             string
	SourceInstance         string
	PriorEvidenceID        string
	Policy                 Policy
	State                  string
	HaltReason             string
	AppliedInstances       int
	CanaryInstanceIDs      []string
	AggregateRegressionPct float64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type RuntimeOptions struct {
	MinVerifiedSuccesses int
	TriggerInterval      time.Duration
}

type RuntimeEngine interface {
	Rollout(context.Context, Request) (Result, error)
}

type RuntimeEvidenceSource interface {
	ListPriorEvidence(context.Context) ([]PriorEvidence, error)
}

type RuntimeCohortSource interface {
	InstancesForPrior(context.Context, Prior) ([]Instance, error)
}

type RuntimeRunStore interface {
	Create(context.Context, RunRecord) error
	Update(context.Context, RunRecord) error
	Latest(context.Context) (RunRecord, bool, error)
}

type RuntimeDependencies struct {
	Engine   RuntimeEngine
	Evidence RuntimeEvidenceSource
	Cohort   RuntimeCohortSource
	Policies PolicySet
	Runs     RuntimeRunStore
	Now      func() time.Time
}

type Runtime struct {
	dependencies RuntimeDependencies
	options      RuntimeOptions
}

func NewRuntime(dependencies RuntimeDependencies, options RuntimeOptions) *Runtime {
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if options.MinVerifiedSuccesses <= 0 {
		options.MinVerifiedSuccesses = 1
	}
	if options.TriggerInterval <= 0 {
		options.TriggerInterval = time.Hour
	}
	return &Runtime{dependencies: dependencies, options: options}
}

func (runtime *Runtime) RunOnce(ctx context.Context) (RunRecord, error) {
	if err := runtime.ready(ctx); err != nil {
		return RunRecord{}, err
	}
	prior, err := runtime.selectPrior(ctx)
	if err != nil {
		return RunRecord{}, err
	}
	instances, err := runtime.dependencies.Cohort.InstancesForPrior(ctx, prior)
	if err != nil {
		return RunRecord{}, fmt.Errorf("select rollout cohort: %w", err)
	}
	policy, err := runtime.resolvePolicy(instances)
	if err != nil {
		return RunRecord{}, err
	}
	record := runtime.newRecord(prior, policy)
	if err := runtime.dependencies.Runs.Create(ctx, record); err != nil {
		return record, fmt.Errorf("create rollout run: %w", err)
	}
	result, rolloutErr := runtime.dependencies.Engine.Rollout(ctx, Request{
		Prior: prior, Instances: instances, Policy: policy,
	})
	runtime.finalizeRecord(&record, result, rolloutErr)
	if err := runtime.dependencies.Runs.Update(context.WithoutCancel(ctx), record); err != nil {
		rolloutErr = errors.Join(rolloutErr, fmt.Errorf("update rollout run: %w", err))
	}
	return record, rolloutErr
}

func (runtime *Runtime) RunDue(ctx context.Context) (bool, error) {
	if err := runtime.ready(ctx); err != nil {
		return false, err
	}
	latest, found, err := runtime.dependencies.Runs.Latest(ctx)
	if err != nil {
		return false, fmt.Errorf("load latest rollout run: %w", err)
	}
	now := runtime.dependencies.Now()
	if found && now.Sub(latest.UpdatedAt) < runtime.options.TriggerInterval {
		return false, nil
	}
	evidence, err := runtime.dependencies.Evidence.ListPriorEvidence(ctx)
	if err != nil {
		return false, fmt.Errorf("list verified successes: %w", err)
	}
	if countVerifiedSince(evidence, latest.UpdatedAt) < runtime.options.MinVerifiedSuccesses {
		return false, nil
	}
	_, err = runtime.RunOnce(ctx)
	return err == nil, err
}

func (runtime *Runtime) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime == nil || runtime.dependencies.Engine == nil ||
		runtime.dependencies.Evidence == nil || runtime.dependencies.Cohort == nil ||
		runtime.dependencies.Runs == nil {
		return errors.New("rollout runtime dependencies are unavailable")
	}
	return nil
}

func (runtime *Runtime) selectPrior(ctx context.Context) (Prior, error) {
	evidence, err := runtime.dependencies.Evidence.ListPriorEvidence(ctx)
	if err != nil {
		return Prior{}, fmt.Errorf("list prior evidence: %w", err)
	}
	sort.SliceStable(evidence, func(left, right int) bool {
		return evidence[left].CompletedAt.After(evidence[right].CompletedAt)
	})
	for _, candidate := range evidence {
		if candidate.Verdict == "success" && !candidate.CompletedAt.IsZero() &&
			validPrior(candidate.Prior) {
			return candidate.Prior, nil
		}
	}
	return Prior{}, ErrPriorRequired
}

func (runtime *Runtime) resolvePolicy(instances []Instance) (Policy, error) {
	if len(instances) == 0 {
		return Policy{}, errors.New("rollout cohort is empty")
	}
	policy, err := runtime.dependencies.Policies.Resolve(instances[0])
	if err != nil {
		return Policy{}, err
	}
	for _, instance := range instances[1:] {
		candidate, resolveErr := runtime.dependencies.Policies.Resolve(instance)
		if resolveErr != nil {
			return Policy{}, resolveErr
		}
		if !reflect.DeepEqual(candidate, policy) {
			return Policy{}, errors.New("rollout cohort resolves to mixed policies")
		}
	}
	return policy, nil
}

func (runtime *Runtime) newRecord(prior Prior, policy Policy) RunRecord {
	now := runtime.dependencies.Now()
	return RunRecord{
		EvidenceID:     fmt.Sprintf("rollout-%d", now.UnixNano()),
		SourceInstance: prior.ValidatedInstanceID, PriorEvidenceID: prior.EvidenceID,
		Policy: policy, State: "canary", CreatedAt: now, UpdatedAt: now,
	}
}

func (runtime *Runtime) finalizeRecord(
	record *RunRecord, result Result, rolloutErr error,
) {
	record.AppliedInstances = result.AppliedInstances
	record.CanaryInstanceIDs = append([]string(nil), result.CanaryInstanceIDs...)
	record.AggregateRegressionPct = result.AggregateRegressionPct
	record.UpdatedAt = runtime.dependencies.Now()
	switch {
	case rolloutErr != nil:
		record.State = "failed"
		if errors.Is(rolloutErr, context.Canceled) {
			record.HaltReason = "context_canceled"
		} else {
			record.HaltReason = "rollout_error"
		}
	case result.Halted:
		record.State, record.HaltReason = "halted", result.HaltReason
	default:
		record.State = "complete"
	}
}

func validPrior(prior Prior) bool {
	return prior.EvidenceID != "" && prior.ValidatedInstanceID != "" && len(prior.Intent) > 0
}

func countVerifiedSince(evidence []PriorEvidence, since time.Time) int {
	count := 0
	for _, candidate := range evidence {
		if candidate.Verdict == "success" && candidate.CompletedAt.After(since) &&
			validPrior(candidate.Prior) {
			count++
		}
	}
	return count
}
