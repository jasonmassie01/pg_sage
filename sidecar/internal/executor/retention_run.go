package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// retentionRun carries one retention batch from admission to verification.
type retentionRun struct {
	executor *Executor
	request  RetentionRequest
	outcome  RetentionOutcome
}

// admit refuses a request the pipeline cannot run or verify, before any
// lease, slot or action record.
func (r *retentionRun) admit(context.Context, int64) error {
	switch {
	case r.executor.pool == nil:
		return errors.New("retention delete: database pool unavailable")
	case r.request.Delete == nil:
		return errors.New("retention delete: no batch to run")
	case strings.TrimSpace(r.request.Target) == "":
		return errors.New("retention delete: no target table")
	case r.request.BatchLimit <= 0:
		return errors.New("retention delete: batch limit must be positive")
	case r.request.Bound <= 0:
		return errors.New("retention delete: the reviewed dry run authorizes no more rows")
	}
	return nil
}

// maxRows is the most the batch may delete: its own limit and what the
// reviewed dry run still authorizes.
func (r *retentionRun) maxRows() int64 {
	return min(int64(r.request.BatchLimit), r.request.Bound)
}

// statement describes the batch for the action log and the lease. It is
// never executed: the batch's own SQL binds the cutoff as a parameter.
func (r *retentionRun) statement() string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s < '%s' (retention batch of at most %d rows)",
		r.request.Target, r.request.Column, r.request.Cutoff.UTC().Format(time.RFC3339),
		r.maxRows())
}

// execute records the action before anything is deleted, then runs the
// batch under the decision's lock timeout. A batch that refuses (identity
// changed, lock timeout, in-transaction verification) rolls back and the
// action is recorded as failed.
func (r *retentionRun) execute(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
	e := r.executor
	actionID, err := r.recordStart(ctx, decision.DecisionID)
	if err != nil {
		return 0, err
	}
	outcome, err := r.request.Delete(ctx, RetentionExecution{
		ActionID:      actionID,
		LockTimeoutMS: e.lockTimeoutMS(r.statement(), decision),
		MaxRows:       r.maxRows(),
	})
	if err != nil {
		updateActionOutcome(ctx, e.pool, actionID, "failed",
			"retention batch rolled back: "+err.Error())
		return actionID, fmt.Errorf("retention batch: %w", err)
	}
	r.outcome = outcome
	return actionID, nil
}

func (r *retentionRun) recordStart(ctx context.Context, decisionID int64) (int64, error) {
	e := r.executor
	before, err := json.Marshal(map[string]any{
		"target": r.request.Target, "retention_column": r.request.Column,
		"cutoff": r.request.Cutoff.UTC(), "candidate_rows": r.request.Candidates,
		"reviewed_bound": r.request.Bound, "max_rows": r.maxRows(),
		"dry_run_id": r.request.DryRunID,
	})
	if err != nil {
		return 0, fmt.Errorf("encode retention action: %w", err)
	}
	var actionID int64
	err = e.pool.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.action_log
		(action_type, sql_executed, before_state, outcome, decision_id, database_id)
		VALUES ('retention_delete', $1, $2, 'pending', NULLIF($3, 0), $4::bigint)
		RETURNING id`, r.statement(), before, decisionID, e.databaseIDValue()).
		Scan(&actionID)
	if err != nil {
		return 0, fmt.Errorf("record retention action (nothing deleted): %w", err)
	}
	return actionID, nil
}

// verify checks the committed batch against what it was authorized for
// and records the verdict. The batch verified its rows before commit; this
// independently re-checks its report and its durable record.
func (r *retentionRun) verify(ctx context.Context, actionID int64) error {
	e := r.executor
	if err := r.verifyOutcome(ctx, actionID); err != nil {
		if _, finalizeErr := finalizeActionVerification(ctx, e.pool, actionID,
			"unverifiable", err.Error()); finalizeErr != nil {
			e.logFn("executor", "record retention verification for action %d: %v",
				actionID, finalizeErr)
		}
		updateActionOutcome(ctx, e.pool, actionID, "failed", err.Error())
		return err
	}
	r.recordSuccess(ctx, actionID)
	return nil
}

func (r *retentionRun) verifyOutcome(ctx context.Context, actionID int64) error {
	o := r.outcome
	if o.OutsidePredicate != 0 || o.OutsideRelation != 0 {
		return fmt.Errorf("%w: %d deleted rows outside the declared predicate, %d outside "+
			"the contracted relation", ErrRetentionUnverified, o.OutsidePredicate,
			o.OutsideRelation)
	}
	if o.Deleted < 0 || o.Deleted > r.maxRows() {
		return fmt.Errorf("%w: deleted %d rows, the reviewed bound allowed %d",
			ErrRetentionUnverified, o.Deleted, r.maxRows())
	}
	var disposition string
	var recorded, recordedAction int64
	err := r.executor.pool.QueryRow(ctx, `/* pg_sage */ SELECT disposition, deleted_rows,
		COALESCE(action_id, 0) FROM sage.retention_run WHERE id=$1`, o.RunID).
		Scan(&disposition, &recorded, &recordedAction)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: retention run %d is not recorded", ErrRetentionUnverified,
			o.RunID)
	}
	if err != nil {
		return fmt.Errorf("%w: read retention run %d: %w", ErrRetentionUnverified, o.RunID, err)
	}
	if disposition != "applied" || recorded != o.Deleted || recordedAction != actionID {
		return fmt.Errorf("%w: retention run %d records %s of %d rows for action %d; the "+
			"batch reported %d rows for action %d", ErrRetentionUnverified, o.RunID,
			disposition, recorded, recordedAction, o.Deleted, actionID)
	}
	return nil
}

func (r *retentionRun) recordSuccess(ctx context.Context, actionID int64) {
	e, o := r.executor, r.outcome
	if _, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_log
		SET outcome='success', measured_at=now(),
		    after_state=jsonb_build_object('deleted_rows', $2::bigint,
		                                   'retention_run_id', $3::bigint)
		WHERE id=$1`, actionID, o.Deleted, o.RunID); err != nil {
		e.logFn("executor", "record retention action %d success: %v", actionID, err)
	}
	reason := fmt.Sprintf("retention batch verified: %d rows within the reviewed bound "+
		"of %d, all inside the declared predicate", o.Deleted, r.maxRows())
	if _, err := finalizeActionVerification(ctx, e.pool, actionID, "success",
		reason); err != nil {
		e.logFn("executor", "record retention verification for action %d: %v", actionID, err)
	}
}
