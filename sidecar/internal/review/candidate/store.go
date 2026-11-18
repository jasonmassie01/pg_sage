package candidate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is a (run, candidate) the store does not hold.
var ErrNotFound = errors.New("review candidate not found")

// Stored is a candidate as sage.review_candidate holds it.
type Stored struct {
	Candidate
	RunID  int64
	Status Status
}

// Store keeps a review run's candidates in sage.review_candidate.
type Store struct{ pool *pgxpool.Pool }

// NewStore is a store over the sage schema's pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const insertSQL = `/* pg_sage review v1 */
INSERT INTO sage.review_candidate (run_id, candidate_id, generator, dimension, kind,
    target, action_type, forward_sql, rollback_sql, evidence_ids, prediction, reversible,
    reason, approval, approval_reason, operator)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (run_id, candidate_id) DO NOTHING`

// Save stores the run's candidates as proposed, in one transaction, and
// returns how many were new: a candidate already stored for the run is
// kept as it is. A candidate whose id is not NewID of its parts is
// refused with ErrInvalid before anything is written.
func (s *Store) Save(ctx context.Context, runID int64, cands []Candidate) (int, error) {
	for i := range cands {
		c := &cands[i]
		if c.ID != NewID(c.Generator, c.Kind, c.Target, c.IDBody()) {
			return 0, c.invalid("the id is not the hash of its parts; not stored")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("save review candidates of run %d: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	saved := 0
	for i := range cands {
		c := &cands[i]
		pred, err := json.Marshal(c.Prediction)
		if err != nil {
			return 0, fmt.Errorf("encode the prediction of %s: %w", c.name(), err)
		}
		op, err := operatorJSON(c.Operator)
		if err != nil {
			return 0, fmt.Errorf("encode the operator fields of %s: %w", c.name(), err)
		}
		tag, err := tx.Exec(ctx, insertSQL, runID, c.ID, c.Generator, c.Dimension, c.Kind,
			c.Target, c.ActionType, c.ForwardSQL, c.RollbackSQL, c.EvidenceIDs, pred,
			c.Reversible, c.Reason, c.Approval, c.ApprovalReason, op)
		if err != nil {
			return 0, fmt.Errorf("insert review candidate %s of run %d: %w", c.name(), runID,
				err)
		}
		saved += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("save review candidates of run %d: commit: %w", runID, err)
	}
	return saved, nil
}

// SetStatus moves one candidate along the contract (Transition). The move
// is one conditional UPDATE, so of two concurrent moves from proposed
// exactly one wins; the other gets ErrTransition. An unknown candidate is
// ErrNotFound.
func (s *Store) SetStatus(ctx context.Context, runID int64, id string, to Status) error {
	if err := Transition(StatusProposed, to); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage review v1 */
		UPDATE sage.review_candidate SET status = $3, updated_at = now()
		 WHERE run_id = $1 AND candidate_id = $2 AND status = 'proposed'`, runID, id,
		string(to))
	if err != nil {
		return fmt.Errorf("move review candidate %s of run %d to %s: %w", id, runID, to, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var cur string
	err = s.pool.QueryRow(ctx, `SELECT status FROM sage.review_candidate
		WHERE run_id = $1 AND candidate_id = $2`, runID, id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: run %d candidate %s", ErrNotFound, runID, id)
	}
	if err != nil {
		return fmt.Errorf("read review candidate %s of run %d: %w", id, runID, err)
	}
	return Transition(Status(cur), to)
}

// operatorJSON is the operator column: NULL for a candidate without one.
func operatorJSON(op *Operator) ([]byte, error) {
	if op == nil {
		return nil, nil
	}
	return json.Marshal(op)
}
