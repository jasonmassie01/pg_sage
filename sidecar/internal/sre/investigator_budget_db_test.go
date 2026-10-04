package sre

import (
	"errors"
	"testing"
)

// The investigator's durable token budget: each model call is reserved
// before provider I/O against the plan's per-investigation cap and the
// database and deployment daily allocations (all callers), without using
// the review turn's two-turn counter.

func invTokens(in, out int64, key string) TokenRequest {
	return TokenRequest{Input: in, Output: out, RequestKey: key}
}

func TestReserveInvestigator_ChargesTheInvestigationCapInclusively(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 9001")
	if _, err := st.ReserveInvestigator(ctx, lease, invTokens(600, 400, "inv-1"), 2000); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := st.ReserveInvestigator(ctx, lease, invTokens(700, 300, "inv-2"), 2000); err != nil {
		t.Fatalf("a call reaching the cap exactly was refused: %v", err)
	}
	_, err := st.ReserveInvestigator(ctx, lease, invTokens(1, 1, "inv-3"), 2000)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a call over the cap: %v, want ErrBudgetExhausted", err)
	}
	inv, err := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inv.ModelTurns != 0 {
		t.Fatalf("model turns = %d; investigator calls must not use the review counter",
			inv.ModelTurns)
	}
	if got := reservationsOf(t, st, inv); got[InvestigatorCallerKind] != 2 {
		t.Fatalf("reservations = %v, want 2 investigator rows", got)
	}
}

func TestReserveInvestigator_ManyCallsPassTheTwoTurnCeiling(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 9002")
	for i := range CeilingModelTurns + 3 {
		key := "inv-many-" + itoa(int64(i))
		if _, err := st.ReserveInvestigator(ctx, lease, invTokens(100, 100, key),
			CeilingInvestigatorTokens); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
}

func TestReserveInvestigator_RepeatedKeyReturnsTheSameReservation(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 9003")
	a, err := st.ReserveInvestigator(ctx, lease, invTokens(500, 500, "same"), 1500)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := st.ReserveInvestigator(ctx, lease, invTokens(500, 500, "same"), 1500)
	if err != nil || b.ID != a.ID {
		t.Fatalf("repeat = %+v (%v), want the first reservation %s", b, err, a.ID)
	}
}

func TestReserveInvestigator_DailyAllocationsBindEveryCaller(t *testing.T) {
	l := budgetLimits()
	l.DatabaseDailyTokens, l.DeploymentDailyTokens = 3000, 1_000_000
	st, _, ctx := liveStore(t, l)
	lease := claimed(t, st, "pid 9004")
	if _, err := st.ReserveModel(ctx, lease, tokens(1000, 1000, "review")); err != nil {
		t.Fatalf("review turn: %v", err)
	}
	_, err := st.ReserveInvestigator(ctx, lease, invTokens(600, 500, "inv"), 64000)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("investigator over the database's day: %v, want ErrBudgetExhausted", err)
	}
	if _, err := st.ReserveInvestigator(ctx, lease, invTokens(500, 500, "inv-fits"),
		64000); err != nil {
		t.Fatalf("a call that fits the day was refused: %v", err)
	}
}

func TestReserveInvestigator_RefusesInvalidRequests(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 9005")
	cases := map[string]struct {
		req TokenRequest
		cap int64
	}{
		"zero input":     {invTokens(0, 10, "a"), 1000},
		"zero output":    {invTokens(10, 0, "b"), 1000},
		"no key":         {invTokens(10, 10, ""), 1000},
		"zero cap":       {invTokens(10, 10, "c"), 0},
		"cap over":       {invTokens(10, 10, "d"), CeilingInvestigatorTokens + 1},
		"negative think": {TokenRequest{Input: 1, Output: 1, Reasoning: -1, RequestKey: "e"}, 100},
	}
	for name, tc := range cases {
		_, err := st.ReserveInvestigator(ctx, lease, tc.req, tc.cap)
		if !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	none := budgetLimits()
	none.DatabaseDailyTokens, none.DeploymentDailyTokens = 0, 0
	st2, _, _ := liveStore(t, none)
	lease2 := claimed(t, st2, "pid 9006")
	if _, err := st2.ReserveInvestigator(ctx, lease2, invTokens(1, 1, "x"),
		1000); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("no daily allocation: %v, want ErrBudgetExhausted", err)
	}
}

func TestReserveInvestigator_LostLeaseReservesNothing(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 9007")
	stale := lease
	stale.Fence++
	if _, err := st.ReserveInvestigator(ctx, stale, invTokens(1, 1, "x"),
		1000); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale fence: %v, want ErrLeaseLost", err)
	}
	inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID)
	if got := reservationsOf(t, st, inv); len(got) != 0 {
		t.Fatalf("a lost lease reserved %v", got)
	}
}
