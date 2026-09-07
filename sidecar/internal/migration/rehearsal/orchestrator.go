package rehearsal

import (
	"context"
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

func (o *Orchestrator) Rehearse(ctx context.Context, plan planpkg.Plan) (Result, error) {
	age, err := o.provider.SnapshotAge(ctx)
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly}, fmt.Errorf("snapshot age: %w", err)
	}
	if age > o.options.MaxCloneAge {
		return Result{Verdict: VerdictRecommendOnly, Reason: ReasonStaleClone}, nil
	}
	target, err := o.provider.Create(ctx, clonepkg.CloneSpec{IncludeData: true})
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly}, fmt.Errorf("create rehearsal clone: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = o.provider.Destroy(cleanup, target)
	}()
	expandOnly := plan
	expandOnly.ContractSteps = nil
	measurement, err := o.runner.Run(ctx, target.DSN, expandOnly)
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly}, fmt.Errorf("rehearse migration: %w", err)
	}
	result := Result{Verdict: VerdictPromoteExpand, Reason: ReasonRehearsalPassed,
		Measurement: measurement}
	if regressed(measurement, o.options.RegressionPct) {
		result.Verdict, result.Reason = VerdictPark, ReasonPlanRegression
	}
	return result, nil
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
