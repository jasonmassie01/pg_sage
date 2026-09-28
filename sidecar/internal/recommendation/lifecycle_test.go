package recommendation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newApproved(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Store) Recommendation {
	t.Helper()
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	approved, err := s.Approve(ctx, rec.ID, rec.ContentHash, "user:3")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return approved
}

func claim(t *testing.T, ctx context.Context, s *Store, rec Recommendation) Claim {
	t.Helper()
	c, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: rec.Revision, Lease: time.Hour})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return c
}

func TestFullLifecycleRecordsEveryStep(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	if c.ID != rec.ID || c.Revision != 1 || c.ContentHash != rec.ContentHash || c.Attempt != 1 {
		t.Fatalf("claim = %+v", c)
	}
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	if err := s.StartVerifying(ctx, c); err != nil {
		t.Fatalf("start verifying: %v", err)
	}
	got := mustGet(t, ctx, s, rec.ID)
	if got.State != StateVerifying || got.ActionLogID == nil || *got.ActionLogID != actionID ||
		got.LeaseUntil != nil {
		t.Fatalf("head = %+v, want verifying linked to action %d, no lease", got, actionID)
	}
	want := []State{StateProposed, StateApproved, StateApplying, StateApplied, StateVerifying}
	h := transitionsOf(t, ctx, s, rec.ID)
	if len(h) != len(want) {
		t.Fatalf("history = %+v", h)
	}
	for i, st := range want {
		if h[i].To != st {
			t.Fatalf("history[%d] = %s, want %s", i, h[i].To, st)
		}
	}
	if h[1].Actor != "user:3" || h[3].ActionLogID == nil || *h[3].ActionLogID != actionID {
		t.Fatalf("approval actor %q / applied action %v", h[1].Actor, h[3].ActionLogID)
	}
}

func TestClaimApprovesProposedForPolicy(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	if _, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: 1, Lease: time.Hour}); !errors.Is(
		err, ErrConflict) {
		t.Fatalf("claiming an unapproved recommendation: err=%v, want ErrConflict", err)
	}
	c, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: 1,
		ApproveAs: "policy:decision:42", Lease: time.Hour})
	if err != nil || c.Attempt != 1 {
		t.Fatalf("policy claim: %+v, %v", c, err)
	}
	got := mustGet(t, ctx, s, rec.ID)
	if got.State != StateApplying || got.ApprovedBy != "policy:decision:42" ||
		got.ApprovedHash != rec.ContentHash || got.LeaseUntil == nil {
		t.Fatalf("head = %+v", got)
	}
}

func TestClaimRefusesStaleRevision(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	_, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: 2, Lease: time.Hour})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("claim of an unknown revision: err=%v, want ErrConflict", err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateApproved || got.AttemptCount != 0 {
		t.Fatalf("head after refused claim = %s attempts=%d", got.State, got.AttemptCount)
	}
}

// Two workers race to claim one approval; exactly one owns the apply.
func TestClaimRaceExactlyOneWins(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	for round := 0; round < 10; round++ {
		rec := newApproved(t, ctx, pool, s)
		wins, conflicts := raceClaims(ctx, s, rec, 2, "")
		if wins != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d wins, %d conflicts; want exactly one winner",
				round, wins, conflicts)
		}
		if got := mustGet(t, ctx, s, rec.ID); got.AttemptCount != 1 {
			t.Fatalf("round %d: attempt_count=%d, want 1", round, got.AttemptCount)
		}
	}
}

// A policy claim of a proposed recommendation races an operator approval
// path; one approval, one apply.
func TestPolicyClaimRaceExactlyOneWins(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	wins, conflicts := raceClaims(ctx, s, rec, 8, "policy")
	if wins != 1 || conflicts != 7 {
		t.Fatalf("%d wins, %d conflicts; want 1 and 7", wins, conflicts)
	}
	approvals := 0
	for _, tr := range transitionsOf(t, ctx, s, rec.ID) {
		if tr.To == StateApproved {
			approvals++
		}
	}
	if approvals != 1 {
		t.Fatalf("%d approval transitions recorded, want 1", approvals)
	}
}

