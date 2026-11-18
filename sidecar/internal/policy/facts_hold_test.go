package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// evid-03b: a do_not_touch fact's binding takes the hold route. The gate
// blocks it like every binding, whoever approved the request, and names
// the fact and who confirmed it; rollbacks of pg_sage's own changes and
// read-only diagnostics are never bound. The facts package's real binder
// is exercised with these requests in internal/facts and internal/executor
// (this package cannot import it).

func holdBinding() FactBinding {
	return FactBinding{FactID: 41, Type: "do_not_touch", Subject: "public.orders",
		Route: "hold", Object: "public.orders",
		Summary:     "table public.orders must not be touched",
		ConfirmedBy: "alice@example.com", ConfirmedAt: factConfirmedAt}
}

func TestGateHoldsOperatorApprovedRequestsOnDoNotTouch(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{holdBinding()}}
	gate := newFactGate(t, gateFixture{}, binder)
	for _, approved := range []bool{false, true} {
		req := validIndexRequest()
		req.OperatorApproved = approved
		d := gate.Authorize(context.Background(), req)
		assertDecision(t, d, VerdictBlocked, ReasonBoundByFact)
		want := "fact #41: table public.orders must not be touched (confirmed by " +
			"alice@example.com on 2026-10-03); route: hold"
		if d.Detail != want {
			t.Fatalf("approved=%v detail %q, want %q", approved, d.Detail, want)
		}
	}
	if binder.calls != 2 {
		t.Fatalf("binder consulted %d times, want 2", binder.calls)
	}
}

func TestGateHoldNeverBindsRollbacks(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{holdBinding()}}
	gate := newFactGate(t, gateFixture{}, binder)
	rollback := validIndexRequest()
	rollback.Rollback, rollback.OperatorApproved = true, true
	d := gate.Authorize(context.Background(), rollback)
	if d.Reason == ReasonBoundByFact || d.Reason == ReasonFactsUnavailable {
		t.Fatalf("a rollback of pg_sage's own change was bound: %+v", d)
	}
	if binder.calls != 0 {
		t.Fatalf("binder consulted %d times for a rollback", binder.calls)
	}
}

func TestGateHoldFailsClosedOnUnreadableFacts(t *testing.T) {
	binder := &fakeBinder{err: errors.New("read confirmed facts: connection refused")}
	req := validIndexRequest()
	req.OperatorApproved = true
	d := newFactGate(t, gateFixture{}, binder).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictBlocked, ReasonFactsUnavailable)
	if !strings.Contains(d.Detail, "connection refused") {
		t.Fatalf("detail %q does not say why", d.Detail)
	}
}

// confirmed (blocks) -> expired (the binder answers nothing: the request
// decides as without facts) -> declared again (blocks).
func TestGateHoldFollowsTheFactsState(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{holdBinding()}}
	gate := newFactGate(t, gateFixture{}, binder)
	plain := newTestGate(t, gateFixture{}).Authorize(context.Background(),
		validIndexRequest())
	assertDecision(t, gate.Authorize(context.Background(), validIndexRequest()),
		VerdictBlocked, ReasonBoundByFact)
	binder.bindings = nil
	lifted := gate.Authorize(context.Background(), validIndexRequest())
	if lifted.Verdict != plain.Verdict || lifted.Reason != plain.Reason {
		t.Fatalf("expired fact: %+v, without facts %+v", lifted, plain)
	}
	binder.bindings = []FactBinding{holdBinding()}
	assertDecision(t, gate.Authorize(context.Background(), validIndexRequest()),
		VerdictBlocked, ReasonBoundByFact)
}

// No concurrent access tests here: the gate reads the binder once per
// request; the snapshot property is pinned on the real binder in
// internal/facts/do_not_touch_test.go.
