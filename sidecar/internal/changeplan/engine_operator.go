package changeplan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Operator steps ("you run this", plan-02d). pg_sage changes nothing in
// them: the engine asks the operator once (awaiting_operator, one
// notification saying what to run), reads the landing each tick, then
// judges the landed change like an action step's. It never reverts one:
// a regression halts and asks, naming the operator's revert command.

// ReasonOperatorNotLanded is an operator step that did not land by its
// deadline (skipped with verdict expired; its on_expired decides).
const ReasonOperatorNotLanded = "operator_not_landed"

// noRevertGiven is the halt's revert text when neither the step nor its
// managed change says how to undo it.
const noRevertGiven = "no revert command was given: restore the prior value by hand"

// askOperator moves pending operator step s to awaiting_operator once the
// plan's approval is live and nothing else runs on the database: an
// operator step changes what pg_sage cannot see, so it runs alone, and
// never inside a peak window. Its landing deadline counts from the ask.
func (e *Engine) askOperator(ctx context.Context, tx pgx.Tx, p Plan, s Step) (followUp,
	error) {
	if err := verifyOn(ctx, tx, p.ID); err != nil {
		if errors.Is(err, ErrHashMismatch) || errors.Is(err, ErrNotApproved) {
			return nil, e.void(ctx, tx, p, err)
		}
		return nil, err
	}
	why, err := e.operatorWait(ctx, tx, p, s)
	if err != nil {
		return nil, err
	}
	if why != "" {
		e.cfg.LogFn("plan", "plan %d step %d waits: %s", p.ID, s.Ordinal, why)
		return nil, e.waitOnCap(ctx, tx, p, s, why)
	}
	if p.Status == StatusApproved {
		if err := e.setStatus(ctx, tx, p, StatusRunning, "", nil); err != nil {
			return nil, err
		}
	}
	detail := map[string]any{"operator_command": truncate(s.OperatorCommand, 1000)}
	if s.Landing != nil {
		detail["landing"] = map[string]any{"setting": s.Landing.Setting,
			"value": s.Landing.Value}
	}
	if s.ManagedChangeID != nil {
		detail["managed_change_id"] = *s.ManagedChangeID
	}
	err = e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StatePending,
		StateAwaitingOperator, stepWrite{reason: "waiting for the operator to run the step",
			claim: true, attempt: true, waitFromNow: true, detail: detail})
	if err != nil {
		return nil, err
	}
	return e.notifyOnce(e.operatorAsk(p, s), p, s), nil
}

// operatorWait is why operator step s may not be asked for now ("" when
// it may): another plan's step in flight, any executor change in flight
// or in verification, or an open peak window. A lookup that fails makes
// it wait (fail closed).
func (e *Engine) operatorWait(ctx context.Context, tx pgx.Tx, p Plan, s Step) (string,
	error) {
	others, err := otherInFlight(ctx, tx, p.ID)
	if err != nil {
		return "", err
	}
	if o, ok := conflict(s, others); ok {
		return fmt.Sprintf("plan %d step %d is in flight", o.planID, o.ordinal), nil
	}
	busy, err := e.cfg.Waits.AnyInFlight(ctx)
	if err != nil {
		return "the in-flight lookup failed: " + err.Error(), nil
	}
	if busy {
		return "an operator step runs only when no change is in flight", nil
	}
	caps, err := e.cfg.Caps(ctx)
	if err != nil {
		return ReasonCapsUnavailable + ": " + err.Error(), nil
	}
	if window, open := caps.PeakWindowAt(e.cfg.Now()); open {
		return fmt.Sprintf("%s: %q is open", ReasonPeakWindow, window), nil
	}
	return "", nil
}

// land reads awaiting_operator step s's landing. Landed by its deadline
// (counted from the ask; landing at the deadline is used), it is judging
// from when it landed; past the deadline unlanded, it is skipped with
// verdict expired; otherwise it waits, recording what it read (a failed
// read is recorded and waits: never landed).
func (e *Engine) land(ctx context.Context, tx pgx.Tx, p Plan, s Step) error {
	now := e.cfg.Now()
	if s.Runtime.WaitingSince == nil {
		_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET waiting_since = $3
			WHERE id = $1 AND plan_id = $2 AND waiting_since IS NULL`, s.ID, p.ID, now)
		return err
	}
	deadline := s.Runtime.WaitingSince.Add(time.Duration(s.DeadlineMinutes) * time.Minute)
	check, err := e.cfg.Landings.CheckLanding(ctx, s, now)
	read := check.Detail
	if err != nil {
		read = "the landing check failed: " + err.Error()
	}
	switch {
	case err == nil && check.Landed && !check.At.After(deadline):
		at := check.At
		return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateAwaitingOperator,
			StateJudging, stepWrite{reason: "landed: " + read, landedAt: &at, claim: true})
	case now.After(deadline):
		return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateAwaitingOperator,
			StateSkipped, stepWrite{reason: ReasonOperatorNotLanded + ": " + read,
				verdict: OutcomeExpired, verdictAt: &now})
	}
	if read == s.Runtime.StateReason {
		return nil
	}
	_, err = tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET state_reason = $3
		WHERE id = $1 AND plan_id = $2 AND state = 'awaiting_operator'`, s.ID, p.ID,
		truncate(read, 1000))
	if err != nil {
		return fmt.Errorf("record the landing read of step %d of plan %d: %w", s.Ordinal,
			p.ID, err)
	}
	return nil
}

