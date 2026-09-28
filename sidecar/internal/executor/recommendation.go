package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

func newRecommendationStore(pool *pgxpool.Pool) *recommendation.Store {
	if pool == nil {
		return nil
	}
	return recommendation.NewStore(pool)
}

// recommendationLease outlives any apply attempt: each Apply stage is
// bounded by applyTimeout, so an expired lease means its owner is gone.
func (e *Executor) recommendationLease() time.Duration {
	return 2 * e.applyTimeout()
}

// reconcileRecommendations resumes interrupted claims and records
// verification outcomes. It is bookkeeping, not action, so it runs in
// every execution mode.
func (e *Executor) reconcileRecommendations(ctx context.Context) {
	if e.recs == nil {
		return
	}
	recovered, err := e.recs.RecoverStaleApplying(ctx, e.databaseName)
	if err != nil {
		e.logFn("executor", "recover interrupted recommendations: %v", err)
	} else if recovered > 0 {
		e.logFn("executor", "recovered %d interrupted recommendation apply attempts",
			recovered)
	}
	if _, err := e.recs.ReconcileVerifying(ctx, e.databaseName); err != nil {
		e.logFn("executor", "reconcile recommendation verification: %v", err)
	}
}

// actionableRecommendations reads the durable candidates of this cycle.
func (e *Executor) actionableRecommendations(ctx context.Context) []recommendation.Candidate {
	if e.recs == nil {
		return nil
	}
	candidates, err := e.recs.ListActionable(ctx, e.databaseName)
	if err != nil {
		e.logFn("executor", "list actionable recommendations: %v", err)
		return nil
	}
	return candidates
}

// processCandidate re-checks a durable candidate's freshness, then routes
// an operator approval to the operator path and everything else through
// the standing gate.
func (e *Executor) processCandidate(
	ctx context.Context, c recommendation.Candidate, isReplica bool,
) {
	fresh, err := e.recs.CheckFresh(ctx, c)
	if err != nil {
		e.logFn("executor", "check recommendation %d: %v", c.ID, err)
		return
	}
	if !fresh.Fresh {
		if fresh.Supersede {
			e.supersede(ctx, c, fresh.Reason)
		}
		return
	}
	if operatorID, ok := operatorApprover(c.ApprovedBy); ok &&
		c.State != recommendation.StateProposed {
		e.runOperatorApproval(ctx, c, operatorID)
		return
	}
	cand := c
	e.processFinding(ctx, findingFromCandidate(e.databaseName, c), isReplica, &cand)
}

func (e *Executor) supersede(ctx context.Context, c recommendation.Candidate, reason string) {
	err := e.recs.Supersede(ctx, c.ID, c.State, c.Revision,
		recommendation.ActorExecutor, reason)
	if err != nil && !errors.Is(err, recommendation.ErrConflict) {
		e.logFn("executor", "supersede recommendation %d: %v", c.ID, err)
	}
}

// operatorApprover parses a "user:<id>" approval actor.
func operatorApprover(actor string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimPrefix(actor, "user:"))
	return id, err == nil && strings.HasPrefix(actor, "user:")
}

// runOperatorApproval executes a durable operator approval (one whose
// synchronous execution was interrupted, or a due retry) under the
// operator authorization, exactly as the approval surface would.
func (e *Executor) runOperatorApproval(
	ctx context.Context, c recommendation.Candidate, approvedBy int,
) {
	if c.FindingID == nil {
		return
	}
	_, err := e.ExecuteManual(ctx, int(*c.FindingID), c.Current.ForwardSQL,
		c.Current.InverseSQL, &approvedBy)
	if err != nil && !errors.Is(err, recommendation.ErrConflict) {
		e.logFn("executor", "operator-approved recommendation %d: %v", c.ID, err)
	}
}

func findingFromCandidate(database string, c recommendation.Candidate) analyzer.Finding {
	rev := c.Current
	return analyzer.Finding{
		Category: c.Category, Severity: rev.Severity, ObjectType: rev.ObjectType,
		ObjectIdentifier: c.Target, Title: rev.Title, Detail: rev.Evidence,
		Recommendation: rev.Recommendation, RecommendedSQL: rev.ForwardSQL,
		RollbackSQL: rev.InverseSQL, ActionRisk: rev.ActionRisk, DatabaseName: database,
	}
}

// claimCandidate takes the durable claim on an autonomous candidate right
// before its SQL runs; the policy verdict is the approval.
func (e *Executor) claimCandidate(
	ctx context.Context, c *recommendation.Candidate, decisionID int64,
) (*recommendation.Claim, error) {
	if c == nil || e.recs == nil {
		return nil, nil
	}
	actor := recommendation.ActorPolicy
	if decisionID > 0 {
		actor = fmt.Sprintf("%s:decision:%d", recommendation.ActorPolicy, decisionID)
	}
	claim, err := e.recs.Claim(ctx, recommendation.ClaimRequest{ID: c.ID,
		Revision: c.Revision, ApproveAs: actor, Lease: e.recommendationLease(),
		Reason: "authorized by the standing policy"})
	if err != nil {
		return nil, fmt.Errorf("claim recommendation %d: %w", c.ID, err)
	}
	return &claim, nil
}

