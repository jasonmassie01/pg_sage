package changeplan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resumeReader reads what may have happened to a step left in flight.
type resumeReader interface {
	evidence(ctx context.Context, p Plan, s Step) (resumeEvidence, error)
}

// dbResume reads it from sage.action_queue, sage.plan_event and
// sage.action_log, bounded by the store's timeouts.
type dbResume struct{ pool *pgxpool.Pool }

// maxLedgerCandidates bounds the ledger rows read per step: two already
// make the step's run ambiguous.
const maxLedgerCandidates = 3

func (r dbResume) evidence(ctx context.Context, p Plan, s Step) (resumeEvidence, error) {
	var ev resumeEvidence
	err := (&Store{pool: r.pool}).inTx(ctx, func(tx pgx.Tx) error {
		if err := readQueueEvidence(ctx, tx, p, s, &ev); err != nil {
			return err
		}
		floor, ok, err := readLedgerFloor(ctx, tx, p.ID, s.ID)
		if err != nil || !ok {
			return err
		}
		ev.floorKnown = true
		return readLedgerEvidence(ctx, tx, floor, s.ProposedSQL, &ev)
	})
	if err != nil {
		return resumeEvidence{}, fmt.Errorf("read what happened to step %d of plan %d: %w",
			s.Ordinal, p.ID, err)
	}
	return ev, nil
}

// readQueueEvidence reads the queue items carrying step s's identity: the
// one the step recorded, else the newest.
func readQueueEvidence(ctx context.Context, tx pgx.Tx, p Plan, s Step,
	ev *resumeEvidence) error {
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT id, status FROM sage.action_queue
		WHERE identity_key = $1 ORDER BY id DESC`, PlanStepKey(p.ID, s.ID))
	if err != nil {
		return fmt.Errorf("read its queue items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			return fmt.Errorf("read its queue items: %w", err)
		}
		ev.queueItems++
		mine := s.Runtime.QueueID != nil && *s.Runtime.QueueID == id
		if ev.queueID == 0 && s.Runtime.QueueID == nil || mine {
			ev.queueID, ev.queueStatus = id, status
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read its queue items: %w", err)
	}
	return nil
}

// readLedgerFloor is the ledger floor step stepID's latest start recorded
// (false: none, a step started before plan-02e).
func readLedgerFloor(ctx context.Context, tx pgx.Tx, planID, stepID int64) (int64, bool,
	error) {
	var floor int64
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT (detail->>$3)::bigint
		FROM sage.plan_event WHERE plan_id = $1 AND step_id = $2 AND detail ? $3
		ORDER BY id DESC LIMIT 1`, planID, stepID, ledgerFloorKey).Scan(&floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read its ledger mark: %w", err)
	}
	return floor, true, nil
}

// readLedgerEvidence reads the runs of sql above floor that no plan step
// has recorded as its own.
func readLedgerEvidence(ctx context.Context, tx pgx.Tx, floor int64, sql string,
	ev *resumeEvidence) error {
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT a.id, COALESCE(a.outcome, ''),
		a.executed_at FROM sage.action_log a
		WHERE a.id > $1 AND md5(a.sql_executed) = md5($2) AND a.sql_executed = $2
		  AND NOT EXISTS (SELECT 1 FROM sage.plan_step ps WHERE ps.action_log_id = a.id)
		ORDER BY a.id LIMIT $3`, floor, sql, maxLedgerCandidates)
	if err != nil {
		return fmt.Errorf("read the ledger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var outcome string
		var at *time.Time
		if err := rows.Scan(&id, &outcome, &at); err != nil {
			return fmt.Errorf("read the ledger: %w", err)
		}
		ev.ledgerRows++
		if ev.ledgerID == 0 {
			ev.ledgerID, ev.ledgerOutcome, ev.ledgerAt = id, outcome, at
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the ledger: %w", err)
	}
	return nil
}

// ledgerFloor is the ledger's highest action id now: a run started after
// it is above it.
func ledgerFloor(ctx context.Context, tx pgx.Tx) (int64, error) {
	var floor int64
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(max(id), 0)
		FROM sage.action_log`).Scan(&floor); err != nil {
		return 0, fmt.Errorf("read the ledger mark: %w", err)
	}
	return floor, nil
}

// inFlightPlans are the approved or running plans with a step resume
// reads (any state it does not leave to the tick).
func inFlightPlans(ctx context.Context, pool *pgxpool.Pool) ([]int64, error) {
	resting := make([]string, 0, len(restingStates))
	for st := range restingStates {
		resting = append(resting, string(st))
	}
	rows, err := pool.Query(ctx, `/* pg_sage */ SELECT DISTINCT s.plan_id
		FROM sage.plan_step s JOIN sage.plan p ON p.id = s.plan_id
		WHERE p.status IN ('approved', 'running') AND s.state <> ALL($1)
		ORDER BY s.plan_id LIMIT $2`, resting, maxActivePlans)
	if err != nil {
		return nil, fmt.Errorf("changeplan: list the plans to resume: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("changeplan: list the plans to resume: %w", err)
	}
	return ids, nil
}

// takeClaim claims step stepID (in state) for this engine when no other
// engine's claim on it is live; ErrStepMoved otherwise.
func (e *Engine) takeClaim(ctx context.Context, planID, stepID int64, state StepState) error {
	now := e.cfg.Now()
	return e.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step
			SET claimed_by = $4, claimed_until = $5
			WHERE id = $1 AND plan_id = $2 AND state = $3
			  AND (claimed_by IS NULL OR claimed_by IN ('', $4) OR claimed_until IS NULL
			       OR claimed_until <= $6)`, stepID, planID, string(state), e.cfg.Owner,
			now.Add(e.cfg.Lease), now)
		if err != nil {
			return fmt.Errorf("claim step %d of plan %d: %w", stepID, planID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: step %d of plan %d is not %s or another engine holds "+
				"it", ErrStepMoved, stepID, planID, state)
		}
		return nil
	})
}
