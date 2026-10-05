package managedparam

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Decide records an operator's decision on a pending proposal. Approval
// means the operator will apply the change; nothing is applied here. Only
// one decision wins when several race.
func (s *Store) Decide(ctx context.Context, id int64, approve bool, userID int,
	note string) (Record, error) {
	if userID <= 0 {
		return Record{}, errors.New("a managed change decision needs a signed-in user")
	}
	status := StatusRejected
	if approve {
		status = StatusApproved
	}
	if r := []rune(note); len(r) > 1000 {
		note = string(r[:1000])
	}
	rec, err := scanRecord(s.pool.QueryRow(ctx, `UPDATE sage.managed_change_proposals
		SET status = $2, decided_by = $3, decided_at = now(), decision_note = $4,
		    updated_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+recordColumns, id, status, userID, note))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, fmt.Errorf("decide managed change %d: %w", id, err)
	}
	if _, getErr := s.Get(ctx, id); getErr != nil {
		return Record{}, getErr
	}
	return Record{}, fmt.Errorf("%w: %d", ErrNotPending, id)
}

// SupersedeExcept closes every open proposal whose fingerprint is not in
// keep (its reason, the finding, is gone) and returns how many.
func (s *Store) SupersedeExcept(ctx context.Context, keep map[string]bool) (int, error) {
	fingerprints := make([]string, 0, len(keep))
	for fp := range keep {
		fingerprints = append(fingerprints, fp)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sage.managed_change_proposals
		SET status = 'superseded', updated_at = now()
		WHERE status IN ('pending', 'approved') AND NOT (fingerprint = ANY($1))`,
		fingerprints)
	if err != nil {
		return 0, fmt.Errorf("supersede managed changes: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkApplied records that PostgreSQL runs the proposed value: the
// operator (or anyone) applied it.
func (s *Store) MarkApplied(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sage.managed_change_proposals
		SET status = 'applied', applied_at = $2, updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'approved')`, id, at)
	if err != nil {
		return fmt.Errorf("mark managed change %d applied: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrNotPending, id)
	}
	return nil
}
