package changeplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/notify"
)

// decideAndApply makes plan p's next decision in tx (the plan row held).
// Starting and reverting a step continue after the commit (followUp), so
// no transaction is open while the executor changes the database; a
// revert goes on along its revert_to_checkpoint walk (continueWalk).
func (e *Engine) decideAndApply(ctx context.Context, tx pgx.Tx, p Plan) (followUp, error) {
	m := decide(p, e.cfg.Now(), e.cfg.Owner)
	next, err := e.apply(ctx, tx, p, m)
	if err != nil || next == nil || (m.kind != moveRevert && m.kind != moveWalkRevert) {
		return next, err
	}
	return func(ctx context.Context) error {
		if err := next(ctx); err != nil {
			return err
		}
		return e.continueWalk(ctx, p.ID)
	}, nil
}

// apply carries out move m on plan p in tx.
func (e *Engine) apply(ctx context.Context, tx pgx.Tx, p Plan, m move) (followUp, error) {
	switch m.kind {
	case moveJudgeStart:
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, m.step.ID, StateExecuted,
			StateJudging, stepWrite{claim: true})
	case moveJudge:
		return e.judge(ctx, tx, p, m.step)
	case moveSkip:
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, m.step.ID, StatePending,
			StateSkipped, stepWrite{reason: m.reason})
	case moveCheck:
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, m.step.ID, StatePending,
			StateKept, stepWrite{reason: "check step: nothing to change"})
	case moveOperator:
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, m.step.ID, StatePending,
			StateAwaitingOperator, stepWrite{reason: "an operator runs this step"})
	case moveComplete:
		return nil, e.setStatus(ctx, tx, p, StatusCompleted, EventCompleted,
			map[string]any{"steps": len(p.Steps)})
	case moveHalt:
		return e.haltMove(ctx, tx, p, m)
	case moveStart:
		return e.prepareStart(ctx, tx, p, m.step)
	case moveRevert:
		return e.resumeRevert(p, m.step), nil
	case moveWalkRevert:
		return e.startWalkRevert(ctx, tx, p, m)
	}
	return nil, nil
}

// prepareStart checks that step s may start now and writes starting (with
// this engine's claim and the attempt) before anything else happens. The
// plan's rows must hash to its live approval (else the plan is voided); a
// never-automatic step stops with awaiting_approval; another plan's step
// on the same queries, any setting step, a setting step's company, an
// in-flight change on its objects, a plan cap and a peak window make it
// wait (holdBeforeStart).
func (e *Engine) prepareStart(ctx context.Context, tx pgx.Tx, p Plan, s Step) (followUp,
	error) {
	if err := verifyOn(ctx, tx, p.ID); err != nil {
		if errors.Is(err, ErrHashMismatch) || errors.Is(err, ErrNotApproved) {
			return nil, e.void(ctx, tx, p, err)
		}
		return nil, err
	}
	approver, err := liveApprover(ctx, tx, p.ID)
	if err != nil {
		return nil, err
	}
	if next, held, err := e.holdBeforeStart(ctx, tx, p, s); held || err != nil {
		return next, err
	}
	if p.Status == StatusApproved {
		if err := e.setStatus(ctx, tx, p, StatusRunning, "", nil); err != nil {
			return nil, err
		}
	}
	err = e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StatePending, StateStarting,
		stepWrite{claim: true, attempt: true, detail: map[string]any{"approver": approver}})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return e.runStep(ctx, p, s, approver) }, nil
}

// mustWait is why s cannot start now ("" when it can). A lookup that fails
// makes it wait (fail closed), logged.
func (e *Engine) mustWait(ctx context.Context, tx pgx.Tx, p Plan, s Step) (string, error) {
	others, err := otherInFlight(ctx, tx, p.ID)
	if err != nil {
		return "", err
	}
	if o, ok := conflict(s, others); ok {
		return fmt.Sprintf("plan %d step %d is in flight", o.planID, o.ordinal), nil
	}
	if isSetting(s) {
		busy, err := e.cfg.Waits.AnyInFlight(ctx)
		if err != nil {
			return "the in-flight lookup failed: " + err.Error(), nil
		}
		if busy {
			return "a setting step runs only when no change is in flight", nil
		}
	}
	pending, err := e.cfg.Waits.PendingFor(ctx, s.ProposedSQL, nil)
	if err != nil {
		return "the in-flight lookup failed: " + err.Error(), nil
	}
	if len(pending) > 0 {
		return fmt.Sprintf("%s has a change in flight (action %d)", pending[0].Object,
			pending[0].ActionID), nil
	}
	return "", nil
}

