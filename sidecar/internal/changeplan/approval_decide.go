package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// A person's other plan decisions (plan-03a): reject a proposed plan,
// resume a halted one, cancel an approved or halted one. Each locks the
// plan row and records one event naming the person.

// Errors the decisions return when the plan is in the wrong status.
var (
	ErrNotRejectable  = errors.New("changeplan: only a proposed plan can be rejected")
	ErrNotResumable   = errors.New("changeplan: only a halted plan can be resumed")
	ErrNotCancellable = errors.New("changeplan: only an approved or halted plan can be " +
		"cancelled")
)

// checkReason trims reason and requires 1..200 characters (blank is
// allowed when optional).
func checkReason(reason string, optional bool) (string, error) {
	reason = strings.TrimSpace(reason)
	n := utf8.RuneCountInString(reason)
	switch {
	case n == 0 && optional:
		return "", nil
	case n == 0:
		return "", refuse(CodeReasonRequired, "a reason is required")
	case n > maxRefRunes:
		return "", refuse(CodeReasonRequired, "the reason is %d characters; at most %d", n,
			maxRefRunes)
	}
	return reason, nil
}

// Reject ends proposed plan planID as rejected, with userID's reason; any
// step approval given before is voided.
func (s *Store) Reject(ctx context.Context, planID int64, reason string, userID int) error {
	reason, err := checkReason(reason, false)
	if err != nil {
		return fmt.Errorf("changeplan: reject plan %d: %w", planID, err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		return endPlan(ctx, tx, planID, StatusRejected, EventRejected, reason, userID,
			func(st Status) bool { return st == StatusProposed }, ErrNotRejectable)
	})
	if err != nil {
		return fmt.Errorf("changeplan: reject plan %d: %w", planID, err)
	}
	return nil
}

// Cancel ends approved or halted plan planID as cancelled (never a running
// one: its step in flight is the executor's); its approvals are voided.
// reason may be blank.
func (s *Store) Cancel(ctx context.Context, planID int64, reason string, userID int) error {
	reason, err := checkReason(reason, true)
	if err != nil {
		return fmt.Errorf("changeplan: cancel plan %d: %w", planID, err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		return endPlan(ctx, tx, planID, StatusCancelled, EventCancelled, reason, userID,
			func(st Status) bool { return st == StatusApproved || st == StatusHalted },
			ErrNotCancellable)
	})
	if err != nil {
		return fmt.Errorf("changeplan: cancel plan %d: %w", planID, err)
	}
	return nil
}

// endPlan moves the locked plan to terminal status to when allowed accepts
// its status (else refused), voiding every approval.
func endPlan(ctx context.Context, tx pgx.Tx, planID int64, to Status, kind EventKind,
	reason string, userID int, allowed func(Status) bool, refused error) error {
	if userID <= 0 {
		return errors.New("no person decided")
	}
	p, err := lockPlan(ctx, tx, planID)
	if err != nil {
		return err
	}
	if !allowed(p.Status) {
		return fmt.Errorf("%w: plan %d is %s", refused, planID, p.Status)
	}
	if err := voidAll(ctx, tx, planID, string(to)); err != nil {
		return err
	}
	if err := setPlan(ctx, tx, planID, to, p.PlanHash); err != nil {
		return err
	}
	detail := map[string]any{"from_status": string(p.Status)}
	if reason != "" {
		detail["reason"] = reason
	}
	_, err = AppendEvent(ctx, tx, Event{PlanID: planID, Kind: kind, PlanHash: p.PlanHash,
		Actor: userActor(userID), Detail: detail})
	return err
}

// Resume sets halted plan planID running again at planHash, the hash the
// person was shown, which must still be the approved one. Each step that
// halted it is marked resumed so it no longer halts the plan: a failed
// step becomes skipped (its dependents are skipped), a halted step kept
// (its change stays in place); a step regressed or reverted keeps its
// state.
func (s *Store) Resume(ctx context.Context, planID int64, planHash string, userID int) error {
	if strings.TrimSpace(planHash) == "" {
		return fmt.Errorf("changeplan: resume plan %d: %w", planID,
			refuse(CodeHashRequired, "plan_hash is required: resume the plan you were shown"))
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if userID <= 0 {
			return errors.New("no person decided")
		}
		p, err := lockPlan(ctx, tx, planID)
		if err != nil {
			return err
		}
		if p.Status != StatusHalted {
			return fmt.Errorf("%w: plan %d is %s", ErrNotResumable, planID, p.Status)
		}
		if planHash != PlanHash(p) {
			return fmt.Errorf("%w (plan %d)", ErrStaleHash, planID)
		}
		if err := verifyOn(ctx, tx, planID); err != nil {
			return err
		}
		return resumeSteps(ctx, tx, p, userID)
	})
	if err != nil {
		return fmt.Errorf("changeplan: resume plan %d: %w", planID, err)
	}
	return nil
}

// resumeSteps marks every step that halts p resumed, sets p running and
// records the resumed event listing them.
func resumeSteps(ctx context.Context, tx pgx.Tx, p Plan, userID int) error {
	actor := userActor(userID)
	var resumed []int
	for _, st := range p.Steps {
		if haltReason(st) == "" {
			continue
		}
		to := st.Runtime.State
		switch to {
		case StateFailed:
			to = StateSkipped
		case StateHalted:
			to = StateKept
		}
		if err := personMove(ctx, tx, p.ID, st.ID, st.Runtime.State, to,
			"resumed by "+actor+": "+st.Runtime.StateReason, true); err != nil {
			return err
		}
		resumed = append(resumed, st.Ordinal)
	}
	if err := setPlan(ctx, tx, p.ID, StatusRunning, p.PlanHash); err != nil {
		return err
	}
	_, err := AppendEvent(ctx, tx, Event{PlanID: p.ID, Kind: EventResumed,
		PlanHash: p.PlanHash, Actor: actor,
		Detail: map[string]any{"from_status": string(p.Status), "steps": resumed}})
	return err
}

// personMove is a person's compare-and-set on one step: from must still
// be its state, and to a legal move from it (or from itself, which only
// marks it). resumed sets resumed_at. The caller records the event.
func personMove(ctx context.Context, tx pgx.Tx, planID, stepID int64, from, to StepState,
	reason string, resumed bool) error {
	if from != to && !legal(from, to) {
		return fmt.Errorf("%w: step %d of plan %d from %s to %s", ErrIllegalTransition,
			stepID, planID, from, to)
	}
	tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET state = $4,
		state_reason = $5, resumed_at = CASE WHEN $6 THEN now() ELSE resumed_at END
		WHERE id = $1 AND plan_id = $2 AND state = $3`, stepID, planID, string(from),
		string(to), truncate(reason, 1000), resumed)
	if err != nil {
		return fmt.Errorf("move step %d of plan %d to %s: %w", stepID, planID, to, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: step %d of plan %d is not %s", ErrStepMoved, stepID, planID,
			from)
	}
	return nil
}
