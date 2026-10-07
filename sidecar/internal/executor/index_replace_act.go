package executor

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// actOnReplace carries out a check's decision: keep (record the verdict,
// start the soft-drop watch), roll back, re-create the old index, or end
// the watch.
func (e *Executor) actOnReplace(ctx context.Context, rec *indexReplaceRecord,
	plan monitorPlan, d replaceDecision, in replaceJudgeInput, evidence map[string]any,
	cfg RollbackMonitorConfig, now time.Time) error {
	switch d.Action {
	case actKeep:
		o := replaceOutcome(rec.ActionLogID, plan, d, in, evidence, now)
		settleOutcome(ctx, e.pool, o, e.logFn)
		rec.Phase = replacePhaseWatch
		return e.setReplaceFields(ctx, *rec)
	case actRollback:
		recordVerdict(ctx, e.pool, replaceOutcome(rec.ActionLogID, plan, d, in, evidence,
			now), e.logFn)
		return e.undoFromWatch(ctx, rec, false, d, cfg)
	case actRestoreOld:
		return e.undoFromWatch(ctx, rec, true, d, cfg)
	case actDone:
		rec.Phase = replacePhaseDone
		return e.setReplaceFields(ctx, *rec)
	}
	return nil
}

// undoFromWatch rolls back (or, keepNew, re-creates the old index) after a
// check, when the rollback authorizer allows it now.
func (e *Executor) undoFromWatch(ctx context.Context, rec *indexReplaceRecord, keepNew bool,
	d replaceDecision, cfg RollbackMonitorConfig) error {
	if cfg.Authorize == nil || !cfg.Authorize(ctx, rec.Plan.RecreateSQL) {
		if !keepNew {
			setMonitoredOutcome(ctx, e.pool, rec.ActionLogID, "rollback_skipped",
				"rollback withheld: "+d.Reason)
		}
		rec.Phase = replacePhaseDone
		return errors.Join(errors.New("index replace: rollback withheld by policy"),
			e.setReplaceFields(ctx, *rec))
	}
	to := replaceRollbackRecreating
	if keepNew {
		to = replaceOldRestoring
	} else if !setMonitoredOutcome(ctx, e.pool, rec.ActionLogID, "rolling_back", d.Reason) {
		e.logFn("executor", "action %d changed state; rollback not repeated",
			rec.ActionLogID)
	}
	if err := e.moveReplace(ctx, rec, to, d.Reason, replaceCompleted); err != nil {
		return err
	}
	if err := e.applyUndo(ctx, rec, keepNew, "automatic rollback: "+d.Reason); err != nil {
		return err
	}
	if keepNew {
		return mergeAfterState(ctx, e.pool, rec.ActionLogID, "soft_drop", map[string]any{
			"trigger": d.Reason, "definition": rec.OldDefinition, "recreated": true})
	}
	return nil
}

// replaceOutcome is a check's verdict for the outcome ledger.
func replaceOutcome(actionID int64, plan monitorPlan, d replaceDecision,
	in replaceJudgeInput, evidence map[string]any, now time.Time) verify.Outcome {
	start, end := plan.executedAt, now
	p := plan.prediction
	p.Class = verify.ClassIndexReplace
	return verify.Outcome{ActionLogID: actionID, Class: verify.ClassIndexReplace,
		Predicted: p, Verdict: d.Verdict, Reason: d.Reason, Evidence: evidence,
		WindowStart: &start, WindowEnd: &end, Observed: observedFrom(in.Targets, nil)}
}
