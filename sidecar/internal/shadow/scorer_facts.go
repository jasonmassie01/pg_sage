package shadow

import (
	"context"
	"fmt"
	"time"
)

// factBatch bounds the operator decisions and actions one pass reads
// (each pass starts at its oldest pending decision).
const factBatch = 2000

// queueRow is an approval item an operator decided, with the action it
// produced.
type queueRow struct {
	queueFact
	shape     string
	decidedAt time.Time
}

// actionRow is an action pg_sage ran (for itself or an operator).
type actionRow struct {
	actionFact
	shape      string
	executedAt time.Time
}

// The verdict of an action is one primary-key read of sage.action_outcome
// per row returned (a scalar subquery, evaluated after the LIMIT), never
// a join the planner may answer by hashing the whole outcome table: it
// did for a window of a few days (perf gate, 2026-10-04).
const queueFactsSQL = `/* pg_sage */ SELECT q.id, q.proposed_sql, q.status, q.decided_at,
	COALESCE(q.action_log_id, 0),
	COALESCE((SELECT o.verdict FROM sage.action_outcome o
	          WHERE o.action_log_id = q.action_log_id), ''),
	COALESCE(l.outcome, '')
	FROM sage.action_queue q
	LEFT JOIN sage.action_log l ON l.id = q.action_log_id
	WHERE q.decided_at >= $1 AND q.status IN ('approved', 'executed', 'rejected')
	ORDER BY q.decided_at, q.id LIMIT $2`

// queueFacts are the operator decisions since the oldest pending decision.
func (s *Scorer) queueFacts(ctx context.Context, since time.Time) ([]queueRow, error) {
	rows, err := s.pool.Query(ctx, queueFactsSQL, since, factBatch)
	if err != nil {
		return nil, fmt.Errorf("shadow: read operator decisions: %w", err)
	}
	defer rows.Close()
	var out []queueRow
	for rows.Next() {
		var r queueRow
		var sql string
		if err := rows.Scan(&r.ID, &sql, &r.Status, &r.decidedAt, &r.ActionLogID, &r.Verdict,
			&r.Lifecycle); err != nil {
			return nil, fmt.Errorf("shadow: scan operator decision: %w", err)
		}
		r.shape = Shape(sql)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: read operator decisions: %w", err)
	}
	return out, nil
}

const actionFactsSQL = `/* pg_sage */ SELECT l.id, l.sql_executed, l.executed_at, l.outcome,
	COALESCE((SELECT o.verdict FROM sage.action_outcome o WHERE o.action_log_id = l.id), '')
	FROM sage.action_log l
	WHERE l.executed_at >= $1 AND l.outcome <> 'failed'
	ORDER BY l.executed_at, l.id LIMIT $2`

// actionFacts are the actions run since the oldest pending decision
// (failed and refused ones changed nothing).
func (s *Scorer) actionFacts(ctx context.Context, since time.Time) ([]actionRow, error) {
	rows, err := s.pool.Query(ctx, actionFactsSQL, since, factBatch)
	if err != nil {
		return nil, fmt.Errorf("shadow: read actions: %w", err)
	}
	defer rows.Close()
	var out []actionRow
	for rows.Next() {
		var r actionRow
		var sql string
		if err := rows.Scan(&r.ID, &sql, &r.executedAt, &r.Lifecycle, &r.Verdict); err != nil {
			return nil, fmt.Errorf("shadow: scan action: %w", err)
		}
		r.shape = Shape(sql)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: read actions: %w", err)
	}
	return out, nil
}

// matchQueue is the first decision an operator made on the same change
// after pg_sage recorded it.
func matchQueue(rows []queueRow, d Decision) *queueFact {
	for i := range rows {
		if rows[i].shape == d.Shape && !rows[i].decidedAt.Before(d.RecordedAt) {
			f := rows[i].queueFact
			return &f
		}
	}
	return nil
}

// matchAction is the first time the same change ran after pg_sage
// recorded it.
func matchAction(rows []actionRow, d Decision) *actionFact {
	for i := range rows {
		if rows[i].shape == d.Shape && !rows[i].executedAt.Before(d.RecordedAt) {
			f := rows[i].actionFact
			return &f
		}
	}
	return nil
}
