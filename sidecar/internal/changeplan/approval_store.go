package changeplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The store's side of the approval modes (plan-03a). Every decision locks
// the plan row, so it serializes with edits and the engine's tick: an
// approval is never recorded on a hash the rows no longer have.

// ErrStepNotApprovable refuses a step approval of a step that has started
// or ended.
var ErrStepNotApprovable = errors.New("changeplan: the step cannot be approved in its state")

// ApproveWith records userID's decision d on plan planID. d.PlanHash must
// be the hash of the rows now (else ErrStaleHash); up_to past the last
// step is refused and up_to the last step is stored as all. The same
// decision again returns the live approval, so concurrent approvers make
// one; another mode at the hash supersedes it.
func (s *Store) ApproveWith(ctx context.Context, planID int64, d Decision,
	userID int) (Approval, error) {
	if userID <= 0 {
		return Approval{}, fmt.Errorf("changeplan: approve plan %d: no approver", planID)
	}
	if err := d.Check(); err != nil {
		return Approval{}, fmt.Errorf("changeplan: approve plan %d: %w", planID, err)
	}
	var a Approval
	var fresh bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPlan(ctx, tx, planID)
		if err != nil {
			return err
		}
		if (p.Status != StatusProposed && p.Status != StatusApproved) ||
			(d.ProposedOnly && p.Status != StatusProposed) {
			return fmt.Errorf("%w: plan %d is %s", ErrNotApprovable, planID, p.Status)
		}
		current := PlanHash(p)
		if d.PlanHash != current {
			return fmt.Errorf("%w (plan %d)", ErrStaleHash, planID)
		}
		if d, err = d.forPlan(len(p.Steps)); err != nil {
			return err
		}
		if err := checkCapsIn(ctx, tx, s.capsSource(), p); err != nil {
			return err
		}
		a, fresh, err = recordApproval(ctx, tx, p, d, userID)
		return err
	})
	if err != nil {
		return Approval{}, fmt.Errorf("changeplan: approve plan %d: %w", planID, err)
	}
	if fresh {
		countApproval(a.Mode)
	}
	return a, nil
}

// recordApproval stores d on locked plan p, sets it approved and, for a
// new approval (fresh), appends the approved event.
func recordApproval(ctx context.Context, tx pgx.Tx, p Plan, d Decision,
	userID int) (Approval, bool, error) {
	a, fresh, err := approveLocked(ctx, tx, p.ID, d, userID)
	if err != nil {
		return Approval{}, false, err
	}
	if err := setPlan(ctx, tx, p.ID, StatusApproved, d.PlanHash); err != nil {
		return Approval{}, false, err
	}
	if !fresh {
		return a, false, nil // the live approval already holds this decision
	}
	detail := map[string]any{"approval_id": a.ID, "mode": string(d.Mode),
		"via": string(d.Via)}
	if d.Mode == ModeUpTo {
		detail["up_to"] = d.UpTo
	}
	_, err = AppendEvent(ctx, tx, Event{PlanID: p.ID, Kind: EventApproved,
		PlanHash: d.PlanHash, Actor: userActor(userID), Detail: detail})
	return a, err == nil, err
}

func userActor(userID int) string { return fmt.Sprintf("user:%d", userID) }

// ApproveStep records userID's approval of step stepID of plan planID at
// planHash, the hash they were shown. The plan must be proposed, approved
// or running, the step one of its action steps not yet started. A step
// awaiting its approval goes back to pending, so the engine starts it;
// approving it again returns the live step approval.
func (s *Store) ApproveStep(ctx context.Context, planID, stepID int64, planHash string,
	userID int, via Via) (StepApproval, error) {
	var sa StepApproval
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, step, err := lockStepForApproval(ctx, tx, planID, stepID, planHash, userID, via)
		if err != nil {
			return err
		}
		live, err := liveStepApproval(ctx, tx, stepID)
		if err != nil || (live != nil && live.PlanHash == planHash) {
			if live != nil {
				sa = *live
			}
			return err
		}
		if sa, err = insertStepApproval(ctx, tx, p, step, userID, via, live != nil); err != nil {
			return err
		}
		return releaseAwaitingStep(ctx, tx, p, step, sa)
	})
	if err != nil {
		return StepApproval{}, fmt.Errorf("changeplan: approve step %d of plan %d: %w",
			stepID, planID, err)
	}
	return sa, nil
}

