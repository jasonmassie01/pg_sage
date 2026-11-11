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
		claimed, missed, other := claimTwice(ctx, s)
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
func claimTwice(ctx context.Context, s *ActionStore) (int, int, []error) {
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
			_, err := s.ClaimDueSchedule(ctx)
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
