package candidate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// List is the run's candidates, by generator, then kind, then target.
func (s *Store) List(ctx context.Context, runID int64) ([]Stored, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage review v1 */
		SELECT run_id, candidate_id, generator, dimension, kind, target, action_type,
		       forward_sql, rollback_sql, evidence_ids, prediction, reversible, reason,
		       approval, approval_reason, status, operator
		  FROM sage.review_candidate WHERE run_id = $1
		 ORDER BY generator, kind, target, candidate_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list review candidates of run %d: %w", runID, err)
	}
	out, err := pgx.CollectRows(rows, scanStored)
	if err != nil {
		return nil, fmt.Errorf("read review candidates of run %d: %w", runID, err)
	}
	return out, nil
}

func scanStored(row pgx.CollectableRow) (Stored, error) {
	var st Stored
	var pred, op []byte
	var status string
	c := &st.Candidate
	err := row.Scan(&st.RunID, &c.ID, &c.Generator, &c.Dimension, &c.Kind, &c.Target,
		&c.ActionType, &c.ForwardSQL, &c.RollbackSQL, &c.EvidenceIDs, &pred, &c.Reversible,
		&c.Reason, &c.Approval, &c.ApprovalReason, &status, &op)
	if err != nil {
		return Stored{}, err
	}
	if op != nil {
		c.Operator = &Operator{}
		if err := json.Unmarshal(op, c.Operator); err != nil {
			return Stored{}, fmt.Errorf("decode the operator fields of %s: %w", c.name(), err)
		}
	}
	if err := json.Unmarshal(pred, &c.Prediction); err != nil {
		return Stored{}, fmt.Errorf("decode the prediction of %s: %w", c.name(), err)
	}
	st.Status = Status(status)
	if !knownStatus(st.Status) {
		return Stored{}, fmt.Errorf("candidate %s has unknown status %q", c.name(), status)
	}
	return st, nil
}
