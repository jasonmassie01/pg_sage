package sre

import (
	"errors"
	"testing"
)

// Reasoning budget (Sage SRE M3): a thinking model's reasoning tokens
// are reserved separately from the 4k answer ceiling, up to
// MaxReasoningTokens per investigation (ceiling 16384). Settled rows
// count what the provider reported; reserved, inflight and unknown rows
// hold their full reasoning allowance; daily allocations count it too.

func TestLimits_ReasoningCeiling(t *testing.T) {
	l := DefaultLimits()
	if l.MaxReasoningTokens != CeilingReasoningTokens || CeilingReasoningTokens != 16384 ||
		l.MaxOutputTokens != 4000 {
		t.Fatalf("defaults = %+v", l)
	}
	for _, v := range []int64{1, 8192, 16384} {
		l.MaxReasoningTokens = v
		if err := l.Validate(); err != nil {
			t.Errorf("reasoning %d rejected: %v", v, err)
		}
	}
	for _, v := range []int64{0, -1, 16385} {
		l.MaxReasoningTokens = v
		if err := l.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("reasoning %d = %v, want ErrInvalidRequest", v, err)
		}
	}
}

func reasoningReq(key string, reasoning int64) TokenRequest {
	return TokenRequest{Input: 8000, Output: 2000, Reasoning: reasoning, RequestKey: key}
}

func TestReserveModel_ReasoningAllowancePerInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 70")
	res, err := st.ReserveModel(ctx, lease, reasoningReq("r1", 8192))
	if err != nil || res.Reasoning != 8192 || res.Output != 2000 {
		t.Fatalf("reserve = %+v (%v)", res, err)
	}
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("r2", 8193)); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("reasoning over the investigation's 16384 = %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("r3", 8192)); err != nil {
		t.Fatalf("the second 8192 must fit: %v", err)
	}
	if inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID); inv.ModelTurns != 2 {
		t.Fatalf("model turns = %d, want 2 (the refused one is not charged)", inv.ModelTurns)
	}
}

func TestReserveModel_ReasoningRequestBounds(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 71")
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("neg", -1)); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("negative reasoning = %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("big", 16385)); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("reasoning over the cap = %v", err)
	}
	res, err := st.ReserveModel(ctx, lease, reasoningReq("none", 0))
	if err != nil || res.Reasoning != 0 {
		t.Fatalf("a non-thinking reservation = %+v (%v)", res, err)
	}
}

// The daily allocation counts answer and reasoning together.
func TestReserveModel_DailyAllocationCountsReasoning(t *testing.T) {
	limits := budgetLimits()
	// The deployment's day is shared by every test of the package; the
	// database's (a fresh scope) is the tight one.
	limits.DatabaseDailyTokens, limits.DeploymentDailyTokens = 30000, 1_000_000_000
	st, _, ctx := liveStore(t, limits)
	first := claimed(t, st, "pid 72")
	if _, err := st.ReserveModel(ctx, first, reasoningReq("d1", 8192)); err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := st.Claim(ctx, first.Scope, mustCreate(t, st, first.Scope, "pid 73"),
		NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.ReserveModel(ctx, second, reasoningReq("d2", 8192)); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("18192 + 18192 over a 30000 database day = %v", err)
	}
	if _, err := st.ReserveModel(ctx, second, reasoningReq("d3", 0)); err != nil {
		t.Fatalf("10000 without reasoning fits: %v", err)
	}
}

// Reasoning over the allowance is recorded as reported, flagged, and
// refuses the next turn that would exceed the investigation's cap.
func TestSettleModel_ReasoningOverrunRecordedAndRefusesNextTurn(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 74")
	res, err := st.ReserveModel(ctx, lease, reasoningReq("o1", 8192))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	out, err := st.SettleModel(ctx, lease.Scope, res,
		Usage{Input: 3000, Output: 500, Reasoning: 15000, Known: true})
	if !errors.Is(err, ErrUsageExceeded) || out.State != ReservationSettled ||
		out.ReasoningUsed != 15000 || out.InputUsed != 3000 || out.OutputUsed != 500 {
		t.Fatalf("settle = %+v (%v)", out, err)
	}
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("o2", 8192)); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("15000 used + 8192 over 16384 = %v", err)
	}
}

// An unknown outcome keeps the full reasoning hold.
func TestSettleModel_UnknownKeepsReasoningHold(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 75")
	res, err := st.ReserveModel(ctx, lease, reasoningReq("u1", 16384))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := st.SettleModel(ctx, lease.Scope, res, Usage{}); err != nil {
		t.Fatalf("settle unknown: %v", err)
	}
	if _, err := st.ReserveModel(ctx, lease, reasoningReq("u2", 1)); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("an unknown 16384 hold must still count: %v", err)
	}
}

func mustCreate(t *testing.T, st *PostgresStore, scope Scope, subject string) UUID {
	t.Helper()
	inv, _, err := st.Create(t.Context(), lockStart(scope, subject))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return inv.ID
}
