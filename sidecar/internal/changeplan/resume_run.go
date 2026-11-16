package changeplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ResumeReport counts what one resume did: steps moved on from what the
// ledger showed, steps halted interrupted_unknown, steps another engine
// still holds and steps whose read failed (both retried on the next tick).
type ResumeReport struct {
	Resumed, Halted, Waiting, Unreadable int
}

// Resume resumes every step of an approved or running plan that a restart
// or a leader change left in flight. Tick runs it on its first leading
// tick; it returns the steps' failures joined (each step that failed is
// left as it was).
func (e *Engine) Resume(ctx context.Context) (ResumeReport, error) {
	rep, _, err := e.resumeAll(ctx)
	return rep, err
}

// resumeAll is Resume, with the plans it did not settle.
func (e *Engine) resumeAll(ctx context.Context) (ResumeReport, map[int64]bool, error) {
	var rep ResumeReport
	unsettled := map[int64]bool{}
	ids, err := inFlightPlans(ctx, e.cfg.Pool)
	if err != nil {
		return rep, unsettled, err
	}
	var errs []error
	for _, id := range ids {
		p, err := (&Store{pool: e.cfg.Pool}).Get(ctx, id)
		if err != nil {
			unsettled[id] = true
			errs = append(errs, err)
			continue
		}
		for _, s := range p.Steps {
			settled, err := e.resumeStep(ctx, p, s, &rep)
			if err != nil {
				errs = append(errs, fmt.Errorf("plan %d step %d: resume: %w", p.ID,
					s.Ordinal, err))
			}
			if !settled || err != nil {
				unsettled[id] = true
			}
		}
	}
	if rep != (ResumeReport{}) {
		e.cfg.LogFn("plan", "resume %s: %d step(s) resumed, %d halted %s, %d held by "+
			"another engine, %d unread", resumedNote, rep.Resumed, rep.Halted,
			ReasonInterruptedUnknown, rep.Waiting, rep.Unreadable)
	}
	return rep, unsettled, errors.Join(errs...)
}

// resumeStep resumes step s of plan p. settled is false while another
// engine holds its claim.
func (e *Engine) resumeStep(ctx context.Context, p Plan, s Step,
	rep *ResumeReport) (bool, error) {
	if restingStates[s.Runtime.State] {
		return true, nil
	}
	if !knownState(s.Runtime.State) {
		rep.Halted++
		return true, e.haltUnknown(ctx, p, s, resumeMove(s, resumeEvidence{}).reason)
	}
	if claimHeldByOther(s, e.cfg.Now(), e.cfg.Owner) {
		rep.Waiting++
		return false, nil
	}
	if err := e.takeClaim(ctx, p.ID, s.ID, s.Runtime.State); err != nil {
		if errors.Is(err, ErrStepMoved) {
			rep.Waiting++
			return false, nil
		}
		return true, err
	}
	ev, err := e.resume.evidence(ctx, p, s)
	if err != nil {
		rep.Unreadable++
		return true, err
	}
	d := resumeMove(s, ev)
	e.cfg.LogFn("plan", "plan %d step %d (%s): resume %s: %s", p.ID, s.Ordinal,
		s.Runtime.State, resumedNote, resumeName(d))
	if d.kind == resumeUnknown {
		rep.Halted++
	} else {
		rep.Resumed++
	}
	return true, e.applyResume(ctx, p, s, d, ev)
}

// resumeName says what decision d does, for the log.
func resumeName(d resumeDecision) string {
	switch d.kind {
	case resumeRetryStart:
		return "start it again (no queue item)"
	case resumeAdoptQueue:
		return "run its pending queue item"
	case resumeRunQueued:
		return "run its queue item (no run in the ledger)"
	case resumeToJudging:
		return "judge its run"
	case resumeFail:
		return "its run failed"
	case resumeRevert:
		return "read its revert back"
	case resumeUnknown:
		return ReasonInterruptedUnknown + ": " + d.reason
	}
	return "nothing"
}

