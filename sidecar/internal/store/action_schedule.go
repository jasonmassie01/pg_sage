package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// Approvals scheduled for the next maintenance window (policy-04). A
// pending row an operator approved outside its window becomes 'scheduled'
// with run_after (the opening) and window_closes_at. The executor claims
// due rows (status 'approved', decided by the scheduler), so one sidecar
// wins. The runner asks the gate before it claims, so a schedule the gate
// withholds returns to 'pending' without approving its recommendation.

var (
	// ErrNotSchedulable: the row is not a pending, unexpired approval of an
	// open finding with the expected content.
	ErrNotSchedulable = errors.New("action is not awaiting approval")
	// ErrNotScheduled: the row has no schedule to cancel.
	ErrNotScheduled = errors.New("action is not scheduled")
	// ErrNoDueSchedule: no scheduled row is due.
	ErrNoDueSchedule = errors.New("no scheduled action is due")
	// ErrScheduleRevised: the claimed row's recommendation was revised or
	// decided since it was scheduled; the row is back in pending.
	ErrScheduleRevised = errors.New("scheduled approval no longer matches its recommendation")
)

// scheduleQueuedSQL schedules one row approveQueuedSQL could approve.
const scheduleQueuedSQL = `/* pg_sage */ UPDATE sage.action_queue q
	 SET status = 'scheduled', scheduled_by = $1::int, scheduled_at = now(),
	     run_after = $4, window_closes_at = $5,
	     reason = 'approved by user ' || $1::int::text || ' for the next maintenance window'
	 WHERE q.id = $2
	   AND q.status = 'pending'
	   AND q.expires_at > now()
	   AND (q.cooldown_until IS NULL OR q.cooldown_until <= now())
	   AND ($3::text IS NULL OR q.proposed_sql = $3)
	   AND EXISTS (
	       SELECT 1 FROM sage.findings f
	        WHERE f.id = q.finding_id AND f.status = 'open'
	          AND f.acted_on_at IS NULL AND f.resolved_at IS NULL)
	 RETURNING ` + queuedActionColumns

// Schedule records userID's approval of a pending row for the window that
// opens at runAfter and closes at closesAt (nil: not within the planning
// scan). A non-empty expectSQL must still be the row's proposed SQL.
func (s *ActionStore) Schedule(ctx context.Context, queueID, userID int, expectSQL string,
	runAfter time.Time, closesAt *time.Time) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var expect *string
	if expectSQL != "" {
		expect = &expectSQL
	}
	a, err := scanQueuedAction(s.pool.QueryRow(qctx, scheduleQueuedSQL, userID, queueID,
		expect, runAfter, closesAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("scheduling action %d: %w", queueID, ErrNotSchedulable)
	}
	if err != nil {
		return nil, fmt.Errorf("scheduling action %d: %w", queueID, err)
	}
	return &a, nil
}

// unscheduleSQL returns a row in status $3 to pending with reason $2 and
// clears its window and any claim. $4 also clears who scheduled it (a
// cancel); otherwise scheduled_by and scheduled_at stay as history.
const unscheduleSQL = `/* pg_sage */ UPDATE sage.action_queue q
	 SET status = 'pending', reason = $2, run_after = NULL, window_closes_at = NULL,
	     decided_by = NULL, decided_at = NULL,
	     scheduled_by = CASE WHEN $4 THEN NULL ELSE q.scheduled_by END,
	     scheduled_at = CASE WHEN $4 THEN NULL ELSE q.scheduled_at END
	 WHERE q.id = $1 AND q.status = $3
	 RETURNING ` + queuedActionColumns

// Unschedule cancels a scheduled approval: the row returns to pending.
func (s *ActionStore) Unschedule(ctx context.Context, queueID, userID int) (
	*QueuedAction, error) {
	reason := "schedule cancelled by user " + strconv.Itoa(userID)
	return s.returnToPending(ctx, queueID, "scheduled", reason, true)
}

// ReturnScheduledToPending returns a schedule the gate withholds at its
// opening to pending with the reason. Who scheduled it stays as history;
// ErrNotScheduled means another sidecar claimed or settled it first.
func (s *ActionStore) ReturnScheduledToPending(ctx context.Context, queueID int,
	reason string) (*QueuedAction, error) {
	return s.returnToPending(ctx, queueID, "scheduled", reason, false)
}

func (s *ActionStore) returnToPending(ctx context.Context, queueID int, from,
	reason string, cancelled bool) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err := scanQueuedAction(s.pool.QueryRow(qctx, unscheduleSQL, queueID, reason, from,
		cancelled))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("returning action %d to pending: %w", queueID, ErrNotScheduled)
	}
	if err != nil {
		return nil, fmt.Errorf("returning action %d to pending: %w", queueID, err)
	}
	return &a, nil
}

