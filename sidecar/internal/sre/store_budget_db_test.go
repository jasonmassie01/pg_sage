package sre

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Durable model budget (Codex §6, CHECK-15): reservations are atomic,
// charged before dispatch and survive a crash; unsettled or unknown
// reservations hold their full allowance; daily database and deployment
// allocations cannot be overspent by concurrent callers.

func budgetLimits() Limits {
	l := DefaultLimits()
	l.DatabaseDailyTokens, l.DeploymentDailyTokens = 1_000_000, 1_000_000
	return l
}

func tokens(in, out int64, key string) TokenRequest {
	return TokenRequest{Input: in, Output: out, RequestKey: key}
}

func claimed(t *testing.T, st *PostgresStore, subject string) Lease {
	t.Helper()
	ctx := t.Context()
	inv, _, err := st.Create(ctx, lockStart(testScope(t, ctx, st), subject))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return lease
}

func TestReserveModel_ChargesTurnBeforeDispatch(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 40")
	res, err := st.ReserveModel(ctx, lease, tokens(1000, 500, "turn-1"))
	if err != nil || res.State != ReservationReserved || res.Input != 1000 ||
		res.Output != 500 {
		t.Fatalf("reserve = %+v (%v)", res, err)
	}
	inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if inv.ModelTurns != 1 {
		t.Fatalf("model turns = %d, want 1 charged before dispatch", inv.ModelTurns)
	}
	again, err := st.ReserveModel(ctx, lease, tokens(1000, 500, "turn-1"))
	if err != nil || again.ID != res.ID {
		t.Fatalf("repeated request key = %+v (%v), want the same reservation", again, err)
	}
	if inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID); inv.ModelTurns != 1 {
		t.Fatalf("a repeated key charged another turn (%d)", inv.ModelTurns)
	}
}

