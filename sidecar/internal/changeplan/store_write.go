package changeplan

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// editable statuses: a plan not yet running and not ended. A running or
// halted plan is changed only by the engine.
func editable(s Status) bool {
	return s == StatusDraft || s == StatusProposed || s == StatusApproved
}

// lockPlan locks plan id's row for this transaction and returns it with
// its steps and edges.
func lockPlan(ctx context.Context, tx pgx.Tx, id int64) (Plan, error) {
	var one int
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT 1 FROM sage.plan WHERE id = $1
		FOR UPDATE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: plan %d", ErrNotFound, id)
	}
	if err != nil {
		return Plan{}, err
	}
	return loadPlan(ctx, tx, id)
}

// UpdateStep replaces the hashed fields of the step with step.Ordinal
// (its id and runtime state are kept), validates the edited plan, voids a
// live approval ("edited") and sets an approved plan back to proposed, in
// one transaction. It returns the stored plan.
func (s *Store) UpdateStep(ctx context.Context, planID int64, step Step,
	r Resolver) (Plan, error) {
	err := s.edit(ctx, planID, r, func(p *Plan) (planEdit, error) {
		idx := -1
		for i := range p.Steps {
			if p.Steps[i].Ordinal == step.Ordinal {
				idx = i
			}
		}
		if idx < 0 {
			return planEdit{}, fmt.Errorf("%w: plan %d has no step %d", ErrNotFound,
				planID, step.Ordinal)
		}
		edited := step.WithDefaults()
		edited.ID, edited.Runtime = p.Steps[idx].ID, p.Steps[idx].Runtime
		p.Steps[idx] = edited
		return planEdit{stepID: &edited.ID, detail: map[string]any{"ordinal": step.Ordinal},
			write: func(tx pgx.Tx) error { return updateStepRow(ctx, tx, edited) }}, nil
	})
	if err != nil {
		return Plan{}, fmt.Errorf("changeplan: update step %d of plan %d: %w", step.Ordinal,
			planID, err)
	}
	return s.Get(ctx, planID)
}

// SetEdges replaces the plan's edges, with UpdateStep's voiding rule.
func (s *Store) SetEdges(ctx context.Context, planID int64, edges []Edge,
	r Resolver) (Plan, error) {
	err := s.edit(ctx, planID, r, func(p *Plan) (planEdit, error) {
		p.Edges = edges
		write := func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.plan_step_edge
				WHERE plan_id = $1`, planID); err != nil {
				return fmt.Errorf("clear the edges: %w", err)
			}
			return insertEdges(ctx, tx, planID, edges)
		}
		return planEdit{detail: map[string]any{"edges": len(edges)}, write: write}, nil
	})
	if err != nil {
		return Plan{}, fmt.Errorf("changeplan: set the edges of plan %d: %w", planID, err)
	}
	return s.Get(ctx, planID)
}

// planEdit is one change edit writes: its rows, and its event's step and
// detail.
type planEdit struct {
	write  func(pgx.Tx) error
	stepID *int64
	detail map[string]any
}

// edit locks the plan, applies change to its in-memory copy, validates
// the result and then writes it, voids a live approval, stores the new
// hash and records the edited event under ctx's actor (WithActor).
func (s *Store) edit(ctx context.Context, planID int64, r Resolver,
	change func(*Plan) (planEdit, error)) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPlan(ctx, tx, planID)
		if err != nil {
			return err
		}
		if !editable(p.Status) {
			return fmt.Errorf("%w: plan %d is %s", ErrNotEditable, planID, p.Status)
		}
		e, err := change(&p)
		if err != nil {
			return err
		}
		if err := checkResult(Validate(ctx, p, r)); err != nil {
			return err
		}
		if err := e.write(tx); err != nil {
			return err
		}
		voided, err := voidLive(ctx, tx, planID, VoidEdited)
		if err != nil {
			return err
		}
		status := p.Status
		if status == StatusApproved {
			status = StatusProposed
		}
		hash := PlanHash(p)
		if err := setPlan(ctx, tx, planID, status, hash); err != nil {
			return err
		}
		e.detail["voided_approval"], e.detail["status"] = voided, string(status)
		_, err = AppendEvent(ctx, tx, Event{PlanID: planID, StepID: e.stepID,
			Kind: EventEdited, PlanHash: hash, Actor: actorOf(ctx), Detail: e.detail})
		return err
	})
}

func updateStepRow(ctx context.Context, tx pgx.Tx, s Step) error {
	args, err := stepArgs(s)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET kind = $3,
		action_type = $4, setting = $5, proposed_sql = $6, rollback_sql = $7,
		finding_id = $8, evidence_ids = $9, target_queryids = $10, metric = $11,
		expected_direction = $12, expected_pct = $13, expected_source = $14,
		guardrails = $15, min_evidence = $16, deadline_minutes = $17, checkpoint = $18,
		on_improved = $19, on_no_change = $20, on_regressed = $21, on_inconclusive = $22,
		on_unverifiable = NULLIF($23, ''), on_expired = $24
		WHERE id = $1 AND ordinal = $2`, append([]any{s.ID}, args...)...)
	if err != nil {
		return fmt.Errorf("store step %d: %w", s.Ordinal, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: step %d changed under the lock", ErrNotFound, s.Ordinal)
	}
	return nil
}

// voidLive voids the plan's live approval, reporting whether there was one.
func voidLive(ctx context.Context, tx pgx.Tx, planID int64, reason string) (bool, error) {
	tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_approval
		SET voided_at = now(), void_reason = $2 WHERE plan_id = $1 AND voided_at IS NULL`,
		planID, reason)
	if err != nil {
		return false, fmt.Errorf("void the live approval: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func setPlan(ctx context.Context, tx pgx.Tx, planID int64, status Status, hash string) error {
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan SET status = $2, plan_hash = $3,
		updated_at = now() WHERE id = $1`, planID, string(status), hash)
	if err != nil {
		return fmt.Errorf("store the plan status: %w", err)
	}
	return nil
}

// Approve records userID's approval of plan planID at planHash, the hash
// the approver was shown. It refuses with ErrStaleHash when the rows hash
// to something else; approving again at the live approval's hash returns
// that approval, so a plan holds at most one live approval.
func (s *Store) Approve(ctx context.Context, planID int64, planHash string,
	userID int) (Approval, error) {
	if userID <= 0 {
		return Approval{}, fmt.Errorf("changeplan: approve plan %d: no approver", planID)
	}
	var a Approval
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPlan(ctx, tx, planID)
		if err != nil {
			return err
		}
		if p.Status != StatusProposed && p.Status != StatusApproved {
			return fmt.Errorf("%w: plan %d is %s", ErrNotApprovable, planID, p.Status)
		}
		current := PlanHash(p)
		if planHash != current {
			return fmt.Errorf("%w (plan %d)", ErrStaleHash, planID)
		}
		var fresh bool
		a, fresh, err = approveLocked(ctx, tx, planID, current, userID)
		if err != nil {
			return err
		}
		if err := setPlan(ctx, tx, planID, StatusApproved, current); err != nil {
			return err
		}
		if !fresh {
			return nil // the live approval already holds this hash: nothing changed
		}
		_, err = AppendEvent(ctx, tx, Event{PlanID: planID, Kind: EventApproved,
			PlanHash: current, Actor: fmt.Sprintf("user:%d", userID),
			Detail: map[string]any{"approval_id": a.ID}})
		return err
	})
	if err != nil {
		return Approval{}, fmt.Errorf("changeplan: approve plan %d: %w", planID, err)
	}
	return a, nil
}

