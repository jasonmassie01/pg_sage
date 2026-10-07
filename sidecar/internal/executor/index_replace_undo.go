package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/value"
)

// RollbackIndexReplace undoes a replacement: re-create the old index from
// its kept definition, then drop the new one, under the table lease. It
// is the operator's rollback of the action (RollbackAction routes here).
func (e *Executor) RollbackIndexReplace(ctx context.Context, actionID int64,
	reason string) error {
	if err := e.manualMutationBlock(ctx); err != nil {
		return err
	}
	rec, err := e.loadReplaceByAction(ctx, actionID)
	if err != nil {
		return fmt.Errorf("load the replacement of action %d: %w", actionID, err)
	}
	if !e.claimRollback(ctx, actionID) {
		return fmt.Errorf("action already rolled back or rollback in progress")
	}
	if err := e.moveReplace(ctx, &rec, replaceRollbackRecreating, "", replaceCompleted,
		replaceDropFailed, replaceOldRestored); err != nil {
		return fmt.Errorf("replacement of action %d cannot be rolled back from %s: %w",
			actionID, rec.State, err)
	}
	return e.applyUndo(ctx, &rec, false, nonEmpty(reason, "manual rollback"))
}

// applyUndo re-creates the old index and, unless keepNew (the soft drop's
// re-create after a verified gain), drops the new one. It runs through
// Apply under an operator-approved rollback authorization and the table
// lease; every step is idempotent, so a resumed undo continues.
func (e *Executor) applyUndo(ctx context.Context, rec *indexReplaceRecord, keepNew bool,
	reason string) error {
	_, err := e.Apply(ctx, ActionIntent{
		Authorize: func(ctx context.Context) (ActionPolicyDecision, error) {
			return e.authorizeReplaceUndo(ctx, *rec)
		},
		TargetLease: operatorLease(rec.pairSQL(), rec.ApprovedBy),
		WaitForSlot: true,
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			return rec.ActionLogID, e.undoSteps(ctx, rec, keepNew, decision)
		},
	})
	var withheld *WithheldError
	if errors.As(err, &withheld) {
		return err // retried by the next resume; nothing ran
	}
	if err != nil {
		return e.failUndo(ctx, rec, err)
	}
	return e.finishUndo(ctx, rec, keepNew, reason)
}

// authorizeReplaceUndo authorizes the undo as part of the operator's
// approval (the card showed it): an operator-approved rollback request.
// Emergency stop, a disabled executor and replicas still refuse it.
func (e *Executor) authorizeReplaceUndo(ctx context.Context, rec indexReplaceRecord) (
	ActionPolicyDecision, error) {
	gate := e.StandingPolicyGate()
	if gate == nil {
		return ActionPolicyDecision{}, fmt.Errorf("%s", reasonNoStandingPolicy)
	}
	req, _ := operatorRequest(rec.pairSQL(), int(rec.FindingID), rec.ApprovedBy)
	req.SQL, req.Rollback = rec.Plan.RecreateSQL, true
	decision := standingPolicyDecision(gate.Authorize(ctx, req))
	if decision.Decision != PolicyDecisionExecute {
		return decision, &WithheldError{Decision: decision}
	}
	return decision, nil
}

// undoSteps: re-create the old index when it is not valid (dropping an
// INVALID remnant of its name first), then drop the new index.
func (e *Executor) undoSteps(ctx context.Context, rec *indexReplaceRecord, keepNew bool,
	decision ActionPolicyDecision) error {
	c, err := e.replaceCatalogView(ctx, rec.Plan)
	if err != nil {
		return err
	}
	if !c.OldValid {
		if err := e.dropInvalidIndex(ctx, rec.Plan.OldIndex); err != nil {
			return fmt.Errorf("drop the invalid remnant of %s: %w", rec.Plan.OldIndex, err)
		}
		sql := rec.Plan.RecreateSQL
		if err := ExecConcurrently(ctx, e.pool, sql, e.ddlTimeout(),
			e.lockOption(sql, decision)); err != nil {
			return fmt.Errorf("re-create %s: %w", rec.Plan.OldIndex, err)
		}
	}
	if keepNew {
		return e.moveReplace(ctx, rec, replaceOldRestored, "", replaceOldRestoring)
	}
	if err := e.moveReplace(ctx, rec, replaceRollbackDropping, "", replaceRollbackRecreating,
		replaceRollbackDropping); err != nil {
		return err
	}
	if err := e.dropNewIndex(ctx, *rec); err != nil {
		return err
	}
	return e.moveReplace(ctx, rec, replaceRolledBack, "", replaceRollbackDropping)
}

// dropNewIndex drops the new index when it is the one this replacement
// built (its recorded OID); an INVALID one is a remnant of the build.
func (e *Executor) dropNewIndex(ctx context.Context, rec indexReplaceRecord) error {
	oid, err := e.indexOID(ctx, rec.Plan.NewIndex)
	if err != nil || oid == 0 {
		return err
	}
	if rec.NewOID > 0 && oid != rec.NewOID {
		return fmt.Errorf("%w: %s is OID %d, built as %d; it is kept",
			ErrReplaceIdentityChanged, rec.Plan.NewIndex, oid, rec.NewOID)
	}
	sql := rec.Plan.DropNewSQL
	return ExecConcurrently(ctx, e.pool, sql, e.ddlTimeout(),
		WithLockTimeout(e.lockTimeoutMS(sql, ActionPolicyDecision{})))
}

func (e *Executor) failUndo(ctx context.Context, rec *indexReplaceRecord, cause error) error {
	err := e.moveReplace(ctx, rec, replaceRollbackFailed, cause.Error(),
		replaceRollbackRecreating, replaceRollbackDropping, replaceOldRestoring)
	if rec.ActionLogID > 0 {
		updateActionOutcome(ctx, e.pool, rec.ActionLogID, "rollback_failed",
			"index replace rollback failed: "+cause.Error())
	}
	return errors.Join(fmt.Errorf("undo the replacement: %w", cause), err)
}

// finishUndo records the undone replacement on its action and ends its
// verification.
func (e *Executor) finishUndo(ctx context.Context, rec *indexReplaceRecord, keepNew bool,
	reason string) error {
	rec.Phase = replacePhaseDone
	err := e.setReplaceFields(ctx, *rec)
	if rec.ActionLogID <= 0 {
		return err
	}
	if keepNew {
		e.notifyPostDDL(ctx, rec.Plan.RecreateSQL)
		return err
	}
	updateActionOutcome(ctx, e.pool, rec.ActionLogID, "rolled_back", reason)
	if _, ferr := finalizeActionVerification(ctx, e.pool, rec.ActionLogID, "revert",
		reason); ferr != nil {
		err = errors.Join(err, ferr)
	}
	if _, zerr := value.NewPostgresRepository(e.pool).ZeroCreditOnRevert(ctx,
		rec.ActionLogID, "rolled_back"); zerr != nil {
		e.logFn("executor", "zero credit of rolled back action %d: %v", rec.ActionLogID, zerr)
	}
	e.notifyPostDDL(ctx, rec.Plan.RecreateSQL)
	return err
}
