package recommendation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// move is one compare-and-set on (id, state, revision), plus the attempt
// when attempt > 0 so a stale claim cannot write over a newer attempt.
type move struct {
	id          int64
	from, to    State
	revision    int
	attempt     int
	actor       string
	reason      string
	actionLogID *int64
}

// applyMove performs m and records it in the history. set is a constant
// SQL fragment (never input) whose placeholders start at $6; args fill them.
func applyMove(ctx context.Context, tx pgx.Tx, m move, set string, args ...any) error {
	if !CanTransition(m.from, m.to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, m.from, m.to)
	}
	sql := `/* pg_sage */ UPDATE sage.recommendation
		SET state = $4, updated_at = now()` + set + `
		WHERE id = $1 AND state = $2 AND revision = $3
		  AND ($5 = 0 OR attempt_count = $5)`
	params := append([]any{m.id, string(m.from), m.revision, string(m.to), m.attempt},
		args...)
	tag, err := tx.Exec(ctx, sql, params...)
	if err != nil {
		return fmt.Errorf("move recommendation %d %s → %s: %w", m.id, m.from, m.to, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: recommendation %d is no longer %s at revision %d",
			ErrConflict, m.id, m.from, m.revision)
	}
	return insertTransition(ctx, tx, m.id, m.from, m.to, m.revision, m.actor, m.reason,
		m.actionLogID)
}

// approvalSet pins the approval to the current revision and content.
const approvalSet = `, approved_revision = revision, approved_hash = content_hash,
	approved_by = $6, approved_at = now()`

// Transition moves id from → to at revision as a compare-and-set. It is
// the generic edge; Approve, Claim and the Record* calls add the facts
// their edges carry.
func (s *Store) Transition(
	ctx context.Context, id int64, from, to State, revision int, actor, reason string,
) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	m := move{id: id, from: from, to: to, revision: revision, actor: actor, reason: reason}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		switch to {
		case StateApproved:
			return applyMove(ctx, tx, m, approvalSet, actor)
		case StateApplying:
			return applyMove(ctx, tx, m, `, lease_until = now() + interval '1 hour'`)
		default:
			return applyMove(ctx, tx, m, `, lease_until = NULL`)
		}
	})
}

// Approve approves the revision whose content hash is hash (C04: an
// approval pins exact content; a revised recommendation needs a new one).
func (s *Store) Approve(ctx context.Context, id int64, hash, actor string) (Recommendation, error) {
	var rec Recommendation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		rec, err = ApproveTx(ctx, tx, id, hash, actor)
		return err
	})
	return rec, err
}

// ApproveTx is Approve inside the caller's transaction, so an approval
// surface (the action queue) and the recommendation commit together.
func ApproveTx(
	ctx context.Context, tx pgx.Tx, id int64, hash, actor string,
) (Recommendation, error) {
	return approveTx(ctx, tx, id, hash, actor, "")
}

// approveTx approves with reason ("" records the approved revision).
func approveTx(
	ctx context.Context, tx pgx.Tx, id int64, hash, actor, reason string,
) (Recommendation, error) {
	rec, err := lockHead(ctx, tx, id)
	if err != nil {
		return Recommendation{}, err
	}
	if rec.ContentHash != hash {
		return Recommendation{}, fmt.Errorf("%w: recommendation %d is at revision %d",
			ErrRevised, id, rec.Revision)
	}
	if rec.State == StateApproved && rec.ApprovedHash == hash {
		return rec, nil
	}
	if rec.State != StateProposed {
		return Recommendation{}, fmt.Errorf("%w: recommendation %d is %s",
			ErrConflict, id, rec.State)
	}
	if reason == "" {
		reason = fmt.Sprintf("approved revision %d", rec.Revision)
	}
	m := move{id: id, from: StateProposed, to: StateApproved, revision: rec.Revision,
		actor: actor, reason: reason}
	if err := applyMove(ctx, tx, m, approvalSet, actor); err != nil {
		return Recommendation{}, err
	}
	return getTx(ctx, tx, id)
}

// lockHead reads a head FOR UPDATE.
func lockHead(ctx context.Context, tx pgx.Tx, id int64) (Recommendation, error) {
	rec, err := scanHead(tx.QueryRow(ctx, `/* pg_sage */ SELECT `+headColumns+`
		FROM sage.recommendation r WHERE r.id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Recommendation{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	return rec, err
}

// Claim moves a recommendation into applying: the durable ownership of
// one apply attempt. Exactly one of any number of racing claims wins.
func (s *Store) Claim(ctx context.Context, req ClaimRequest) (Claim, error) {
	var c Claim
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rec, err := lockHead(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		if err := claimable(rec, req); err != nil {
			return err
		}
		from := rec.State
		if from == StateProposed {
			m := move{id: rec.ID, from: from, to: StateApproved, revision: rec.Revision,
				actor: req.ApproveAs, reason: approveReason(req, rec.Revision)}
			if err := applyMove(ctx, tx, m, approvalSet, req.ApproveAs); err != nil {
				return err
			}
			from = StateApproved
		}
		c = Claim{ID: rec.ID, Revision: rec.Revision, ContentHash: rec.ContentHash,
			Attempt: rec.AttemptCount + 1}
		m := move{id: rec.ID, from: from, to: StateApplying, revision: rec.Revision,
			actor: claimActor(req), reason: fmt.Sprintf("apply attempt %d", c.Attempt)}
		return applyMove(ctx, tx, m, `, attempt_count = attempt_count + 1,
			lease_until = now() + make_interval(secs => $6), next_attempt_at = NULL,
			reason = NULL`, req.Lease.Seconds())
	})
	return c, err
}

// claimable checks a locked head against a claim request.
func claimable(rec Recommendation, req ClaimRequest) error {
	if rec.Revision != req.Revision {
		return fmt.Errorf("%w: recommendation %d is at revision %d, not %d",
			ErrConflict, rec.ID, rec.Revision, req.Revision)
	}
	switch rec.State {
	case StateApproved:
		return nil
	case StateProposed:
		if req.ApproveAs != "" {
			return nil
		}
		return fmt.Errorf("%w: recommendation %d is not approved", ErrConflict, rec.ID)
	case StateFailed:
		// An operator's explicit retry overrides the backoff; nothing else does.
		if isOperator(req.ApproveAs) || rec.due {
			return nil
		}
		return fmt.Errorf("%w: recommendation %d is backing off until %s",
			ErrConflict, rec.ID, rec.NextAttemptAt.Format("15:04:05"))
	default:
		return fmt.Errorf("%w: recommendation %d is %s", ErrConflict, rec.ID, rec.State)
	}
}

func isOperator(actor string) bool { return strings.HasPrefix(actor, "user:") }

func approveReason(req ClaimRequest, revision int) string {
	if req.Reason != "" {
		return req.Reason
	}
	return fmt.Sprintf("approved revision %d", revision)
}

func claimActor(req ClaimRequest) string {
	if req.ApproveAs != "" {
		return req.ApproveAs
	}
	return ActorExecutor
}
