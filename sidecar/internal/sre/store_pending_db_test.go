package sre

import (
	"errors"
	"testing"
	"time"
)

// Pending lists the work a coordinator resumes after a restart: queued,
// waiting for evidence, or active with an expired lease (a dead worker).
func TestStore_PendingFindsQueuedAndOrphanedWork(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 300 * time.Millisecond
	st, _, ctx := liveStore(t, limits)
	scope := testScope(t, ctx, st)
	queued, _, _ := st.Create(ctx, lockStart(scope, "pid 60"))
	orphan, _, _ := st.Create(ctx, lockStart(scope, "pid 61"))
	held, _, _ := st.Create(ctx, lockStart(scope, "pid 62"))
	done, _ := evaluating(t, ctx, st, scope, "pid 63")
	if _, err := st.Conclude(ctx, done, Conclusion{State: StateInconclusive,
		Summary: Summary{Reason: "no lock waits"}}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if _, err := st.Claim(ctx, scope, orphan.ID, NewUUID()); err != nil {
		t.Fatalf("claim orphan: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := st.Claim(ctx, scope, held.ID, NewUUID()); err != nil {
		t.Fatalf("claim held: %v", err)
	}
	ids, err := st.Pending(ctx, scope, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	want := map[UUID]bool{queued.ID: true, orphan.ID: true}
	if len(ids) != 2 || !want[ids[0]] || !want[ids[1]] {
		t.Fatalf("pending = %v, want queued %s and orphaned %s", ids, queued.ID, orphan.ID)
	}
	if ids, err := st.Pending(ctx, scope, 1); err != nil || len(ids) != 1 ||
		ids[0] != queued.ID {
		t.Fatalf("pending limit 1 = %v (%v), want the oldest", ids, err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := st.Pending(ctx, scope, limit); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("pending limit %d = %v, want ErrInvalidRequest", limit, err)
		}
	}
}

// An investigation whose active-time budget a dead worker used up ends
// as failed (budget_exhausted) instead of staying "collecting" forever.
func TestStore_ClaimPastTheBudgetFailsTheInvestigation(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxActive, limits.LeaseTTL = time.Second, time.Second
	st, _, ctx := liveStore(t, limits)
	scope := testScope(t, ctx, st)
	inv, _, _ := st.Create(ctx, lockStart(scope, "pid 64"))
	if _, err := st.Claim(ctx, scope, inv.ID, NewUUID()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(1300 * time.Millisecond)
	if _, err := st.Claim(ctx, scope, inv.ID, NewUUID()); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("claim past the budget = %v, want ErrBudgetExhausted", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateFailed || got.FailureCode != "budget_exhausted" ||
		got.LeaseOwner != "" {
		t.Fatalf("after exhaustion = %+v, want failed/budget_exhausted", got)
	}
	if ids, _ := st.Pending(ctx, scope, 10); len(ids) != 0 {
		t.Fatalf("exhausted investigation still pending: %v", ids)
	}
	if err := st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatalf("chain: %v", err)
	}
}