// lockStepForApproval locks the plan and checks a step approval's inputs
// against it.
func lockStepForApproval(ctx context.Context, tx pgx.Tx, planID, stepID int64,
	planHash string, userID int, via Via) (Plan, Step, error) {
	switch {
	case userID <= 0:
		return Plan{}, Step{}, errors.New("no approver")
	case planHash == "":
		return Plan{}, Step{}, refuse(CodeHashRequired, "plan_hash is required")
	}
	if err := checkVia(via); err != nil {
		return Plan{}, Step{}, err
	}
	p, err := lockPlan(ctx, tx, planID)
	if err != nil {
		return Plan{}, Step{}, err
	}
	if p.Status != StatusProposed && p.Status != StatusApproved && p.Status != StatusRunning {
		return Plan{}, Step{}, fmt.Errorf("%w: plan %d is %s", ErrNotApprovable, planID,
			p.Status)
	}
	if planHash != PlanHash(p) {
		return Plan{}, Step{}, fmt.Errorf("%w (plan %d)", ErrStaleHash, planID)
	}
	for _, st := range p.Steps {
		if st.ID != stepID {
			continue
		}
		if st.Kind != KindAction {
			return Plan{}, Step{}, refuse(CodeNotAnAction, "step %d is a %s step: it "+
				"changes nothing and needs no approval", st.Ordinal, st.Kind)
		}
		if st.Runtime.State != StatePending && st.Runtime.State != StateAwaitingApproval {
			return Plan{}, Step{}, fmt.Errorf("%w: step %d is %s", ErrStepNotApprovable,
				st.Ordinal, st.Runtime.State)
		}
		return p, st, nil
	}
	return Plan{}, Step{}, refuse(CodeStepNotInPlan, "plan %d has no step %d", planID, stepID)
}

const stepApprovalColumns = `a.id, a.plan_id, a.step_id, st.ordinal, a.plan_hash,
	a.approver_user_id, a.via, a.decided_at, a.voided_at, COALESCE(a.void_reason, '')`

func scanStepApproval(row pgx.Row) (StepApproval, error) {
	var sa StepApproval
	err := row.Scan(&sa.ID, &sa.PlanID, &sa.StepID, &sa.Ordinal, &sa.PlanHash,
		&sa.ApproverUserID, &sa.Via, &sa.DecidedAt, &sa.VoidedAt, &sa.VoidReason)
	return sa, err
}

// insertStepApproval voids the step's live approval at another hash
// (superseded, when there is one) and stores userID's.
func insertStepApproval(ctx context.Context, tx pgx.Tx, p Plan, st Step, userID int,
	via Via, supersede bool) (StepApproval, error) {
	if supersede {
		if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step_approval
			SET voided_at = now(), void_reason = 'superseded'
			WHERE step_id = $1 AND voided_at IS NULL`, st.ID); err != nil {
			return StepApproval{}, fmt.Errorf("void the step's old approval: %w", err)
		}
	}
	sa, err := scanStepApproval(tx.QueryRow(ctx, `/* pg_sage */ WITH a AS (
		INSERT INTO sage.plan_step_approval (plan_id, step_id, plan_hash, approver_user_id,
		via) VALUES ($1, $2, $3, $4, $5) RETURNING *)
		SELECT `+stepApprovalColumns+` FROM a JOIN sage.plan_step st ON st.id = a.step_id`,
		p.ID, st.ID, p.PlanHash, userID, string(via)))
	if err != nil {
		return StepApproval{}, fmt.Errorf("store the step approval: %w", err)
	}
	return sa, nil
}

// releaseAwaitingStep moves a step awaiting its approval back to pending
// and records the approved event naming the step.
func releaseAwaitingStep(ctx context.Context, tx pgx.Tx, p Plan, st Step,
	sa StepApproval) error {
	detail := map[string]any{"step_approval_id": sa.ID, "ordinal": st.Ordinal,
		"via": string(sa.Via), "from": string(st.Runtime.State)}
	if st.Runtime.State == StateAwaitingApproval {
		err := personMove(ctx, tx, p.ID, st.ID, StateAwaitingApproval, StatePending,
			"approved by "+userActor(sa.ApproverUserID), false)
		if err != nil {
			return err
		}
		detail["to"] = string(StatePending)
	}
	_, err := AppendEvent(ctx, tx, Event{PlanID: p.ID, StepID: &st.ID, Kind: EventApproved,
		PlanHash: p.PlanHash, Actor: userActor(sa.ApproverUserID), Detail: detail})
	return err
}

// liveApproval is the plan's live approval, nil without one.
func liveApproval(ctx context.Context, q querier, planID int64) (*Approval, error) {
	a, err := scanApproval(q.QueryRow(ctx, `/* pg_sage */ SELECT `+approvalColumns+`
		FROM sage.plan_approval WHERE plan_id = $1 AND voided_at IS NULL`, planID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the live approval: %w", err)
	}
	return &a, nil
}

// liveStepApproval is the step's live approval, nil without one.
func liveStepApproval(ctx context.Context, q querier, stepID int64) (*StepApproval, error) {
	sa, err := scanStepApproval(q.QueryRow(ctx, `/* pg_sage */ SELECT `+stepApprovalColumns+`
		FROM sage.plan_step_approval a JOIN sage.plan_step st ON st.id = a.step_id
		WHERE a.step_id = $1 AND a.voided_at IS NULL`, stepID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the step's live approval: %w", err)
	}
	return &sa, nil
}

func stepApprovalsOf(ctx context.Context, q querier, planID int64) ([]StepApproval, error) {
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT `+stepApprovalColumns+`
		FROM sage.plan_step_approval a JOIN sage.plan_step st ON st.id = a.step_id
		WHERE a.plan_id = $1 ORDER BY a.id`, planID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StepApproval, error) {
		return scanStepApproval(r)
	})
}

