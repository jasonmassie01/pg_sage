package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	return e.EnableStandingPolicyWithStore(ctx, nil, profile, databaseID)
}

// EnableStandingPolicyWithStore reads the standing policy from policyPool
// (the control/meta database the API writes to) while decisions stay in
// the executor's own database. A nil policyPool uses the executor pool
// (standalone). Fleet executors must pass the control pool (G5-B11).
func (e *Executor) EnableStandingPolicyWithStore(
	ctx context.Context, policyPool *pgxpool.Pool,
	profile string, databaseID *int,
) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("standing policy requires a database pool")
	}
	if policyPool == nil {
		policyPool = e.pool
	}
	policyStore := policy.NewStore(policyPool)
	ledgerService := ledger.NewService(ledger.NewPostgresRepository(e.pool))
	scope := policy.Scope{DatabaseID: int64Pointer(databaseID)}
	e.policyMu.Lock()
	e.databaseID = databaseID
	e.policyMu.Unlock()
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
			return e.standingRuntimeState(ctx, request), nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Usage:       e.standingUsage,
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
		DatabaseID: databaseID, Feature: request.Feature, Intent: ledgerIntent(request),
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

// ledgerIntent separates operator-approved decisions, which the gate's
// self-initiated usage limits exclude, from autonomous ones.
func ledgerIntent(request policy.ActionRequest) string {
	if request.OperatorApproved {
		return operatorDecisionIntent
	}
	return request.Feature
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

// EnableStandingPolicyDocument installs the standing gate over the
// executor's live runtime state with an in-memory policy document instead
// of the durable policy store. Decisions are recorded in the ledger when
// the executor has a pool. It serves embedders and tests without a policy
// table; production executors use EnableStandingPolicyWithStore.
func (e *Executor) EnableStandingPolicyDocument(doc policy.Document, now func() time.Time) {
	config := policy.GateConfig{
		Runtime: func(ctx context.Context, req policy.ActionRequest) (policy.RuntimeState, error) {
			state := e.standingRuntimeState(ctx, req)
			if now != nil {
				cfg, _, _ := e.policySnapshot()
				state.InConfiguredWindow = inMaintenanceWindowForPolicy(cfg, now())
			}
			return state, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now: now,
	}
	if e.pool != nil {
		decisions := ledger.NewService(ledger.NewPostgresRepository(e.pool))
		config.Usage = e.standingUsage
		config.RecordDecisionDetailed = func(
			ctx context.Context, req policy.ActionRequest, decision policy.Decision,
		) (string, int64, error) {
			recorded, err := decisions.RecordDecision(ctx,
				ledgerInput(e.databaseID, 1, req, decision))
			return recorded.EvidenceID, recorded.ID, err
		}
	}
	e.WithPolicyGate(policy.NewGate(config))
}

// WithEmergencyStopCheck replaces how the executor reads the emergency-stop
// flag (default: sage.config in the executor's database).
func (e *Executor) WithEmergencyStopCheck(check func(context.Context) bool) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.emergencyStopFn = check
}
