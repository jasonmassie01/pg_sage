package changeplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The engine's reverts (plan-02c). Every revert goes through the
// executor's RollbackAction (the stored rollback SQL under the DDL slot
// and the lock timeout; an index replace routes to RollbackIndexReplace),
// and its landing is read back before the step is recorded reverted: a
// revert that does not land fails the step and halts the plan.

// ErrRevertMode refuses a RevertTo mode that is not a revert.
var ErrRevertMode = errors.New("changeplan: a revert mode is revert_step or " +
	"revert_to_checkpoint")

// RevertTo carries out mode for failed step (reverting since its
// verdict): revert_step reverts it alone; revert_to_checkpoint reverts it
// and then walks back to the latest checkpoint before it, reverting kept
// steps in reverse run order, until the walk ends or stops and the plan
// halts with what was kept and why.
func (e *Engine) RevertTo(ctx context.Context, p Plan, failed Step, mode OutcomeAction) error {
	if mode != ActionRevertStep && mode != ActionRevertToCheckpoint {
		return fmt.Errorf("%w: %q", ErrRevertMode, truncate(string(mode), 50))
	}
	o := failed.Runtime.Verdict
	why := fmt.Sprintf("plan %d step %d: verdict %s (on_%s %s)", p.ID, failed.Ordinal, o, o,
		mode)
	if err := e.revertStep(ctx, p, failed, why); err != nil {
		return err
	}
	if mode == ActionRevertStep {
		return nil
	}
	return e.continueWalk(ctx, p.ID)
}

// continueWalk carries plan planID's revert_to_checkpoint walk on, one
// step at a time under the engines' lock and the plan's row, until it
// ends (the plan halts), waits, or the emergency stop is set.
func (e *Engine) continueWalk(ctx context.Context, planID int64) error {
	for range MaxSteps + 1 {
		if e.cfg.Stopped(ctx) {
			return nil
		}
		var next followUp
		err := e.inTx(ctx, func(tx pgx.Tx) error {
			p, ok, err := lockForTick(ctx, tx, planID)
			if err != nil || !ok {
				return err
			}
			if m := decide(p, e.cfg.Now(), e.cfg.Owner); m.walk != nil {
				next, err = e.apply(ctx, tx, p, m)
			}
			return err
		})
		if err != nil || next == nil {
			return err
		}
		if err := next(ctx); err != nil {
			return err
		}
	}
	return nil
}

// startWalkRevert moves the walk's next kept step to reverting (its event
// names the walk and the action), then reverts it after the commit.
func (e *Engine) startWalkRevert(ctx context.Context, tx pgx.Tx, p Plan, m move) (followUp,
	error) {
	s, w := m.step, m.walk
	err := e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateKept, StateReverting,
		stepWrite{claim: true, kind: EventStepReverted,
			reason: fmt.Sprintf("revert_to_checkpoint from step %d", w.origin.Ordinal),
			detail: map[string]any{"walk_from": w.origin.Ordinal,
				"checkpoint": w.checkpoint, "action_log_id": *s.Runtime.ActionLogID}})
	if err != nil {
		return nil, err
	}
	why := fmt.Sprintf("plan %d step %d: revert_to_checkpoint from step %d", p.ID,
		s.Ordinal, w.origin.Ordinal)
	return func(ctx context.Context) error { return e.revertStep(ctx, p, s, why) }, nil
}

// resumeRevert carries on the revert of a step left reverting (a rollback
// still running outside the plan, a read-back that failed, a restart).
func (e *Engine) resumeRevert(p Plan, s Step) followUp {
	why := fmt.Sprintf("plan %d step %d: resume its revert", p.ID, s.Ordinal)
	return func(ctx context.Context) error { return e.revertStep(ctx, p, s, why) }
}

