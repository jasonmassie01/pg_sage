package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"time"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
)

type Verdict string

const (
	VerdictRecommendOnly Verdict = "recommend_only"
	VerdictPark          Verdict = "park"
	VerdictPromoteExpand Verdict = "promote_expand"
)

type Reason string

const (
	ReasonStaleClone      Reason = "stale_clone"
	ReasonPlanRegression  Reason = "plan_regression"
	ReasonRehearsalPassed Reason = "rehearsal_passed"
	// ReasonInconclusiveWorkload: the rehearsal produced no paired
	// before/after workload evidence, so a regression cannot be ruled
	// out and the plan is never promoted (G7-B22, R08).
	ReasonInconclusiveWorkload Reason = "inconclusive_no_workload"
)

// ErrCloneUnavailable wraps failures of the clone provider (snapshot
// age, create). ErrStepFailed wraps failures of the rehearsed steps on
// a clone that did exist (G7-B25).
var (
	ErrCloneUnavailable = errors.New("rehearsal clone unavailable")
	ErrStepFailed       = errors.New("rehearsal step failed")
)

type QueryMeasurement struct {
	QueryID         int64
	BeforeLatencyMS float64
	AfterLatencyMS  float64
	BeforePlanHash  string
	AfterPlanHash   string
}
type Measurement struct {
	MaxLockDuration  time.Duration
	BackfillDuration time.Duration
	DiskDeltaBytes   int64
	AffectedQueries  []QueryMeasurement
}
type Result struct {
	Verdict     Verdict
	Reason      Reason
	Measurement Measurement
	// CleanupError is non-empty when the rehearsal clone could not be
	// destroyed; callers must record it durably (leaked resource).
	CleanupError string
}
type Options struct {
	MaxCloneAge   time.Duration
	RegressionPct float64
}
type Runner interface {
	Run(context.Context, string, planpkg.Plan) (Measurement, error)
}
type Orchestrator struct {
	provider clonepkg.Provider
	runner   Runner
	options  Options
}

func NewOrchestrator(p clonepkg.Provider, r Runner, o Options) *Orchestrator {
	return &Orchestrator{p, r, o}
}

func (o *Orchestrator) Rehearse(
	ctx context.Context, plan planpkg.Plan,
) (result Result, err error) {
	age, err := o.provider.SnapshotAge(ctx)
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly},
			fmt.Errorf("%w: snapshot age: %w", ErrCloneUnavailable, err)
	}
	if age > o.options.MaxCloneAge {
		return Result{Verdict: VerdictRecommendOnly, Reason: ReasonStaleClone}, nil
	}
	target, err := o.provider.Create(ctx, clonepkg.CloneSpec{IncludeData: true})
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly},
			fmt.Errorf("%w: create rehearsal clone: %w", ErrCloneUnavailable, err)
	}
	defer func() { result.CleanupError = o.destroy(target) }()
	expandOnly := plan
	expandOnly.ContractSteps = nil
	measurement, err := o.runner.Run(ctx, target.DSN, expandOnly)
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly},
			fmt.Errorf("%w: rehearse migration: %w", ErrStepFailed, err)
	}
	return o.judge(measurement), nil
}

// destroy tears the clone down on a fresh context (the caller's may be
// cancelled) and reports a failure instead of discarding it (R08).
func (o *Orchestrator) destroy(target clonepkg.Clone) string {
	cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := o.provider.Destroy(cleanup, target); err != nil {
		return fmt.Sprintf("destroy rehearsal clone %s: %v", target.ID, err)
	}
	return ""
}

// judge turns a measurement into a verdict. Without affected-query
// evidence the result is inconclusive and never promotes.
func (o *Orchestrator) judge(measurement Measurement) Result {
	result := Result{Verdict: VerdictPromoteExpand, Reason: ReasonRehearsalPassed,
		Measurement: measurement}
	if len(measurement.AffectedQueries) == 0 {
		result.Verdict, result.Reason = VerdictRecommendOnly, ReasonInconclusiveWorkload
		return result
	}
	if regressed(measurement, o.options.RegressionPct) {
		result.Verdict, result.Reason = VerdictPark, ReasonPlanRegression
	}
	return result
}

func regressed(measurement Measurement, threshold float64) bool {
	for _, query := range measurement.AffectedQueries {
		if query.BeforeLatencyMS <= 0 {
			continue
		}
		change := (query.AfterLatencyMS - query.BeforeLatencyMS) * 100 / query.BeforeLatencyMS
		if change > threshold || (query.AfterPlanHash != query.BeforePlanHash && change > threshold) {
			return true
		}
	}
	return false
}
