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
	// ReasonNoReceiptStore: no durable receipt could be written for the
	// clone, so none was created (migration-07).
	ReasonNoReceiptStore Reason = "no_receipt_store"
	// ReasonTargetIsProduction: the clone the provider returned is the
	// production server; nothing ran on it (migration-07).
	ReasonTargetIsProduction Reason = "target_is_production"
	// ReasonTargetUnverified: the clone could not be shown not to be the
	// production server; nothing ran on it.
	ReasonTargetUnverified Reason = "target_unverified"
	// ReasonStepBudget: a rehearsed step ran too long for production: 1.5x
	// its time exceeds the applier's statement_timeout, or a step without
	// CONCURRENTLY held ACCESS EXCLUSIVE past migration.max_lock_ms
	// (migration-06). Result.Detail carries the measured value.
	ReasonStepBudget Reason = "step_budget_exceeded"
)

// DefaultMaxDuration bounds one rehearsal, clone creation included, when
// Options.MaxDuration is not set. A clone receipt must outlive it.
const DefaultMaxDuration = time.Hour

// ErrCloneUnavailable wraps failures of the clone provider (snapshot
// age, create). ErrStepFailed wraps failures of the rehearsed steps on
// a clone that did exist (G7-B25).
var (
	ErrCloneUnavailable = errors.New("rehearsal clone unavailable")
	ErrStepFailed       = errors.New("rehearsal step failed")
	// ErrTargetIsProduction: a Runner refuses a target that is the
	// production server (the same server, under any DSN). It is the clone
	// identity guard's error, shared with the tuning rehearser (lab-04a).
	ErrTargetIsProduction = clonepkg.ErrTargetIsProduction
	// ErrTargetUnverified: a Runner could not show the target is not the
	// production server, so it ran nothing (fail closed).
	ErrTargetUnverified = clonepkg.ErrTargetUnverified
)

// QueryMeasurement is one affected statement measured on the clone before
// and after the expand steps (lab-04b). An executed statement carries its
// latency and shared blocks; one planned only carries its plan cost and
// NotExecuted. Unmeasured says why a statement gives no evidence (a
// timeout, more than one statement, an error): it is never judged.
type QueryMeasurement struct {
	QueryID         int64   `json:"query_id,string"`
	BeforeLatencyMS float64 `json:"before_latency_ms,omitempty"`
	AfterLatencyMS  float64 `json:"after_latency_ms,omitempty"`
	BeforePlanHash  string  `json:"before_plan_hash,omitempty"`
	AfterPlanHash   string  `json:"after_plan_hash,omitempty"`
	BeforeCost      float64 `json:"before_cost,omitempty"`
	AfterCost       float64 `json:"after_cost,omitempty"`
	BeforeBlocks    *int64  `json:"before_blocks,omitempty"`
	AfterBlocks     *int64  `json:"after_blocks,omitempty"`
	NotExecuted     string  `json:"not_executed,omitempty"`
	Unmeasured      string  `json:"unmeasured,omitempty"`
	TimedOut        bool    `json:"timed_out,omitempty"`
}
type Measurement struct {
	// MaxStepDuration is the longest step's wall time (migration-06
	// renamed it from MaxLockDuration: it is not a lock hold time).
	MaxStepDuration  time.Duration
	BackfillDuration time.Duration
	DiskDeltaBytes   int64
	AffectedQueries  []QueryMeasurement
	// Steps holds every rehearsed step's wall time and lock, in order.
	Steps []StepMeasurement
	// WorkloadNote says why AffectedQueries is empty: no statement
	// references the table, or pg_stat_statements is unavailable on
	// production.
	WorkloadNote string
}
type Result struct {
	Verdict Verdict
	Reason  Reason
	// Detail is the measured value behind Reason (step budget parks).
	Detail      string
	Measurement Measurement
	// CleanupError is non-empty when the rehearsal clone could not be
	// destroyed; callers must record it durably (leaked resource).
	CleanupError string
	// CloneID and ReceiptID name the clone the rehearsal ran on and its
	// durable receipt (0 without a receipting provider); Provider is
	// Options.Provider. At is when the rehearsal ended.
	CloneID   string
	ReceiptID int64
	Provider  string
	At        time.Time
}
type Options struct {
	MaxCloneAge   time.Duration
	RegressionPct float64
	// PlanFlipPct is the latency change above which a query whose plan
	// hash changed parks; zero means DefaultPlanFlipPct.
	PlanFlipPct float64
	// MaxDuration bounds one rehearsal; zero means DefaultMaxDuration.
	MaxDuration time.Duration
	// StatementTimeout is the production applier's statement_timeout;
	// zero means DefaultStepStatementTimeout.
	StatementTimeout time.Duration
	// MaxLock bounds how long a step without CONCURRENTLY may hold
	// ACCESS EXCLUSIVE (migration.max_lock_ms); zero means DefaultMaxLock.
	MaxLock time.Duration
	// Provider names the clone provider on the result and its ladder
	// record (dle, lakebase_branch).
	Provider string
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
	defer func() { result.Provider, result.At = o.options.Provider, time.Now() }()
	ctx, cancel := context.WithTimeout(ctx, o.maxDuration())
	defer cancel()
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
	// The receipt is named while the clone lives: a destroyed clone has
	// none to name.
	receipt := o.receiptOf(target)
	defer func() {
		result.CloneID, result.ReceiptID = target.ID, receipt
		result.CleanupError = o.destroy(target)
	}()
	expandOnly := plan
	expandOnly.ContractSteps = nil
	measurement, err := o.runner.Run(ctx, target.DSN, expandOnly)
	if err != nil {
		return Result{Verdict: VerdictRecommendOnly},
			fmt.Errorf("%w: rehearse migration: %w", ErrStepFailed, err)
	}
	return o.judge(measurement), nil
}

func (o *Orchestrator) maxDuration() time.Duration {
	if o.options.MaxDuration > 0 {
		return o.options.MaxDuration
	}
	return DefaultMaxDuration
}

// receiptOf is the clone's durable receipt id, or 0 when the provider
// keeps none.
func (o *Orchestrator) receiptOf(target clonepkg.Clone) int64 {
	if namer, ok := o.provider.(clonepkg.ReceiptNamer); ok {
		if id, ok := namer.ReceiptOf(target.ID); ok {
			return id
		}
	}
	return 0
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
