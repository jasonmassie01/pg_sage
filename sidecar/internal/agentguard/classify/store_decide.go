package classify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Decide confirms or rejects a proposed classification as actor. A
// non-empty expectHash binds the decision to the classification as it was
// shown (Classification.Hash); a changed one is ErrChanged.
func (s *Store) Decide(ctx context.Context, id int64, confirm bool, actor, note,
	expectHash string) (Classification, error) {
	actor, err := validActor(actor)
	if err != nil {
		return Classification{}, err
	}
	if s.pool == nil {
		return Classification{}, ErrNoStore
	}
	var out Classification
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.Status != StatusProposed {
			return fmt.Errorf("%w: classification %d is %s", ErrInvalidTransition, id,
				cur.Status)
		}
		if expectHash != "" && expectHash != cur.Hash() {
			return ErrChanged
		}
		status := StatusRejected
		if confirm {
			status = StatusConfirmed
		}
		if _, err := tx.Exec(ctx, `/* pg_sage agent_classify v1 */
			UPDATE sage.facts SET status = $2, decided_by = $3, decided_at = now(),
			    decision_note = $4, updated_at = now()
			WHERE id = $1`, id, string(status), actor, cleanNote(note)); err != nil {
			return fmt.Errorf("decide classification %d: %w", id, err)
		}
		out, err = s.get(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return Classification{}, err
	}
	return out, nil
}

// Filter selects classifications to list.
type Filter struct {
	Status []Status
	Limit  int
	// After is the id cursor: list ids greater than it.
	After int64
}

// Page bounds of List (spec §8.1: limit ≤ 200).
const (
	DefaultListLimit = 100
	MaxListLimit     = 200
)

// List returns classifications in id order after the cursor, and the next
// cursor (0 when there are no more).
func (s *Store) List(ctx context.Context, f Filter) ([]Classification, int64, error) {
	statuses := make([]string, 0, len(f.Status))
	for _, st := range f.Status {
		switch st {
		case StatusProposed, StatusConfirmed, StatusRejected, StatusExpired:
			statuses = append(statuses, string(st))
		default:
			return nil, 0, fmt.Errorf("%w: %q", ErrInvalidStatus, st)
		}
	}
	if s.pool == nil {
		return nil, 0, ErrNoStore
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	rows, err := s.pool.Query(ctx, `/* pg_sage agent_classify v1 */ `+
		selectFrom("sage.facts")+` WHERE f.fact_type = 'column_class' AND f.id > $1
		  AND (cardinality($2::text[]) = 0 OR f.status = ANY($2))
		ORDER BY f.id LIMIT $3`, f.After, statuses, limit+1)
	if err != nil {
		return nil, 0, fmt.Errorf("list classifications: %w", err)
	}
	out, err := collect(rows)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}