// claimForOperator claims the live recommendation an operator action
// executes, when there is one. The operator's action is its approval.
func (e *Executor) claimForOperator(
	ctx context.Context, findingID int, sql string, approvedBy *int,
) (*recommendation.Claim, error) {
	if e.recs == nil || findingID <= 0 {
		return nil, nil
	}
	c, err := e.recs.FindForOperator(ctx, int64(findingID), sql)
	if err != nil || c == nil {
		return nil, err
	}
	actor := "operator"
	if approvedBy != nil {
		actor = "user:" + strconv.Itoa(*approvedBy)
	}
	claim, err := e.recs.Claim(ctx, recommendation.ClaimRequest{ID: c.ID,
		Revision: c.Revision, ApproveAs: actor, Lease: e.recommendationLease(),
		Reason: "approved by operator action"})
	if err != nil {
		return nil, fmt.Errorf("claim recommendation %d: %w", c.ID, err)
	}
	return &claim, nil
}

// actionLogWriter inserts one action_log row: the pool, or the
// transaction that also records the recommendation transition.
type actionLogWriter interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// recordAction inserts an action_log row and, for a claimed
// recommendation, records applying → applied (or → failed) in the same
// transaction. If that transaction fails the action is still recorded
// alone: what ran must never be lost from the audit.
func (e *Executor) recordAction(
	ctx context.Context, claim *recommendation.Claim, execErr error,
	insert func(actionLogWriter) (int64, error),
) (int64, error) {
	if claim == nil || e.recs == nil {
		return insert(e.pool)
	}
	var actionID int64
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		id, err := insert(tx)
		if err != nil {
			return err
		}
		actionID = id
		if execErr != nil {
			_, err = recommendation.RecordFailureTx(ctx, tx, *claim, execErr.Error(), &id)
		} else {
			err = recommendation.RecordAppliedTx(ctx, tx, *claim, id)
		}
		if errors.Is(err, recommendation.ErrConflict) {
			e.logFn("executor", "recommendation %d claim lost before its outcome: %v",
				claim.ID, err)
			return nil
		}
		return err
	})
	if err == nil {
		return actionID, nil
	}
	e.logFn("executor", "record recommendation %d outcome: %v", claim.ID, err)
	return insert(e.pool)
}

// settleClaim resolves a claim its action_log transaction did not (a
// refusal before any SQL ran, or a covered index), then starts
// verification of an applied recommendation. Conflicts mean it was
// already resolved.
func (e *Executor) settleClaim(
	ctx context.Context, claim *recommendation.Claim, actionID int64, err error,
) {
	if claim == nil || e.recs == nil {
		return
	}
	var settleErr error
	switch {
	case err != nil:
		_, settleErr = e.recs.RecordFailure(ctx, *claim, err.Error())
	case actionID <= 0:
		_, settleErr = e.recs.RecordFailure(ctx, *claim, "no action was recorded")
	default:
		settleErr = e.recs.RecordApplied(ctx, *claim, actionID)
		if settleErr == nil || errors.Is(settleErr, recommendation.ErrConflict) {
			settleErr = e.recs.StartVerifying(ctx, *claim)
		}
	}
	if settleErr != nil && !errors.Is(settleErr, recommendation.ErrConflict) {
		e.logFn("executor", "settle recommendation %d: %v", claim.ID, settleErr)
	}
}

// logClaimedAction records the action in sage.action_log, together with
// the claimed recommendation's outcome when claim is set.
func (e *Executor) logClaimedAction(
	ctx context.Context, f analyzer.Finding, findingID int64,
	beforeState map[string]any, decisionID int64, execErr error,
	claim *recommendation.Claim,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)
	outcome := actionOutcome(execErr)
	var errReason *string
	if execErr != nil {
		s := execErr.Error()
		errReason = &s
	}
	actionID, err := e.recordAction(ctx, claim, execErr,
		func(q actionLogWriter) (int64, error) {
			var id int64
			err := q.QueryRow(ctx,
				`/* pg_sage */ INSERT INTO sage.action_log
				 (action_type, finding_id, sql_executed, rollback_sql,
				  before_state, outcome, rollback_reason, decision_id, database_id)
				 VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, $7, NULLIF($8, 0), $9::bigint)
				 RETURNING id`,
				categorizeAction(f.RecommendedSQL), findingID, f.RecommendedSQL,
				nilIfEmpty(f.RollbackSQL), beforeJSON, outcome,
				errReason, decisionID, e.databaseIDValue(),
			).Scan(&id)
			return id, err
		})
	if err != nil {
		e.logFn("executor", "failed to log action for %q: %v", f.Title, err)
		return 0
	}
	// Only a successful action marks the finding acted on, so a failed
	// finding stays retryable (lookupFindingID filters on acted_on_at IS NULL).
	if outcome != "failed" && findingID > 0 {
		e.markFindingActioned(ctx, findingID, actionID)
	}
	return actionID
}