// applyResume carries out decision d on step s.
func (e *Engine) applyResume(ctx context.Context, p Plan, s Step, d resumeDecision,
	ev resumeEvidence) error {
	switch d.kind {
	case resumeRetryStart, resumeAdoptQueue, resumeRunQueued:
		return e.resumeRun(ctx, p, s, d.kind, ev)
	case resumeToJudging:
		return e.resumeJudging(ctx, p, s, ev)
	case resumeFail:
		return e.failStep(ctx, p.ID, s.ID, int(ev.queueID), fmt.Sprintf(
			"action %d failed (read back %s)", ev.ledgerID, resumedNote))
	case resumeRevert:
		return e.revertStep(ctx, p, s, fmt.Sprintf("plan %d step %d: resume its revert %s",
			p.ID, s.Ordinal, resumedNote))
	case resumeUnknown:
		return e.haltUnknown(ctx, p, s, d.reason)
	}
	return nil
}

// resumeRun starts step s again, adopts its pending queue item or runs
// its queued item, as the plan's approver.
func (e *Engine) resumeRun(ctx context.Context, p Plan, s Step, kind resumeKind,
	ev resumeEvidence) error {
	var approver int
	if err := e.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		approver, err = startApprover(ctx, tx, p, s)
		return err
	}); err != nil {
		return err
	}
	switch kind {
	case resumeRetryStart:
		return e.runStep(ctx, p, s, approver)
	case resumeAdoptQueue:
		qid := ev.queueID
		if err := e.transition(ctx, p.ID, s.ID, StateStarting, StateQueued,
			stepWrite{queueID: &qid, claim: true,
				detail: map[string]any{"resumed": resumedNote}}); err != nil {
			return err
		}
		return e.runQueued(ctx, p, s, int(qid), approver, true)
	}
	return e.runQueued(ctx, p, s, int(ev.queueID), approver, ev.queueStatus == "pending")
}

// resumeJudging records a running step's run from the ledger (executed,
// and its queue item executed), then moves the step to judging.
func (e *Engine) resumeJudging(ctx context.Context, p Plan, s Step, ev resumeEvidence) error {
	detail := map[string]any{"resumed": resumedNote}
	if s.Runtime.State == StateRunning {
		id := ev.ledgerID
		at := ev.ledgerAt
		if at == nil {
			now := e.cfg.Now()
			at = &now
		}
		if err := e.transition(ctx, p.ID, s.ID, StateRunning, StateExecuted,
			stepWrite{actionLogID: &id, executedAt: at, claim: true,
				detail: map[string]any{"action_log_id": id, "resumed": resumedNote}}); err != nil {
			return err
		}
		if ev.queueID > 0 {
			if err := e.actions.MarkExecuted(ctx, int(ev.queueID), id,
				"monitoring"); err != nil {
				e.cfg.LogFn("plan", "plan %d step %d: %v", p.ID, s.Ordinal, err)
			}
		}
	}
	return e.transition(ctx, p.ID, s.ID, StateExecuted, StateJudging,
		stepWrite{claim: true, detail: detail})
}

// haltUnknown halts step s (where a legal move allows it) and plan p with
// interrupted_unknown and why; the notification goes out after the commit.
func (e *Engine) haltUnknown(ctx context.Context, p Plan, s Step, why string) error {
	reason := ReasonInterruptedUnknown + ": " + why
	var next followUp
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		cur, ok, err := lockForResume(ctx, tx, p.ID)
		if err != nil || !ok {
			return err
		}
		if legal(s.Runtime.State, StateHalted) {
			if err := e.transitionTx(ctx, tx, p.ID, cur.PlanHash, s.ID, s.Runtime.State,
				StateHalted, stepWrite{reason: reason, claim: true,
					detail: map[string]any{"resumed": resumedNote}}); err != nil {
				return err
			}
			s.Runtime.State, s.Runtime.StateReason = StateHalted, reason
		}
		next, err = e.halt(ctx, tx, cur, s, reason, map[string]any{"interrupted": true})
		return err
	})
	if err != nil || next == nil {
		return err
	}
	return next(ctx)
}

// lockForResume locks plan planID's row while it is approved or running.
func lockForResume(ctx context.Context, tx pgx.Tx, planID int64) (Plan, bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.plan WHERE id = $1
		AND status IN ('approved', 'running') FOR UPDATE`, planID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, fmt.Errorf("lock plan %d: %w", planID, err)
	}
	p, err := loadPlan(ctx, tx, planID)
	return p, err == nil, err
}
