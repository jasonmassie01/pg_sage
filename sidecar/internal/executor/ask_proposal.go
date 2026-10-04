package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Ask Sage (roadmap phase 3) may queue one of pg_sage's own findings for
// a person's approval and nothing more. The standing gate is asked with
// Explain (no decision recorded, no approval flags): what it blocks is
// not queued; what it would run autonomously is still only queued. Only
// typed actions with a rollback (or none needed) qualify, and the
// finding's own SQL is queued, never text from the conversation. A block
// of pg_sage's own initiative that a person's approval lifts (the trust
// ramp, for example) does not stop the proposal: the person decides.

// Errors of ProposeFindingForApproval, distinguishable with errors.Is.
var (
	ErrNotProposable            = errors.New("finding cannot be proposed")
	ErrProposalBlocked          = errors.New("the policy gate blocks this proposal")
	ErrApprovalQueueUnavailable = errors.New("no approval queue to propose to")
)

// askProposalTTL is how long a proposal waits for a person.
const askProposalTTL = 24 * time.Hour

// FindingProposal is a finding queued (or already waiting) for approval.
type FindingProposal struct {
	QueueID       int
	FindingID     int64
	Created       bool
	ActionType    string
	SQL           string
	RollbackSQL   string
	RollbackClass string
	Decision      ActionPolicyDecision
	Prediction    verify.Prediction
}

type proposableFinding struct {
	title, category, object, sql, rollback string
	detail                                 map[string]any
}

// ProposeFindingForApproval queues finding findingID for a person's
// approval. It never executes anything.
func (e *Executor) ProposeFindingForApproval(ctx context.Context,
	findingID int64) (FindingProposal, error) {
	proposer, err := e.approvalQueue()
	if err != nil {
		return FindingProposal{}, err
	}
	f, err := e.proposableFinding(ctx, findingID)
	if err != nil {
		return FindingProposal{}, err
	}
	p, contract, err := typedProposal(findingID, f)
	if err != nil {
		return FindingProposal{}, err
	}
	p.Decision = e.ExplainAction(ctx, contract, f.sql, f.object)
	if !approvalCanRun(p.Decision) {
		return p, fmt.Errorf("%w: %s %s", ErrProposalBlocked, p.Decision.BlockedReason,
			p.Decision.Detail)
	}
	p.Prediction = e.predictEffect(ctx, f.sql, f.detail, map[string]any{})
	release, err := e.lockFindingProposal(ctx, findingID)
	if err != nil {
		return p, err
	}
	defer release()
	if id, ok, err := e.pendingProposal(ctx, findingID); err != nil || ok {
		p.QueueID = id
		return p, err
	}
	return e.queueProposal(ctx, proposer, p, f, contract)
}

// approvalLifts are the gate's blocks of pg_sage's own initiative that a
// person's approval lifts (policy.operatorDecision): tiers, the trust
// ramp, earned autonomy, budgets, rate limits, the refusal set, a lease
// conflict and windows (an approval still waits for its window).
var approvalLifts = map[string]bool{
	string(policy.ReasonTrustRampNotSatisfied):    true,
	string(policy.ReasonApprovalRequired):         true,
	string(policy.ReasonBudgetExceeded):           true,
	string(policy.ReasonRateLimitExceeded):        true,
	string(policy.ReasonRefusedByPolicy):          true,
	string(policy.ReasonOutsideMaintenanceWindow): true,
	string(policy.ReasonAutonomyLevel):            true,
	string(policy.ReasonAutonomyHandoff):          true,
	string(policy.ReasonAutonomyDowngraded):       true,
	string(policy.ReasonDDLConflict):              true,
}

// approvalCanRun reports whether a person's approval of the proposal
// could run it: any verdict but blocked and observe-only, or a block an
// approval lifts. Unknown reasons fail closed.
func approvalCanRun(d ActionPolicyDecision) bool {
	switch d.Decision {
	case PolicyDecisionBlocked:
		return approvalLifts[d.BlockedReason]
	case PolicyDecisionObserveOnly:
		return false
	}
	return true
}

func (e *Executor) approvalQueue() (ActionMetadataProposer, error) {
	if e == nil || e.pool == nil {
		return nil, fmt.Errorf("%w: no executor database", ErrApprovalQueueUnavailable)
	}
	proposer, ok := e.actionStore.(ActionMetadataProposer)
	if !ok {
		return nil, fmt.Errorf("%w: the executor has no action store",
			ErrApprovalQueueUnavailable)
	}
	return proposer, nil
}