// approveLocked returns the live approval when it holds hash, else voids
// any other live approval ("superseded") and inserts a new one (fresh).
func approveLocked(ctx context.Context, tx pgx.Tx, planID int64, hash string,
	userID int) (a Approval, fresh bool, err error) {
	err = tx.QueryRow(ctx, `/* pg_sage */ SELECT id, plan_id, plan_hash, approver_user_id,
		decided_at FROM sage.plan_approval WHERE plan_id = $1 AND voided_at IS NULL`,
		planID).Scan(&a.ID, &a.PlanID, &a.PlanHash, &a.ApproverUserID, &a.DecidedAt)
	switch {
	case err == nil && a.PlanHash == hash:
		return a, false, nil
	case err == nil:
		if _, err := voidLive(ctx, tx, planID, "superseded"); err != nil {
			return Approval{}, false, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return Approval{}, false, fmt.Errorf("read the live approval: %w", err)
	}
	a = Approval{PlanID: planID, PlanHash: hash, ApproverUserID: userID}
	err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.plan_approval (plan_id, plan_hash,
		approver_user_id) VALUES ($1, $2, $3) RETURNING id, decided_at`,
		planID, hash, userID).Scan(&a.ID, &a.DecidedAt)
	if err != nil {
		return Approval{}, false, fmt.Errorf("store the approval: %w", err)
	}
	return a, true, nil
}

// Void ends a plan that has not started or has halted: its live approval
// is voided with reason, the plan becomes voided (terminal) and the voided
// event names ctx's actor (WithActor).
func (s *Store) Void(ctx context.Context, planID int64, reason string) error {
	if n := utf8.RuneCountInString(reason); n < 1 || n > maxRefRunes {
		return fmt.Errorf("changeplan: void plan %d: the reason is 1 to %d characters",
			planID, maxRefRunes)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPlan(ctx, tx, planID)
		if err != nil {
			return err
		}
		if !editable(p.Status) && p.Status != StatusHalted {
			return fmt.Errorf("%w: plan %d is %s", ErrNotEditable, planID, p.Status)
		}
		if _, err := voidLive(ctx, tx, planID, reason); err != nil {
			return err
		}
		if err := setPlan(ctx, tx, planID, StatusVoided, p.PlanHash); err != nil {
			return err
		}
		_, err = AppendEvent(ctx, tx, Event{PlanID: planID, Kind: EventVoided,
			PlanHash: p.PlanHash, Actor: actorOf(ctx),
			Detail: map[string]any{"reason": reason, "from_status": string(p.Status)}})
		return err
	})
	if err != nil {
		return fmt.Errorf("changeplan: void plan %d: %w", planID, err)
	}
	return nil
}

// Verify recomputes plan planID's hash from its stored rows and returns
// ErrHashMismatch when it differs from the live approval's hash, and
// ErrNotApproved when there is no live approval. The engine runs it
// before every step (plan-02a), so a row edited by direct SQL voids the
// plan.
func (s *Store) Verify(ctx context.Context, planID int64) error {
	return verifyOn(ctx, s.pool, planID)
}

func verifyOn(ctx context.Context, q querier, planID int64) error {
	p, err := loadPlan(ctx, q, planID)
	if err != nil {
		return fmt.Errorf("changeplan: verify plan %d: %w", planID, err)
	}
	var approved string
	err = q.QueryRow(ctx, `/* pg_sage */ SELECT plan_hash FROM sage.plan_approval
		WHERE plan_id = $1 AND voided_at IS NULL`, planID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: plan %d", ErrNotApproved, planID)
	}
	if err != nil {
		return fmt.Errorf("changeplan: verify plan %d: read the approval: %w", planID, err)
	}
	if got := PlanHash(p); got != approved {
		return fmt.Errorf("%w: plan %d hashes to %s, approved %s", ErrHashMismatch, planID,
			got, approved)
	}
	return nil
}
