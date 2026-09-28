package runtime

import (
	"context"
	"errors"
	"fmt"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/policy"
)

type Verdict string

const (
	VerdictExpanded      Verdict = "expanded"
	VerdictRecommendOnly Verdict = "recommend_only"
	VerdictParked        Verdict = "parked"
	VerdictFailed        Verdict = "failed"
)

// ReasonContractPending marks an expanded migration whose contract
// steps (SET NOT NULL, attach UNIQUE USING INDEX, ...) have NOT run.
// Nothing in the runtime continues them yet; the steps are returned in
// Result.PendingContractSQL so the caller can finish them (G7-B23).
const ReasonContractPending = "contract_pending"

// reasonCleanupFailed is appended when the rehearsal clone leaked.
const reasonCleanupFailed = "clone_cleanup_failed"

type Request struct {
	DatabaseID *int64
	SQL        string
	Cycle      int
	Table      planpkg.TableFacts
	Proofs     []planpkg.Proof
}

type Result struct {
	Verdict                Verdict
	Reason                 string
	EvidenceID             string
	ContractNotBeforeCycle int
	PendingContractSQL     []string
}

type Record struct {
	Request                Request
	Verdict                Verdict
	Reason                 string
	EvidenceID             string
	ContractNotBeforeCycle int
	Measurement            *rehearsalpkg.Measurement
}

type Planner interface {
	Plan(context.Context, planpkg.Request) (planpkg.Plan, error)
}

type Rehearser interface {
	Rehearse(context.Context, planpkg.Plan) (rehearsalpkg.Result, error)
}

type Gate interface {
	Authorize(context.Context, policy.ActionRequest) policy.Decision
}

type Applier interface {
	Apply(context.Context, planpkg.Step) error
}

type Recorder interface {
	Record(context.Context, Record) error
}

type Orchestrator struct {
	planner   Planner
	rehearser Rehearser
	gate      Gate
	applier   Applier
	recorder  Recorder
}

func NewOrchestrator(
	planner Planner, rehearser Rehearser, gate Gate, applier Applier, recorder Recorder,
) *Orchestrator {
	return &Orchestrator{planner, rehearser, gate, applier, recorder}
}

func (o *Orchestrator) Apply(ctx context.Context, request Request) (Result, error) {
	if err := o.validate(); err != nil {
		return Result{}, err
	}
	plan, err := o.planner.Plan(ctx, planpkg.Request{
		SQL: request.SQL, Cycle: request.Cycle, Table: request.Table, Proofs: request.Proofs,
	})
	if err != nil {
		return o.fail(ctx, request, err)
	}
	rehearsal, err := o.rehearser.Rehearse(ctx, plan)
	if err != nil {
		return o.fail(ctx, request, err)
	}
	if rehearsal.Verdict != rehearsalpkg.VerdictPromoteExpand ||
		rehearsal.CleanupError != "" {
		return o.recordRehearsalResult(ctx, request, plan, rehearsal)
	}
	return o.applyExpand(ctx, request, plan, &rehearsal.Measurement)
}

func (o *Orchestrator) validate() error {
	if o == nil || o.planner == nil || o.rehearser == nil || o.gate == nil ||
		o.applier == nil || o.recorder == nil {
		return errors.New("migration runtime dependencies are incomplete")
	}
	return nil
}

func (o *Orchestrator) applyExpand(
	ctx context.Context, request Request, plan planpkg.Plan,
	measurement *rehearsalpkg.Measurement,
) (Result, error) {
	result := Result{Verdict: VerdictExpanded,
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}
	for _, step := range plan.ExpandSteps {
		decision := o.gate.Authorize(ctx, migrationAction(request, step))
		result.EvidenceID = decision.EvidenceID
		if decision.Verdict != policy.VerdictExecute {
			result.Verdict, result.Reason = VerdictParked, string(decision.Reason)
			return o.recordResult(ctx, request, result, measurement)
		}
		if err := o.applier.Apply(ctx, step); err != nil {
			return o.fail(ctx, request, err)
		}
	}
	for _, step := range plan.ContractSteps {
		result.PendingContractSQL = append(result.PendingContractSQL, step.SQL)
	}
	if len(result.PendingContractSQL) > 0 {
		result.Reason = ReasonContractPending
	}
	return o.recordResult(ctx, request, result, measurement)
}

func migrationAction(request Request, step planpkg.Step) policy.ActionRequest {
	return policy.ActionRequest{
		DatabaseID: request.DatabaseID,
		Feature:    string(policy.ChangeOnlineMigration), SQL: step.SQL,
		TargetObjs: []string{request.Table.Schema + "." + request.Table.Name},
		Contract: &policy.ActionContract{
			ActionType: "online_migration", RiskTier: policy.RiskModerate,
		},
	}
}

func (o *Orchestrator) recordRehearsalResult(
	ctx context.Context, request Request, plan planpkg.Plan, rehearsal rehearsalpkg.Result,
) (Result, error) {
	verdict := VerdictRecommendOnly
	if rehearsal.Verdict == rehearsalpkg.VerdictPark {
		verdict = VerdictParked
	}
	reason := string(rehearsal.Reason)
	if rehearsal.CleanupError != "" {
		// A leaked clone is recorded durably and blocks promotion.
		reason = joinReason(reason, reasonCleanupFailed+": "+rehearsal.CleanupError)
	}
	result := Result{Verdict: verdict, Reason: reason,
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}
	return o.recordResult(ctx, request, result, &rehearsal.Measurement)
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func (o *Orchestrator) fail(
	ctx context.Context, request Request, cause error,
) (Result, error) {
	result := Result{Verdict: VerdictFailed, Reason: cause.Error()}
	_, recordErr := o.recordResult(ctx, request, result, nil)
	return result, errors.Join(cause, recordErr)
}

func (o *Orchestrator) recordResult(
	ctx context.Context, request Request, result Result,
	measurement *rehearsalpkg.Measurement,
) (Result, error) {
	err := o.recorder.Record(ctx, Record{
		Request: request, Verdict: result.Verdict, Reason: result.Reason,
		EvidenceID:             result.EvidenceID,
		ContractNotBeforeCycle: result.ContractNotBeforeCycle,
		Measurement:            measurement,
	})
	if err != nil {
		return result, fmt.Errorf("record migration runtime: %w", err)
	}
	return result, nil
}