// proposableFinding reads an open finding that carries SQL to run.
func (e *Executor) proposableFinding(ctx context.Context, id int64) (proposableFinding,
	error) {
	if id <= 0 {
		return proposableFinding{}, fmt.Errorf("%w: finding id %d", ErrNotProposable, id)
	}
	var f proposableFinding
	var status string
	var detail []byte
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT title, category,
		COALESCE(object_identifier, ''), COALESCE(recommended_sql, ''),
		COALESCE(rollback_sql, ''), detail, status FROM sage.findings WHERE id = $1`,
		id).Scan(&f.title, &f.category, &f.object, &f.sql, &f.rollback, &detail, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return f, fmt.Errorf("%w: finding %d not found", ErrNotProposable, id)
	case err != nil:
		return f, fmt.Errorf("read finding %d: %w", id, err)
	case status != "open":
		return f, fmt.Errorf("%w: finding %d is %s", ErrNotProposable, id, status)
	case strings.TrimSpace(f.sql) == "":
		return f, fmt.Errorf("%w: finding %d recommends no SQL", ErrNotProposable, id)
	}
	if err := json.Unmarshal(detail, &f.detail); err != nil {
		f.detail = map[string]any{}
	}
	return f, nil
}

// typedProposal requires a typed action contract and a rollback, unless
// the contract needs none.
func typedProposal(id int64, f proposableFinding) (FindingProposal, ActionContract, error) {
	actionType := actionTypeForProposalSQL(f.sql)
	contract, ok := ContractForActionType(actionType)
	if actionType == "" || !ok {
		return FindingProposal{}, ActionContract{}, fmt.Errorf(
			"%w: finding %d has no typed action contract for its SQL", ErrNotProposable, id)
	}
	if contract.RollbackClass == "reversible" && strings.TrimSpace(f.rollback) == "" {
		return FindingProposal{}, ActionContract{}, fmt.Errorf(
			"%w: finding %d is reversible but carries no rollback SQL", ErrNotProposable, id)
	}
	return FindingProposal{FindingID: id, ActionType: actionType, SQL: f.sql,
		RollbackSQL: f.rollback, RollbackClass: contract.RollbackClass}, contract, nil
}

// lockFindingProposal serializes proposals of one finding across
// sidecars with a session advisory lock on a dedicated connection.
func (e *Executor) lockFindingProposal(ctx context.Context, id int64) (func(), error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock proposal of finding %d: %w", id, err)
	}
	key := fmt.Sprintf("pg_sage:ask_proposal:%d", id)
	if _, err := conn.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_lock(
		hashtextextended($1, 0))`, key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock proposal of finding %d: %w", id, err)
	}
	return func() {
		_, unlockErr := conn.Exec(context.WithoutCancel(ctx), `/* pg_sage */ SELECT
			pg_advisory_unlock(hashtextextended($1, 0))`, key)
		if unlockErr != nil {
			e.logFn("executor", "unlock proposal of finding %d: %v", id, unlockErr)
		}
		conn.Release()
	}, nil
}

// pendingProposal is the finding's item already waiting for approval.
func (e *Executor) pendingProposal(ctx context.Context, id int64) (int, bool, error) {
	var queueID int
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.action_queue
		WHERE finding_id = $1 AND status = 'pending'
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY id DESC LIMIT 1`, id).Scan(&queueID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read pending proposal of finding %d: %w", id, err)
	}
	return queueID, true, nil
}

func (e *Executor) queueProposal(ctx context.Context, proposer ActionMetadataProposer,
	p FindingProposal, f proposableFinding, contract ActionContract) (FindingProposal,
	error) {
	expires := time.Now().UTC().Add(askProposalTTL)
	meta := store.ActionProposalMetadata{ActionType: p.ActionType,
		IdentityKey:    strings.Join([]string{f.category, f.object, p.ActionType}, ":"),
		PolicyDecision: p.Decision.Decision, Guardrails: p.Decision.Guardrails,
		VerificationStatus: "not_started", ExpiresAt: &expires,
		ShadowToilMinutes: estimatedToilForActionType(p.ActionType)}
	risk := p.Decision.RiskTier
	if risk == "" {
		risk = contract.BaseRiskTier
	}
	id, err := proposer.ProposeWithMetadata(ctx, e.databaseID, int(p.FindingID), p.SQL,
		p.RollbackSQL, risk, meta)
	if err != nil {
		return p, fmt.Errorf("queue proposal of finding %d: %w", p.FindingID, err)
	}
	p.QueueID, p.Created = id, true
	e.requestApproval(ctx, "Ask Sage proposal: "+f.title, p.SQL, risk, 0, id)
	return p, nil
}
