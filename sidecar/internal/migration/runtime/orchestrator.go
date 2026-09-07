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
}

type Record struct {
	Request                Request
	Verdict                Verdict
	Reason                 string
	EvidenceID             string
	ContractNotBeforeCycle int
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
	if rehearsal.Verdict != rehearsalpkg.VerdictPromoteExpand {
		return o.recordRehearsalResult(ctx, request, plan, rehearsal)
	}
	return o.applyExpand(ctx, request, plan)
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
) (Result, error) {
	result := Result{Verdict: VerdictExpanded,
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}
	for _, step := range plan.ExpandSteps {
		decision := o.gate.Authorize(ctx, migrationAction(request, step))
		result.EvidenceID = decision.EvidenceID
		if decision.Verdict != policy.VerdictExecute {
			result.Verdict, result.Reason = VerdictParked, string(decision.Reason)
			return o.recordResult(ctx, request, result)
		}
		if err := o.applier.Apply(ctx, step); err != nil {
			return o.fail(ctx, request, err)
		}
	}
	return o.recordResult(ctx, request, result)
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
	result := Result{Verdict: verdict, Reason: string(rehearsal.Reason),
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}
	return o.recordResult(ctx, request, result)
}

func (o *Orchestrator) fail(
	ctx context.Context, request Request, cause error,
) (Result, error) {
	result := Result{Verdict: VerdictFailed, Reason: cause.Error()}
	_, recordErr := o.recordResult(ctx, request, result)
	return result, errors.Join(cause, recordErr)
}

func (o *Orchestrator) recordResult(
	ctx context.Context, request Request, result Result,
) (Result, error) {
	err := o.recorder.Record(ctx, Record{
		Request: request, Verdict: result.Verdict, Reason: result.Reason,
		EvidenceID:             result.EvidenceID,
		ContractNotBeforeCycle: result.ContractNotBeforeCycle,
	})
	if err != nil {
		return result, fmt.Errorf("record migration runtime: %w", err)
	}
	return result, nil
}