func otherInFlight(ctx context.Context, tx pgx.Tx, planID int64) ([]otherStep, error) {
	states := make([]string, 0, len(inFlightStates))
	for s := range inFlightStates {
		states = append(states, string(s))
	}
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT plan_id, ordinal,
		COALESCE(target_queryids, '{}'), proposed_sql FROM sage.plan_step
		WHERE plan_id <> $1 AND state = ANY($2)`, planID, states)
	if err != nil {
		return nil, fmt.Errorf("read the other plans' steps in flight: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (otherStep, error) {
		var o otherStep
		return o, r.Scan(&o.planID, &o.ordinal, &o.targets, &o.sql)
	})
	if err != nil {
		return nil, fmt.Errorf("read the other plans' steps in flight: %w", err)
	}
	return out, nil
}

func liveApprover(ctx context.Context, tx pgx.Tx, planID int64) (int, error) {
	var id int
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT approver_user_id FROM sage.plan_approval
		WHERE plan_id = $1 AND voided_at IS NULL`, planID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("read the approver of plan %d: %w", planID, err)
	}
	return id, nil
}

// void ends a plan whose rows no longer match its approval, with why.
func (e *Engine) void(ctx context.Context, tx pgx.Tx, p Plan, why error) error {
	reason := "hash_mismatch"
	if errors.Is(why, ErrNotApproved) {
		reason = "no_live_approval"
	}
	if _, err := voidLive(ctx, tx, p.ID, reason); err != nil {
		return err
	}
	e.cfg.LogFn("plan", "plan %d voided before its next step: %v", p.ID, why)
	return e.setStatus(ctx, tx, p, StatusVoided, EventVoided,
		map[string]any{"reason": reason, "detail": why.Error()})
}

// halt stops plan p: status halted, one halted event and, after the
// commit, one notification naming the plan, the step and why. A halted
// plan starts nothing until a person resumes or cancels it.
func (e *Engine) halt(ctx context.Context, tx pgx.Tx, p Plan, s Step,
	reason string, extra map[string]any) (followUp, error) {
	detail := map[string]any{"reason": truncate(reason, 1000)}
	for k, v := range extra {
		detail[k] = v
	}
	if s.ID != 0 {
		detail["ordinal"], detail["step_state"] = s.Ordinal, string(s.Runtime.State)
		detail["verdict"] = string(s.Runtime.Verdict)
	}
	if err := e.setStatus(ctx, tx, p, StatusHalted, EventHalted, detail); err != nil {
		return nil, err
	}
	evt := e.haltedEvent(p, s, reason)
	return func(ctx context.Context) error {
		if e.cfg.Notify == nil {
			return nil
		}
		if err := e.cfg.Notify.Dispatch(ctx, evt); err != nil {
			e.cfg.LogFn("plan", "plan %d halted; the notification failed: %v", p.ID, err)
		}
		return nil
	}, nil
}

// haltedEvent asks a person to decide a halted plan (an approval_needed
// event without a queue item: it is decided on the plan).
func (e *Engine) haltedEvent(p Plan, s Step, reason string) notify.Event {
	step := "before any step"
	if s.ID != 0 {
		step = fmt.Sprintf("at step %d", s.Ordinal)
	}
	verdict := string(s.Runtime.Verdict)
	if verdict == "" {
		verdict = "none"
	}
	return notify.Event{Type: "approval_needed", Severity: "warning",
		Subject: fmt.Sprintf("Plan %d halted %s", p.ID, step),
		Body: fmt.Sprintf("Plan %d (%s) on %s halted %s. Verdict: %s. Why: %s. Nothing "+
			"else in the plan starts until you resume or cancel it.", p.ID,
			truncate(p.Goal, 120), e.cfg.Database, step, verdict,
			truncate(reason, 500)),
		Data: map[string]any{"plan_id": p.ID, "step": s.Ordinal, "verdict": verdict,
			"database": e.cfg.Database},
		DedupKey: fmt.Sprintf("plan_halted:%s:%d", e.cfg.Database, p.ID)}
}
