package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// RetentionRequest describes one bounded retention delete batch.
type RetentionRequest struct {
	Target     string
	Column     string
	Cutoff     time.Time
	Window     time.Duration
	BatchLimit int
	Candidates int64
	IsReplica  bool
}

// AuthorizeRetention routes a retention delete through the standing policy
// gate (emergency stop, executor enabled, trust, execution mode, replica,
// change class, ramp and windows). It returns nil only for an execute
// verdict; every other verdict withholds the delete.
func (e *Executor) AuthorizeRetention(ctx context.Context, request RetentionRequest) error {
	gate := e.StandingPolicyGate()
	if gate == nil {
		return fmt.Errorf("%w: standing policy gate is unavailable", ErrCustodianProposalWithheld)
	}
	decision := standingPolicyDecision(gate.Authorize(ctx, policy.ActionRequest{
		Contract:        policyContract(retentionDeleteContract()),
		InternalControl: true, Feature: string(policy.ChangeRetention),
		TargetObjs: []string{request.Target}, IsReplica: request.IsReplica,
		Evidence: map[string]any{
			"retention_column": request.Column, "cutoff": request.Cutoff.UTC(),
			"window_seconds": request.Window.Seconds(), "batch_limit": request.BatchLimit,
			"candidate_rows": request.Candidates,
		},
	}))
	if decision.Decision != PolicyDecisionExecute {
		return fmt.Errorf("%w: %s", ErrCustodianProposalWithheld, decision.BlockedReason)
	}
	return nil
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
			"explicit append-only retention contract",
			"reviewed dry run for the same column and window",
		},
		Guardrails:      []string{"statement_timeout", "lock_timeout", "bounded batch"},
		ExecutionPlan:   []string{"DELETE one bounded batch by (tableoid, ctid)"},
		SuccessCriteria: []string{"only rows older than the cutoff are deleted"},
		PostChecks:      []string{"record deleted row count in sage.retention_run"},
		RollbackClass:   "not_reversible",
		Cooldown:        "one batch per autonomy tick",
		AuditFields:     []string{"table", "retention_column", "cutoff", "deleted_rows"},
	}
}