// StepApprovals returns every step approval of plan id, oldest first.
func (s *Store) StepApprovals(ctx context.Context, id int64) ([]StepApproval, error) {
	out, err := stepApprovalsOf(ctx, s.pool, id)
	if err != nil {
		return nil, fmt.Errorf("changeplan: step approvals of plan %d: %w", id, err)
	}
	return out, nil
}

// voidStepApprovals voids every live step approval of the plan, returning
// how many it voided.
func voidStepApprovals(ctx context.Context, tx pgx.Tx, planID int64,
	reason string) (int64, error) {
	tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step_approval
		SET voided_at = now(), void_reason = $2 WHERE plan_id = $1 AND voided_at IS NULL`,
		planID, reason)
	if err != nil {
		return 0, fmt.Errorf("void the step approvals: %w", err)
	}
	return tag.RowsAffected(), nil
}

// voidAll voids the plan's live approval and every live step approval.
func voidAll(ctx context.Context, tx pgx.Tx, planID int64, reason string) error {
	if _, err := voidLive(ctx, tx, planID, reason); err != nil {
		return err
	}
	_, err := voidStepApprovals(ctx, tx, planID, reason)
	return err
}

// Detail is a plan as a person reviews it: its steps and edges, every
// approval and step approval, and each step's coverage now.
type Detail struct {
	Plan          Plan           `json:"plan"`
	Approvals     []Approval     `json:"approvals"`
	StepApprovals []StepApproval `json:"step_approvals"`
	Coverage      []StepCoverage `json:"coverage"`
}

// Detail reads plan id's detail in one read-only snapshot.
func (s *Store) Detail(ctx context.Context, id int64) (Detail, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return Detail{}, fmt.Errorf("changeplan: read plan %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	d, err := readDetail(ctx, tx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("changeplan: read plan %d: %w", id, err)
	}
	return d, nil
}

func readDetail(ctx context.Context, q querier, id int64) (Detail, error) {
	p, err := loadPlan(ctx, q, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Plan: p}
	if d.Approvals, err = approvalsOf(ctx, q, id); err != nil {
		return Detail{}, fmt.Errorf("read the approvals: %w", err)
	}
	if d.StepApprovals, err = stepApprovalsOf(ctx, q, id); err != nil {
		return Detail{}, fmt.Errorf("read the step approvals: %w", err)
	}
	settings, err := readSettingContexts(ctx, q, p.Steps)
	if err != nil {
		return Detail{}, err
	}
	var live *Approval
	for i := range d.Approvals {
		if d.Approvals[i].VoidedAt == nil {
			live = &d.Approvals[i]
		}
	}
	d.Coverage = Coverage(p, live, d.StepApprovals, settings)
	return d, nil
}