func raceClaims(
	ctx context.Context, s *Store, rec Recommendation, workers int, approveAs string,
) (int, int) {
	var mu sync.Mutex
	var wins, conflicts int
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: rec.Revision,
				ApproveAs: approveAs, Lease: time.Hour})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			}
		}()
	}
	close(start)
	wg.Wait()
	return wins, conflicts
}

func TestTransitionRaceExactlyOneWins(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, to := range []State{StateSuperseded, StateApplying} {
		wg.Add(1)
		go func(to State) {
			defer wg.Done()
			results <- s.Transition(ctx, rec.ID, StateApproved, to, rec.Revision, "w", "race")
		}(to)
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("%d succeeded, %d conflicted; want 1 and 1", ok, conflict)
	}
	if n := len(transitionsOf(t, ctx, s, rec.ID)); n != 3 {
		t.Fatalf("history has %d rows, want 3 (proposed, approved, one winner)", n)
	}
}

// Decision (a): a failed apply moves to failed with backoff, never back
// to proposed; the retry budget bounds the attempts.
func TestRetryBudgetBoundaries(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	for _, budget := range []int{0, 1, 3} {
		rec := newApproved(t, ctx, pool, s)
		if _, err := pool.Exec(ctx, `UPDATE sage.recommendation SET retry_budget=$2
			WHERE id=$1`, rec.ID, budget); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= budget+1; attempt++ {
			c := claim(t, ctx, s, mustGet(t, ctx, s, rec.ID))
			if c.Attempt != attempt {
				t.Fatalf("budget %d: claim attempt %d, want %d", budget, c.Attempt, attempt)
			}
			state, err := s.RecordFailure(ctx, c, "lock timeout")
			if err != nil {
				t.Fatalf("budget %d attempt %d: %v", budget, attempt, err)
			}
			got := mustGet(t, ctx, s, rec.ID)
			if attempt <= budget {
				assertRetryable(t, ctx, pool, got, state, budget, attempt)
				makeDue(t, ctx, pool, rec.ID)
				continue
			}
			if state != StateAbandoned || got.State != StateAbandoned ||
				got.Reason == "" || got.NextAttemptAt != nil {
				t.Fatalf("budget %d exhausted: state=%s head=%+v, want abandoned with reason",
					budget, state, got)
			}
		}
		_, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: 1, Lease: time.Hour})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("budget %d: abandoned recommendation was claimable: %v", budget, err)
		}
		for _, tr := range transitionsOf(t, ctx, s, rec.ID) {
			if tr.From == StateFailed && tr.To == StateProposed {
				t.Fatalf("budget %d: failed silently returned to proposed", budget)
			}
		}
	}
}

func assertRetryable(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, got Recommendation,
	state State, budget, attempt int,
) {
	t.Helper()
	if state != StateFailed || got.State != StateFailed || got.NextAttemptAt == nil ||
		got.ApprovedHash == "" || got.Reason != "lock timeout" {
		t.Fatalf("budget %d attempt %d: state=%s head=%+v, want failed with backoff",
			budget, attempt, state, got)
	}
	var due bool
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at > now() + $2::interval
		FROM sage.recommendation WHERE id=$1`, got.ID,
		(Backoff(attempt) - time.Minute).String()).Scan(&due); err != nil || !due {
		t.Fatalf("budget %d attempt %d: next_attempt_at is not the backoff (%v, %v)",
			budget, attempt, due, err)
	}
}

func makeDue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation
		SET next_attempt_at = now() - interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestFailedNotClaimableBeforeBackoff(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	if _, err := s.RecordFailure(ctx, claim(t, ctx, s, rec), "boom"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Claim(ctx, ClaimRequest{ID: rec.ID, Revision: 1, Lease: time.Hour})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("claim during backoff: err=%v, want ErrConflict", err)
	}
	makeDue(t, ctx, pool, rec.ID)
	if c := claim(t, ctx, s, mustGet(t, ctx, s, rec.ID)); c.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", c.Attempt)
	}
}

func TestRecordAppliedRequiresLiveClaim(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	if _, err := s.RecordFailure(ctx, c, "first"); err != nil {
		t.Fatal(err)
	}
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("applied after the claim was resolved: err=%v, want ErrConflict", err)
	}
	if _, err := s.RecordFailure(ctx, c, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second failure on one claim: err=%v, want ErrConflict", err)
	}
}
