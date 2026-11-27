package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
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
// pg_sage never runs them: the steps are returned in
// Result.PendingContractSQL and as Result.ContractScript, a forward-fix
// script that asserts the proof first (G7-B23, migration-06).
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
	// ContractScript is the rendered forward-fix script for
	// PendingContractSQL; it is never executed.
	ContractScript string
	// AffectedQueries is what the rehearsal measured of the statements
	// that reference the table, and Ladder its tier-2 record with the
	// clone receipt (lab-04b); both are empty when nothing was rehearsed.
	AffectedQueries []rehearsalpkg.QueryMeasurement
	Ladder          *verify.LadderRecord
}

type Record struct {
	Request     Request
	Verdict     Verdict
	Reason      string
	EvidenceID  string
	Measurement *rehearsalpkg.Measurement
	// ContractDelay is how long after now the contract may run; zero
	// records no contract_not_before (nothing expanded).
	ContractDelay time.Duration
	// Ladder is the rehearsal's tier-2 record; CloneReceipt its clone
	// receipt id (0 for none).
	Ladder       *verify.LadderRecord
	CloneReceipt int64
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
	return o.applyExpand(ctx, request, plan, rehearsal)
}

// withRehearsal carries the rehearsal's measured statements and ladder
// record onto the result.
func withRehearsal(result Result, rehearsal rehearsalpkg.Result) Result {
	ladder := rehearsalpkg.Ladder(rehearsal)
	result.AffectedQueries = rehearsal.Measurement.AffectedQueries
	result.Ladder = &ladder
	return result
}

func (o *Orchestrator) validate() error {
	if o == nil || o.planner == nil || o.rehearser == nil || o.gate == nil ||
		o.applier == nil || o.recorder == nil {
		return errors.New("migration runtime dependencies are incomplete")
	}
	return nil
}

func (o *Orchestrator) applyExpand(
	ctx context.Context, request Request, plan planpkg.Plan, rehearsal rehearsalpkg.Result,
) (Result, error) {
	result := withRehearsal(Result{Verdict: VerdictExpanded,
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}, rehearsal)
	for _, step := range plan.ExpandSteps {
		decision := o.gate.Authorize(ctx, migrationAction(request, step))
		result.EvidenceID = decision.EvidenceID
		if decision.Verdict != policy.VerdictExecute {
			result.Verdict, result.Reason = VerdictParked, string(decision.Reason)
			return o.recordResult(ctx, request, result, &rehearsal, 0)
		}
		if err := o.applier.Apply(ctx, step); err != nil {
			return o.fail(ctx, request, err)
		}
	}
	if len(plan.ContractSteps) == 0 {
		return o.recordResult(ctx, request, result, &rehearsal, 0)
	}
	for _, step := range plan.ContractSteps {
		result.PendingContractSQL = append(result.PendingContractSQL, step.SQL)
	}
	result.Reason = ReasonContractPending
	script, err := plan.ContractScript()
	if err != nil {
		result.Reason = joinReason(result.Reason, "contract_script_unavailable: "+err.Error())
	}
	result.ContractScript = script
	return o.recordResult(ctx, request, result, &rehearsal, plan.ContractNotBefore)
}

func migrationAction(request Request, step planpkg.Step) policy.ActionRequest {
	return policy.ActionRequest{
		DatabaseID: request.DatabaseID,
		Feature:    string(policy.ChangeOnlineMigration), SQL: step.SQL,
		TargetObjs: []string{request.Table.Schema + "." + request.Table.Name},
		Contract: &policy.ActionContract{
			ActionType: "online_migration", RiskTier: policy.RiskModerate,
			RollbackClass: policy.RollbackForwardFixOnly,
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
	if rehearsal.Detail != "" {
		reason += ": " + rehearsal.Detail
	}
	if rehearsal.CleanupError != "" {
		// A leaked clone is recorded durably and blocks promotion.
		reason = joinReason(reason, reasonCleanupFailed+": "+rehearsal.CleanupError)
	}
	result := withRehearsal(Result{Verdict: verdict, Reason: reason,
		ContractNotBeforeCycle: plan.ContractNotBeforeCycle}, rehearsal)
	return o.recordResult(ctx, request, result, &rehearsal, 0)
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
	_, recordErr := o.recordResult(ctx, request, result, nil, 0)
	return result, errors.Join(cause, recordErr)
}

// recordResult writes the run with the rehearsal behind it (nil when
// none ran); contractDelay is zero unless the expand steps ran and
// contract steps wait.
func (o *Orchestrator) recordResult(
	ctx context.Context, request Request, result Result,
	rehearsal *rehearsalpkg.Result, contractDelay time.Duration,
) (Result, error) {
	record := Record{
		Request: request, Verdict: result.Verdict, Reason: result.Reason,
		EvidenceID: result.EvidenceID, ContractDelay: contractDelay, Ladder: result.Ladder,
	}
	if rehearsal != nil {
		record.Measurement, record.CloneReceipt = &rehearsal.Measurement, rehearsal.ReceiptID
	}
	err := o.recorder.Record(ctx, record)
	if err != nil {
		return result, fmt.Errorf("record migration runtime: %w", err)
	}
	return result, nil
}
