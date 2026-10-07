package executor

import (
	"context"
	"errors"
	"fmt"
)

// ResumeIndexReplaces finishes what a crash or a shutdown left between the
// steps of a replacement, and restarts the verification watch of the ones
// being verified. It runs every executor cycle. The forward drop resumes
// only under a fresh operator-approved authorization and the table lease;
// a refusal leaves the replacement where it is for the next cycle.
func (e *Executor) ResumeIndexReplaces(ctx context.Context) error {
	if e.pool == nil {
		return nil
	}
	recs, err := e.loadOpenReplaces(ctx)
	if err != nil {
		return err
	}
	var errs error
	for i := range recs {
		if err := e.resumeReplace(ctx, &recs[i]); err != nil {
			errs = errors.Join(errs, fmt.Errorf("resume replacement %d (%s): %w",
				recs[i].ID, recs[i].Plan.OldIndex, err))
		}
	}
	return errs
}

func (e *Executor) resumeReplace(ctx context.Context, rec *indexReplaceRecord) error {
	if rec.State == replaceCompleted &&
		(rec.Phase == replacePhaseJudging || rec.Phase == replacePhaseWatch) {
		e.watchIndexReplace(ctx, rec.ActionLogID)
		return nil
	}
	c, err := e.replaceCatalogView(ctx, rec.Plan)
	if err != nil {
		return err
	}
	step := resumeStepFor(rec.State, c)
	e.logFn("executor", "resuming replacement %d from %s: %s", rec.ID, rec.State, step)
	switch step {
	case stepFailCreate:
		return e.moveReplace(ctx, rec, replaceCreateFailed,
			"interrupted before the build registered the new index", replaceCreating)
	case stepDropRemnant:
		if err := e.dropInvalidIndex(ctx, rec.Plan.NewIndex); err != nil {
			return err
		}
		return e.moveReplace(ctx, rec, replaceCreateFailed,
			"interrupted during the build; its INVALID remnant was dropped", replaceCreating)
	case stepDropOld, stepComplete:
		return e.resumeForward(ctx, rec, step)
	case stepRestore:
		if err := e.moveReplace(ctx, rec, replaceRollbackRecreating,
			"interrupted: the new index is not valid", replaceCreated,
			replaceDropping); err != nil {
			return err
		}
		return e.applyUndo(ctx, rec, false, "interrupted replacement restored")
	case stepRecreateOld, stepDropNew, stepRolledBack:
		return e.applyUndo(ctx, rec, false, "rollback resumed after a restart")
	case stepRestoreOld, stepOldRestored:
		return e.applyUndo(ctx, rec, true, "soft-drop re-create resumed after a restart")
	}
	if rec.Phase == replacePhaseJudging || rec.Phase == replacePhaseWatch {
		rec.Phase = replacePhaseDone
		return e.setReplaceFields(ctx, *rec)
	}
	return nil
}

// resumeForward continues an approved replacement whose new index is valid:
// it records the build if the crash came before that, then drops the old
// index (or records the completion when the drop had committed).
func (e *Executor) resumeForward(ctx context.Context, rec *indexReplaceRecord,
	step replaceStep) error {
	sql := rec.pairSQL()
	_, err := e.Apply(ctx, ActionIntent{
		Authorize: func(ctx context.Context) (ActionPolicyDecision, error) {
			decision, err := e.authorizeOperatorAction(ctx, sql, int(rec.FindingID),
				rec.ApprovedBy)
			return standingPolicyDecision(decision), err
		},
		TargetLease: operatorLease(sql, rec.ApprovedBy),
		WaitForSlot: true,
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			if err := e.recordResumedBuild(ctx, rec); err != nil {
				return 0, err
			}
			if step == stepComplete {
				return e.completeReplace(ctx, rec, nil)
			}
			return e.replaceDropStep(ctx, rec, decision, nil)
		},
		Verify: func(ctx context.Context, actionID int64) error {
			if rec.State == replaceCompleted {
				e.watchIndexReplace(ctx, actionID)
			}
			return nil
		},
	})
	return err
}

// recordResumedBuild records a build that finished before the crash.
func (e *Executor) recordResumedBuild(ctx context.Context, rec *indexReplaceRecord) error {
	if rec.State != replaceCreating {
		return nil
	}
	oid, err := e.indexOID(ctx, rec.Plan.NewIndex)
	if err != nil {
		return err
	}
	rec.NewOID = oid
	if err := e.moveReplace(ctx, rec, replaceCreated, "", replaceCreating); err != nil {
		return err
	}
	return e.setReplaceFields(ctx, *rec)
}
