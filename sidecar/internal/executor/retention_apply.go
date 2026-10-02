package executor

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ErrRetentionUnverified reports a retention batch whose outcome does not
// match what it was authorized for: more rows than the reviewed bound,
// rows outside the declared predicate or relation, or a durable record
// that disagrees with the batch.
var ErrRetentionUnverified = errors.New("retention batch failed verification")

// RetentionRequest describes one bounded retention delete batch.
type RetentionRequest struct {
	Target string
	Column string
	// DeclaredColumn is the owner-declared retention column of the table
	// contract (D5); empty when the contract declares none.
	DeclaredColumn string
	Cutoff         time.Time
	Window         time.Duration
	BatchLimit     int
	Candidates     int64
	// Bound is how many more rows the reviewed dry run authorizes; the
	// batch never deletes more than min(BatchLimit, Bound).
	Bound int64
	// DryRunID is the reviewed dry run (sage.retention_run) behind Bound.
	DryRunID  int64
	IsReplica bool
	// Delete runs the D5 batch: identity re-check, bounded delete,
	// in-transaction verification and its durable record, in one
	// transaction. Nil refuses the request.
	Delete RetentionDelete
}

// RetentionDelete runs one batch for the pipeline.
type RetentionDelete func(context.Context, RetentionExecution) (RetentionOutcome, error)

// RetentionExecution is what the pipeline gives the batch: the action it
// is recorded under, the lock timeout of its authorized decision and the
// most rows it may delete.
type RetentionExecution struct {
	ActionID      int64
	LockTimeoutMS int
	MaxRows       int64
}

// RetentionOutcome is the batch's verified result: its sage.retention_run
// row, the rows deleted and how many of them fell outside the declared
// predicate or the contracted relation (both must be zero).
type RetentionOutcome struct {
	RunID            int64
	Deleted          int64
	OutsidePredicate int64
	OutsideRelation  int64
}

// ExecuteRetention runs one retention delete batch through Apply, the
// single execution pipeline: the standing gate authorizes it (emergency
// stop, executor enabled, trust, mode, replica, change class, ramp,
// windows, refusal set), it takes the table's typed-target lease and a DDL
// slot, is re-authorized after those waits, runs as a recorded action and
// is verified against its bound and its durable record. It returns the
// action_log id; a policy refusal is ErrCustodianProposalWithheld.
func (e *Executor) ExecuteRetention(ctx context.Context, request RetentionRequest) (int64, error) {
	run := &retentionRun{executor: e, request: request}
	actionID, err := e.Apply(ctx, ActionIntent{
		Request: retentionPolicyRequest(request),
		TargetLease: &TargetLease{Kind: "retention", Actor: "retention",
			Targets: []string{request.Target}, Intent: run.statement()},
		WaitForSlot: true, Admit: run.admit, Execute: run.execute, Verify: run.verify,
	})
	return actionID, custodianWithheld(err, "after lease")
}

func retentionPolicyRequest(request RetentionRequest) policy.ActionRequest {
	return policy.ActionRequest{
		Contract:        policyContract(retentionDeleteContract()),
		InternalControl: true, Feature: string(policy.ChangeRetention),
		// The owner's retention contract is the authority for this
		// unrollbackable delete only when it explicitly declares the column
		// being deleted by (D5, sage.table_contract.retention_column).
		OwnerDeclared: ownerDeclaredRetention(request),
		TargetObjs:    []string{request.Target}, IsReplica: request.IsReplica,
		Evidence: map[string]any{
			"retention_column": request.Column, "cutoff": request.Cutoff.UTC(),
			"window_seconds": request.Window.Seconds(), "batch_limit": request.BatchLimit,
			"candidate_rows": request.Candidates, "reviewed_bound": request.Bound,
			"dry_run_id": request.DryRunID,
		},
	}
}

// retentionDeleteContract types the bounded retention delete. It deletes
// user rows, so it is moderate: never autonomous before the moderate trust
// ramp, tier3_moderate and an open maintenance window.
func retentionDeleteContract() ActionContract {
	return ActionContract{
		ActionType:          "retention_delete",
		BaseRiskTier:        "moderate",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"DELETE on the contracted table"},
		Prechecks: []string{
			"explicit append-only retention contract with an owner-declared column",
			"reviewed dry run for the same relation, column identity, contract " +
				"version and window",
		},
		Guardrails: []string{"statement_timeout", "lock_timeout", "bounded batch",
			"typed-target lease"},
		ExecutionPlan: []string{"DELETE one bounded batch by (tableoid, ctid)"},
		SuccessCriteria: []string{"only rows older than the cutoff are deleted",
			"no more rows than the reviewed dry run's bound"},
		PostChecks: []string{"verify deleted rows against the declared predicate " +
			"before commit", "match the sage.retention_run record to the action"},
		RollbackClass: "not_reversible",
		Cooldown:      "one batch per autonomy tick",
		AuditFields:   []string{"table", "retention_column", "cutoff", "deleted_rows"},
	}
}

// ownerDeclaredRetention reports a delete that runs on the retention column
// the owner declared in the table contract, with a positive window. A
// pre-D5 contract declares no column and has no owner authority, so the
// refusal set's "unrollbackable" token sends such a delete to a human.
func ownerDeclaredRetention(request RetentionRequest) bool {
	return request.DeclaredColumn != "" && request.DeclaredColumn == request.Column &&
		request.Window > 0
}