// haltMove halts plan p; a walk's halt also records why each step it kept
// was kept (state_reason) and lists them in the halted event.
func (e *Engine) haltMove(ctx context.Context, tx pgx.Tx, p Plan, m move) (followUp, error) {
	if m.walk == nil {
		return e.halt(ctx, tx, p, m.step, m.reason, nil)
	}
	ids := map[int]int64{}
	for _, s := range p.Steps {
		ids[s.Ordinal] = s.ID
	}
	for _, k := range m.walk.kept {
		if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET state_reason = $3
			WHERE id = $1 AND plan_id = $2 AND state = 'kept'`, ids[k.Ordinal], p.ID,
			truncate(k.Why, 1000)); err != nil {
			return nil, fmt.Errorf("record why step %d of plan %d was kept: %w", k.Ordinal,
				p.ID, err)
		}
	}
	return e.halt(ctx, tx, p, m.step, m.reason, m.walk.detail())
}

// revertStep rolls back reverting step s's action through the executor and
// reads the revert back: reverted when it landed, failed when it did not.
// An action already rolled back (or being rolled back) outside the plan is
// not rolled back again: its landing decides. A step that is no longer
// reverting is not touched.
func (e *Engine) revertStep(ctx context.Context, p Plan, s Step, why string) error {
	cur, err := e.loadStep(ctx, p.ID, s.ID)
	if err != nil {
		return err
	}
	if cur.Runtime.State != StateReverting {
		return fmt.Errorf("%w: step %d of plan %d is %s, not reverting", ErrStepMoved,
			cur.Ordinal, p.ID, cur.Runtime.State)
	}
	if cur.Runtime.ActionLogID == nil {
		return e.transition(ctx, p.ID, cur.ID, StateReverting, StateFailed,
			stepWrite{reason: "the step has no action to revert"})
	}
	id := *cur.Runtime.ActionLogID
	first, err := e.readLanding(ctx, id)
	if err != nil {
		return e.revertWaits(ctx, p, cur, "read the action before its revert: "+err.Error())
	}
	outside := first.outcome == "rolled_back" || first.outcome == "rolling_back"
	var rbErr error
	if !outside {
		rbErr = e.cfg.Executor.RollbackAction(ctx, id, why)
	}
	land, err := e.readLanding(ctx, id)
	if err != nil {
		return e.revertWaits(ctx, p, cur, "read the revert back: "+err.Error())
	}
	return e.settleRevert(ctx, p, cur, land, outside || rbErr != nil, rbErr)
}

// settleRevert records what the revert of step s did. outside reports a
// rollback that was not (only) this one.
func (e *Engine) settleRevert(ctx context.Context, p Plan, s Step, land revertLanding,
	outside bool, rbErr error) error {
	id := *s.Runtime.ActionLogID
	w := stepWrite{detail: map[string]any{"action_log_id": id, "readback": land.note}}
	switch {
	case land.pending:
		return e.revertWaits(ctx, p, s, fmt.Sprintf("a rollback of action %d is in "+
			"progress outside the plan; the plan reads it back on its next tick", id))
	case land.landed && outside:
		w.reason = fmt.Sprintf("%s: action %d was rolled back outside the plan; %s",
			ReasonRevertedOutside, id, land.note)
	case land.landed:
		w.reason = "reverted by the plan: " + land.note
	case rbErr != nil:
		w.reason = "rollback: " + rbErr.Error()
		return e.transition(ctx, p.ID, s.ID, StateReverting, StateFailed, w)
	default:
		w.reason = "the revert did not land: " + land.note
		return e.transition(ctx, p.ID, s.ID, StateReverting, StateFailed, w)
	}
	return e.transition(ctx, p.ID, s.ID, StateReverting, StateReverted, w)
}

// revertWaits leaves step s reverting with why (logged); the next tick
// resumes it.
func (e *Engine) revertWaits(ctx context.Context, p Plan, s Step, why string) error {
	e.cfg.LogFn("plan", "plan %d step %d: revert waits: %s", p.ID, s.Ordinal, why)
	return e.note(ctx, p.ID, s.ID, StateReverting, why)
}