// staleSchedulesSQL returns schedules that can no longer run in their
// window to pending: the window closed (or, with no known close, the row
// is a day overdue), or the finding is no longer open. Each schedule
// covers one window only.
const staleSchedulesSQL = `/* pg_sage */ WITH stale AS (
	SELECT q.id, CASE
	    WHEN NOT EXISTS (SELECT 1 FROM sage.findings f WHERE f.id = q.finding_id
	          AND f.status = 'open' AND f.acted_on_at IS NULL AND f.resolved_at IS NULL)
	    THEN 'scheduled approval returned: the finding is no longer open'
	    ELSE 'scheduled approval returned: its maintenance window closed before it ran'
	    END AS why
	  FROM sage.action_queue q
	 WHERE q.status = 'scheduled'
	   AND (q.window_closes_at <= now()
	        OR (q.window_closes_at IS NULL AND q.run_after <= now() - interval '1 day')
	        OR NOT EXISTS (SELECT 1 FROM sage.findings f WHERE f.id = q.finding_id
	             AND f.status = 'open' AND f.acted_on_at IS NULL AND f.resolved_at IS NULL))
	 FOR UPDATE OF q SKIP LOCKED)
	UPDATE sage.action_queue q
	   SET status = 'pending', reason = stale.why, run_after = NULL,
	       window_closes_at = NULL
	  FROM stale WHERE q.id = stale.id AND q.status = 'scheduled'
	RETURNING ` + queuedActionColumns

// ReturnStaleSchedules returns every schedule that missed its window or
// lost its finding to pending, and the rows it returned (ScheduledBy
// still names the approver).
func (s *ActionStore) ReturnStaleSchedules(ctx context.Context) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, staleSchedulesSQL)
	if err != nil {
		return nil, fmt.Errorf("returning stale schedules: %w", err)
	}
	defer rows.Close()
	return scanQueuedActions(rows)
}

// dueScheduleWhere is a schedule that may start now: due and still inside
// the window it was planned for.
const dueScheduleWhere = ` q.status = 'scheduled' AND q.run_after <= now()
	   AND (q.window_closes_at IS NULL OR q.window_closes_at > now())`

// NextDueSchedule reads, without claiming, the earliest schedule that may
// start now, so the runner can ask the gate before it claims it; none is
// ErrNoDueSchedule.
func (s *ActionStore) NextDueSchedule(ctx context.Context) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err := scanQueuedAction(s.pool.QueryRow(qctx, queuedActionSelectSQL+
		` WHERE`+dueScheduleWhere+` ORDER BY q.run_after, q.id LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoDueSchedule
	}
	if err != nil {
		return nil, fmt.Errorf("reading the next due schedule: %w", err)
	}
	return &a, nil
}

// claimDueSQL claims schedule $1 while it may start now: it becomes an
// approval decided by the person who scheduled it. A row another sidecar
// holds is skipped, and the status is checked again under the row lock.
const claimDueSQL = `/* pg_sage */ UPDATE sage.action_queue q
	 SET status = 'approved', decided_by = q.scheduled_by, decided_at = now(),
	     run_after = NULL, window_closes_at = NULL
	 WHERE q.id = (
	     SELECT q.id FROM sage.action_queue q
	      WHERE q.id = $1 AND` + dueScheduleWhere + `
	      FOR UPDATE SKIP LOCKED)
	   AND q.status = 'scheduled'
	 RETURNING ` + queuedActionColumns

// ClaimDueSchedule claims schedule queueID and approves its recommendation
// in one transaction. A schedule whose recommendation was revised since
// is returned to pending and reported as ErrScheduleRevised with the row;
// one that is not (or no longer) due, or that another sidecar holds, is
// ErrNoDueSchedule.
func (s *ActionStore) ClaimDueSchedule(ctx context.Context, queueID int) (
	*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var a QueuedAction
	err := pgx.BeginFunc(qctx, s.pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanQueuedAction(tx.QueryRow(qctx, claimDueSQL, queueID))
		if err != nil || a.RecommendationID == nil {
			return err
		}
		_, err = recommendation.ApproveTx(qctx, tx, *a.RecommendationID, a.ContentHash,
			"user:"+strconv.Itoa(derefInt(a.DecidedBy)))
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNoDueSchedule
	case errors.Is(err, recommendation.ErrRevised), errors.Is(err, recommendation.ErrConflict):
		returned, rerr := s.returnToPending(ctx, a.ID, "scheduled",
			"scheduled approval returned: the recommendation was revised or decided since "+
				"it was approved; review the current version", false)
		if rerr != nil {
			return nil, fmt.Errorf("claimed schedule %d was revised: %w", a.ID, rerr)
		}
		return returned, fmt.Errorf("schedule %d: %w: %v", a.ID, ErrScheduleRevised, err)
	case err != nil:
		return nil, fmt.Errorf("claiming a due schedule: %w", err)
	}
	return &a, nil
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// ListScheduled returns the approvals waiting for their window, earliest
// first.
func (s *ActionStore) ListScheduled(ctx context.Context) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, queuedActionSelectSQL+
		` WHERE q.status = 'scheduled' ORDER BY q.run_after, q.id`)
	if err != nil {
		return nil, fmt.Errorf("listing scheduled actions: %w", err)
	}
	defer rows.Close()
	return scanQueuedActions(rows)
}