func TestReserveModel_TurnAndTokenCaps(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 41")
	if _, err := st.ReserveModel(ctx, lease, tokens(16001, 1, "big")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("input over cap = %v, want ErrBudgetExhausted", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(1, 4001, "bigout")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("output over cap = %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(8000, 2000, "t1")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(8001, 1, "t2-over")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("aggregate input 16001 = %v, want ErrBudgetExhausted", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(8000, 2000, "t2")); err != nil {
		t.Fatalf("turn 2 exactly at the caps: %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(1, 1, "t3")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("third turn = %v, want ErrBudgetExhausted", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(0, 1, "zero")); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("zero input = %v, want ErrInvalidRequest", err)
	}
}

// CHECK-15: a crash after reservation does not reset limits. The new
// process sees the unsettled reservation at its full allowance and the
// turn it consumed.
func TestReserveModel_CrashAfterReservationKeepsTheHold(t *testing.T) {
	limits := budgetLimits()
	st, pool, ctx := liveStore(t, limits)
	lease := claimed(t, st, "pid 42")
	if _, err := st.ReserveModel(ctx, lease, tokens(12000, 3000, "before-crash")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Crash: the process and its store are gone; the lease expires.
	restarted, err := NewPostgresStore(pool, limits)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	expireLease(t, ctx, pool, lease)
	lease2, err := restarted.Claim(ctx, lease.Scope, lease.InvestigationID, NewUUID())
	if err != nil {
		t.Fatalf("claim after crash: %v", err)
	}
	if _, err := restarted.ReserveModel(ctx, lease2, tokens(5000, 500, "after")); !errors.Is(
		err, ErrBudgetExhausted) {
		t.Fatalf("reserve past the held allowance = %v, want ErrBudgetExhausted", err)
	}
	res, err := restarted.ReserveModel(ctx, lease2, tokens(4000, 1000, "after-fit"))
	if err != nil || res.State != ReservationReserved {
		t.Fatalf("reserve within the remaining allowance = %+v (%v)", res, err)
	}
	inv, _ := restarted.Get(ctx, lease.Scope, lease.InvestigationID)
	if inv.ModelTurns != 2 {
		t.Fatalf("model turns after crash = %d, want 2", inv.ModelTurns)
	}
}

func TestSettleModel_KnownUsageReleasesTheRest(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 43")
	res, _ := st.ReserveModel(ctx, lease, tokens(15000, 3500, "t1"))
	res, err := st.MarkDispatched(ctx, lease.Scope, res)
	if err != nil || res.State != ReservationInflight {
		t.Fatalf("dispatch = %+v (%v)", res, err)
	}
	settled, err := st.SettleModel(ctx, lease.Scope, res, Usage{Input: 900,
		Output: 100, Known: true})
	if err != nil || settled.State != ReservationSettled {
		t.Fatalf("settle = %+v (%v)", settled, err)
	}
	if _, err := st.SettleModel(ctx, lease.Scope, settled, Usage{Input: 900,
		Output: 100, Known: true}); err != nil {
		t.Fatalf("repeated settle must be idempotent: %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(15000, 3500, "t2")); err != nil {
		t.Fatalf("after settling 900/100 the rest is free again: %v", err)
	}
}

func TestSettleModel_UnknownUsageKeepsTheFullHold(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 44")
	res, _ := st.ReserveModel(ctx, lease, tokens(10000, 2000, "t1"))
	res, _ = st.MarkDispatched(ctx, lease.Scope, res)
	unk, err := st.SettleModel(ctx, lease.Scope, res, Usage{})
	if err != nil || unk.State != ReservationUnknown {
		t.Fatalf("unknown settle = %+v (%v)", unk, err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(7000, 100, "t2")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("an unknown (possibly billed) call was released: %v", err)
	}
}

func TestSettleModel_CancelledBeforeDispatchReleases(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 45")
	res, _ := st.ReserveModel(ctx, lease, tokens(10000, 2000, "t1"))
	c, err := st.CancelModel(ctx, lease.Scope, res)
	if err != nil || c.State != ReservationCancelled {
		t.Fatalf("cancel = %+v (%v)", c, err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(15000, 3000, "t2")); err != nil {
		t.Fatalf("a proven pre-dispatch cancel must release: %v", err)
	}
	dispatched, _ := st.ReserveModel(ctx, claimed(t, st, "pid 45b"), tokens(1, 1, "d"))
	dispatched, _ = st.MarkDispatched(ctx, dispatched.Scope, dispatched)
	if _, err := st.CancelModel(ctx, dispatched.Scope, dispatched); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("cancelling a dispatched call = %v, want ErrInvalidTransition", err)
	}
}

func TestSettleModel_OverrunIsRecordedAndFlagged(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 46")
	res, _ := st.ReserveModel(ctx, lease, tokens(100, 50, "t1"))
	res, _ = st.MarkDispatched(ctx, lease.Scope, res)
	settled, err := st.SettleModel(ctx, lease.Scope, res, Usage{Input: 300,
		Output: 80, Known: true})
	if !errors.Is(err, ErrUsageExceeded) || settled.InputUsed != 300 ||
		settled.OutputUsed != 80 {
		t.Fatalf("overrun settle = %+v (%v), want actuals kept and flagged", settled, err)
	}
}

func TestReserveModel_StaleLeaseIsRefused(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 47")
	inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if _, err := st.Pause(ctx, lease.Scope, lease.InvestigationID, inv.Version); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, tokens(10, 10, "t")); !errors.Is(err,
		ErrLeaseLost) {
		t.Fatalf("reserve on a paused investigation = %v, want ErrLeaseLost", err)
	}
}

func TestReserveModel_NoDailyAllocationRefusesModelUse(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits()) // daily allocations 0
	lease := claimed(t, st, "pid 48")
	_, err := st.ReserveModel(ctx, lease, tokens(10, 10, "t"))
	if !errors.Is(err, ErrBudgetExhausted) ||
		!strings.Contains(err.Error(), "daily allocation") {
		t.Fatalf("reserve without a daily allocation = %v", err)
	}
}

func TestReserveModel_DatabaseDailyCap(t *testing.T) {
	limits := DefaultLimits()
	limits.DatabaseDailyTokens, limits.DeploymentDailyTokens = 3000, 1_000_000
	st, _, ctx := liveStore(t, limits)
	dep, _ := st.EnsureDeployment(ctx)
	scope, _ := st.BindDatabase(ctx, Binding{DeploymentID: dep,
		RuntimeKey: "daily-" + string(NewUUID()), Strength: StrengthConfigured,
		ClusterEpoch: "e"})
	var leases []Lease
	for _, subj := range []string{"a", "b"} {
		inv, _, _ := st.Create(ctx, lockStart(scope, subj))
		l, err := st.Claim(ctx, scope, inv.ID, NewUUID())
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		leases = append(leases, l)
	}
	if _, err := st.ReserveModel(ctx, leases[0], tokens(1500, 500, "x")); err != nil {
		t.Fatalf("first investigation: %v", err)
	}
	if _, err := st.ReserveModel(ctx, leases[1], tokens(800, 201, "y")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("second investigation past the database day = %v", err)
	}
	if _, err := st.ReserveModel(ctx, leases[1], tokens(800, 200, "z")); err != nil {
		t.Fatalf("exactly at the database daily cap: %v", err)
	}
}

// Concurrency: callers on several databases share the deployment's day
// allocation; the advisory lock keeps the total within the cap.
func TestReserveModel_ConcurrentCallersCannotOverspendTheDeployment(t *testing.T) {
	limits := DefaultLimits()
	limits.DatabaseDailyTokens, limits.DeploymentDailyTokens = 5000, 5000
	st, pool, ctx := liveStore(t, limits)
	// Isolate the day's deployment ledger for this test.
	if _, err := pool.Exec(ctx, "DELETE FROM sage.sre_budget_reservations"); err != nil {
		t.Fatalf("clean ledger: %v", err)
	}
	var leases []Lease
	for i := 0; i < 8; i++ {
		leases = append(leases, claimed(t, st, fmt.Sprintf("pid 5%d", i)))
	}
	var granted int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, l := range leases {
		wg.Add(1)
		go func(i int, l Lease) {
			defer wg.Done()
			res, err := st.ReserveModel(ctx, l, tokens(900, 100, fmt.Sprintf("c%d", i)))
			if err == nil {
				mu.Lock()
				granted += res.Input + res.Output
				mu.Unlock()
			} else if !errors.Is(err, ErrBudgetExhausted) {
				t.Errorf("reserve %d: %v", i, err)
			}
		}(i, l)
	}
	wg.Wait()
	if granted != 5000 {
		t.Fatalf("granted %d tokens across the deployment, want exactly 5000", granted)
	}
}
