//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// policy-04 concurrency: two sidecars claiming the same due schedule at
// the same instant must claim it once. Many trials with a start barrier,
// so a claim that re-checks nothing after the row lock shows up as a
// double claim.

const claimTrials = 25

func TestClaimDueScheduleClaimsOnceUnderContention(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.action_queue
		WHERE status = 'scheduled'`); err != nil {
		t.Fatalf("clear scheduled rows: %v", err)
	}
	findingID := insertActionFinding(t, pool, ctx, "claim_contention")
	for trial := 0; trial < claimTrials; trial++ {
		id := insertDueSchedule(t, ctx, s, findingID, trial)
		claimed, missed, other := claimTwice(ctx, s, id)
		if claimed != 1 || missed != 1 || len(other) != 0 {
			t.Fatalf("trial %d (row %d): %d claims, %d ErrNoDueSchedule, errors %v; "+
				"want exactly one claim", trial, id, claimed, missed, other)
		}
		var status string
		var decidedBy *int
		if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM sage.action_queue
			WHERE id = $1`, id).Scan(&status, &decidedBy); err != nil {
			t.Fatalf("read row %d: %v", id, err)
		}
		if status != "approved" || decidedBy == nil || *decidedBy != 7 {
			t.Fatalf("row %d: status %q decided_by %v; want approved by 7", id, status,
				decidedBy)
		}
	}
}

// insertDueSchedule proposes a row and schedules it for user 7 in a window
// that opened a minute ago and closes in an hour.
func insertDueSchedule(t *testing.T, ctx context.Context, s *ActionStore, findingID,
	trial int) int {
	t.Helper()
	id, err := s.Propose(ctx, nil, findingID,
		fmt.Sprintf("ANALYZE public.claim_contention_%d", trial), "", "moderate")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})
	if _, err := s.pool.Exec(ctx, `UPDATE sage.action_queue
		SET status = 'scheduled', scheduled_by = 7, scheduled_at = now(),
		    run_after = now() - interval '1 minute',
		    window_closes_at = now() + interval '1 hour'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("schedule row %d: %v", id, err)
	}
	return id
}

// claimTwice runs two claims released by one barrier and counts the
// outcomes.
func claimTwice(ctx context.Context, s *ActionStore, id int) (int, int, []error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	claimed, missed := 0, 0
	var other []error
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ClaimDueSchedule(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				claimed++
			case errors.Is(err, ErrNoDueSchedule):
				missed++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return claimed, missed, other
}

// NextDueSchedule reads, unclaimed, only a schedule that may start now:
// due, and inside the window it was planned for; the earliest first.
func TestNextDueScheduleSelectsOnlyStartableRows(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.action_queue
		WHERE status = 'scheduled'`); err != nil {
		t.Fatalf("clear scheduled rows: %v", err)
	}
	if _, err := s.NextDueSchedule(ctx); !errors.Is(err, ErrNoDueSchedule) {
		t.Fatalf("empty queue: %v, want ErrNoDueSchedule", err)
	}
	findingID := insertActionFinding(t, pool, ctx, "next_due")
	later := insertDueSchedule(t, ctx, s, findingID, 100)
	setSchedule(t, ctx, s, later, `run_after = now() + interval '1 minute'`)
	closed := insertDueSchedule(t, ctx, s, findingID, 101)
	setSchedule(t, ctx, s, closed, `run_after = now() - interval '2 hours',
		window_closes_at = now() - interval '1 second'`)
	if _, err := s.NextDueSchedule(ctx); !errors.Is(err, ErrNoDueSchedule) {
		t.Fatalf("only a future and a closed schedule: %v, want ErrNoDueSchedule", err)
	}
	if _, err := s.ClaimDueSchedule(ctx, later); !errors.Is(err, ErrNoDueSchedule) {
		t.Fatalf("claim a schedule not yet due: %v, want ErrNoDueSchedule", err)
	}
	if _, err := s.ClaimDueSchedule(ctx, closed); !errors.Is(err, ErrNoDueSchedule) {
		t.Fatalf("claim a schedule whose window closed: %v, want ErrNoDueSchedule", err)
	}
	insertDueSchedule(t, ctx, s, findingID, 102) // due a minute ago
	first := insertDueSchedule(t, ctx, s, findingID, 103)
	setSchedule(t, ctx, s, first, `run_after = now() - interval '10 minutes'`)
	got, err := s.NextDueSchedule(ctx)
	if err != nil || got.ID != first || got.Status != "scheduled" || got.ScheduledBy == nil ||
		*got.ScheduledBy != 7 || got.DecidedBy != nil {
		t.Fatalf("NextDueSchedule = %+v, %v; want row %d, unclaimed", got, err, first)
	}
	if again, err := s.NextDueSchedule(ctx); err != nil || again.ID != first {
		t.Fatalf("a read claims nothing: second read %+v, %v; want row %d", again, err,
			first)
	}
}

// A schedule withheld at its opening returns to pending, unclaimed, with
// the reason and its approver kept as history; once it is gone (claimed
// or settled by another sidecar) the return is ErrNotScheduled.
func TestReturnScheduledToPending(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	findingID := insertActionFinding(t, pool, ctx, "withhold")
	id := insertDueSchedule(t, ctx, s, findingID, 200)
	got, err := s.ReturnScheduledToPending(ctx, id, "withheld: emergency stop")
	if err != nil || got.Status != "pending" || got.Reason != "withheld: emergency stop" ||
		got.RunAfter != nil || got.WindowClosesAt != nil || got.DecidedBy != nil ||
		got.ScheduledBy == nil || *got.ScheduledBy != 7 {
		t.Fatalf("ReturnScheduledToPending = %+v, %v", got, err)
	}
	if _, err := s.ReturnScheduledToPending(ctx, id, "again"); !errors.Is(err,
		ErrNotScheduled) {
		t.Fatalf("second return: %v, want ErrNotScheduled", err)
	}
	claimed := insertDueSchedule(t, ctx, s, findingID, 201)
	if _, err := s.ClaimDueSchedule(ctx, claimed); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.ReturnScheduledToPending(ctx, claimed, "late"); !errors.Is(err,
		ErrNotScheduled) {
		t.Fatalf("return a claimed row: %v, want ErrNotScheduled", err)
	}
}

func setSchedule(t *testing.T, ctx context.Context, s *ActionStore, id int, set string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE sage.action_queue SET `+set+` WHERE id = $1`,
		id); err != nil {
		t.Fatalf("update schedule %d: %v", id, err)
	}
}