// judgeOperator reads landed operator step s's verdict and keeps it, or
// halts with the operator's revert command: pg_sage never reverts an
// operator step (a revert action is refused at validation; one stored
// anyway halts, fail closed). A failed or pending read waits.
func (e *Engine) judgeOperator(ctx context.Context, tx pgx.Tx, p Plan, s Step) error {
	now := e.cfg.Now()
	v, err := e.cfg.Operator.JudgeOperator(ctx, s, now)
	if err != nil {
		e.cfg.LogFn("plan", "plan %d step %d: judge the operator step: %v", p.ID,
			s.Ordinal, err)
		return nil
	}
	o, _, done := judgeOutcome(s, v, now)
	if !done {
		return nil
	}
	w := stepWrite{verdict: o, verdictAt: &now, claim: true,
		detail: map[string]any{"verifier_verdict": v.Verdict}}
	for k, val := range v.Detail {
		w.detail[k] = val
	}
	if w.reason = unknownVerdict(v.Verdict); w.reason != "" {
		return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging, StateHalted, w)
	}
	act := s.ActionFor(o)
	if act == ActionKeep {
		return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging, StateKept, w)
	}
	revert := e.operatorRevert(ctx, s)
	w.detail["operator_revert"] = revert
	w.reason = fmt.Sprintf("verdict %s asks a person (%s): pg_sage does not revert an "+
		"operator step; to undo it, run: %s", o, act, revert)
	return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging, StateHalted, w)
}

// operatorRevert is what undoes operator step s: its own revert text, else
// its managed change's rollback command, else a sentence saying none was
// given. A failed read of the managed change is logged and says so.
func (e *Engine) operatorRevert(ctx context.Context, s Step) string {
	if s.OperatorRevert != "" {
		return s.OperatorRevert
	}
	cmd, err := e.cfg.Landings.RevertCommand(ctx, s)
	if err != nil {
		e.cfg.LogFn("plan", "step %d: read the managed change's rollback: %v", s.Ordinal, err)
		return "the managed change's rollback could not be read (" + err.Error() + "); " +
			noRevertGiven
	}
	if cmd == "" {
		return noRevertGiven
	}
	return cmd
}

// operatorAsk is the one notification that tells the operator what to run.
func (e *Engine) operatorAsk(p Plan, s Step) notify.Event {
	landing := "the linked managed change is applied"
	if s.Landing != nil {
		landing = fmt.Sprintf("%s reads %s", s.Landing.Setting, s.Landing.Value)
		if s.ManagedChangeID != nil {
			landing += " and the linked managed change is applied"
		}
	}
	return notify.Event{Type: "approval_needed", Severity: "info",
		Subject: fmt.Sprintf("Plan %d step %d: you run this", p.ID, s.Ordinal),
		Body: fmt.Sprintf("Plan %d (%s) on %s waits for you at step %d. pg_sage does not "+
			"apply this change. Run: %s. The step lands when %s, within %d minutes; then "+
			"pg_sage judges it.", p.ID, truncate(p.Goal, 120), e.cfg.Database, s.Ordinal,
			truncate(s.OperatorCommand, 2000), landing, s.DeadlineMinutes),
		Data: map[string]any{"plan_id": p.ID, "step": s.Ordinal,
			"database": e.cfg.Database},
		DedupKey: fmt.Sprintf("plan_step_operator:%s:%d:%d", e.cfg.Database, p.ID, s.ID)}
}

// notifyOnce is the follow-up that sends evt after the commit (a failed
// send is logged: the step's state already says what it waits for).
func (e *Engine) notifyOnce(evt notify.Event, p Plan, s Step) followUp {
	return func(ctx context.Context) error {
		if e.cfg.Notify == nil {
			return nil
		}
		if err := e.cfg.Notify.Dispatch(ctx, evt); err != nil {
			e.cfg.LogFn("plan", "plan %d step %d: the notification failed: %v", p.ID,
				s.Ordinal, err)
		}
		return nil
	}
}
