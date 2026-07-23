package executor

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// EnableStandingPolicy installs the durable standing-policy gate and then
// bootstraps the selected profile. The gate is installed first so a bootstrap
// failure leaves the executor fail-closed rather than falling back to legacy
// trust flags.
func (e *Executor) EnableStandingPolicy(
	ctx context.Context, profile string, databaseID *int,
) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("standing policy requires a database pool")
	}
	policyStore := policy.NewStore(e.pool)
	ledgerService := ledger.NewService(ledger.NewPostgresRepository(e.pool))
	scope := policy.Scope{DatabaseID: int64Pointer(databaseID)}
	e.WithPolicyGate(e.newStandingPolicyGate(policyStore, ledgerService, scope, databaseID))
	if _, err := policyStore.Bootstrap(ctx, scope, profile, "system-bootstrap"); err != nil {
		return fmt.Errorf("bootstrap standing policy: %w", err)
	}
	return nil
}

func (e *Executor) newStandingPolicyGate(
	store *policy.Store, decisions *ledger.Service, scope policy.Scope, databaseID *int,
) policy.Gate {
	return policy.NewGate(policy.GateConfig{
		Runtime: func(ctx context.Context, request policy.ActionRequest) (policy.RuntimeState, error) {
			cfg, mode, enabled := e.policySnapshot()
			trust := ""
			if cfg != nil {
				trust = cfg.Trust.Level
			}
			return policy.RuntimeState{
				ExecutorEnabled: enabled, EmergencyStop: e.checkEmergencyStop(ctx),
				IsReplica: request.IsReplica, TrustLevel: trust, ExecutionMode: mode,
			}, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(ctx context.Context, _ policy.ActionRequest) (policy.Document, error) {
			current, err := store.Current(ctx, scope)
			if err != nil {
				return policy.Document{}, err
			}
			doc, err := policy.ParseDocument(current.Document)
			if err == nil {
				doc.Profile = policy.Profile(current.Profile)
			}
			return doc, err
		},
		RecordDecisionDetailed: func(
			ctx context.Context, request policy.ActionRequest, decision policy.Decision,
		) (string, int64, error) {
			current, err := store.Current(ctx, scope)
			if err != nil {
				return "", 0, err
			}
			input := ledgerInput(databaseID, current.Version, request, decision)
			recorded, err := decisions.RecordDecision(ctx, input)
			return recorded.EvidenceID, recorded.ID, err
		},
	})
}

func ledgerInput(
	databaseID *int, policyVersion int64, request policy.ActionRequest,
	decision policy.Decision,
) ledger.DecisionInput {
	evidenceID := ledger.NewEvidenceID()
	evidence := cloneCustodianEvidence(request.Evidence)
	evidence["off_window_ok"] = decision.OffWindowOK
	input := ledger.DecisionInput{
		DatabaseID: databaseID, Feature: request.Feature, Intent: request.Feature,
		Evidence:    evidence,
		ProposedSQL: request.SQL, Verdict: ledgerVerdict(decision.Verdict),
		Reason: string(decision.Reason), RiskTier: string(decision.RiskTier),
		PolicyVersion: int(policyVersion), TargetObjects: request.TargetObjs,
		EvidenceID: evidenceID,
	}
	if request.Deadline != nil {
		input.DeadlineKind = string(request.Deadline.Kind)
		input.DeadlineHardAt = &request.Deadline.HardAt
	}
	return input
}

func ledgerVerdict(verdict policy.Verdict) ledger.Verdict {
	if verdict == policy.VerdictPark {
		return ledger.VerdictPark
	}
	return ledger.Verdict(verdict)
}

func int64Pointer(value *int) *int64 {
	if value == nil {
		return nil
	}
	result := int64(*value)
	return &result
}
