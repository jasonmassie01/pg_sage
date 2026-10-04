package policy

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Roadmap 2.3: confirmed facts are binding. The gate asks the fact binder
// which confirmed facts a request touches; any binding blocks the request
// with a reason naming the fact. Facts only ever narrow: they never turn
// a non-execute verdict into execute, and with no binding the verdict is
// exactly what it would have been without facts.

type fakeBinder struct {
	bindings []FactBinding
	err      error
	calls    int
	seen     []ActionRequest
}

func (b *fakeBinder) Bind(_ context.Context, req ActionRequest) ([]FactBinding, error) {
	b.calls++
	b.seen = append(b.seen, req)
	return b.bindings, b.err
}

var factConfirmedAt = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func appOwnedBinding() FactBinding {
	return FactBinding{FactID: 12, Type: "owned_by_app_migrations",
		Subject: "public.orders", Route: "source_fix", Object: "public.orders",
		Summary:     "table public.orders is owned by the application's migrations",
		ConfirmedBy: "alice@example.com", ConfirmedAt: factConfirmedAt}
}

func newFactGate(t *testing.T, fixture gateFixture, binder FactBinder) Gate {
	t.Helper()
	gate := newTestGate(t, fixture)
	gate.(*authorizationGate).config.Facts = binder
	return gate
}

func TestGateBlocksARequestBoundByAConfirmedFact(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding()}}
	calls := []string{}
	gate := newFactGate(t, gateFixture{calls: &calls}, binder)
	d := gate.Authorize(context.Background(), validIndexRequest())
	assertDecision(t, d, VerdictBlocked, ReasonBoundByFact)
	for _, want := range []string{"fact #12", "table public.orders is owned by the " +
		"application's migrations", "confirmed by alice@example.com on 2026-10-03",
		"route: source_fix"} {
		if !strings.Contains(d.Detail, want) {
			t.Fatalf("detail %q lacks %q", d.Detail, want)
		}
	}
	if binder.calls != 1 || d.RiskTier != RiskSafe {
		t.Fatalf("binder calls %d, risk %q", binder.calls, d.RiskTier)
	}
	assertNotCalled(t, calls, "policy", "usage")
}

func TestGateFactDetailNamesEveryBinding(t *testing.T) {
	second := appOwnedBinding()
	second.FactID, second.Subject = 30, "public"
	second.Summary = "schema public is owned by the application's migrations"
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding(), second}}
	d := newFactGate(t, gateFixture{}, binder).Authorize(context.Background(),
		validIndexRequest())
	if !strings.Contains(d.Detail, "fact #12") || !strings.Contains(d.Detail, "fact #30") {
		t.Fatalf("detail %q must name both facts", d.Detail)
	}
}

// An operator's approval does not get past a binding fact: the operator
// rejects or expires the fact first (one click), which is recorded.
func TestGateFactsBindOperatorApprovals(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding()}}
	req := validIndexRequest()
	req.OperatorApproved = true
	d := newFactGate(t, gateFixture{}, binder).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictBlocked, ReasonBoundByFact)
	if len(binder.seen) != 1 || !binder.seen[0].OperatorApproved {
		t.Fatalf("the binder must see the operator approval: %+v", binder.seen)
	}
}

func TestGateDoesNotAskFactsForReadOnlyOrRollbacks(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding()}}
	gate := newFactGate(t, gateFixture{}, binder)
	read := validIndexRequest()
	read.Contract = &ActionContract{ActionType: "diagnose_lock_blockers", RiskTier: RiskReadOnly}
	read.SQL = "SELECT 1 /* CONCURRENTLY */"
	if d := gate.Authorize(context.Background(), read); d.Reason == ReasonBoundByFact {
		t.Fatalf("read-only diagnostics are never bound: %+v", d)
	}
	rollback := validIndexRequest()
	rollback.Rollback = true
	if d := gate.Authorize(context.Background(), rollback); d.Reason == ReasonBoundByFact {
		t.Fatalf("undoing pg_sage's own change is never bound: %+v", d)
	}
	if binder.calls != 0 {
		t.Fatalf("binder consulted %d times", binder.calls)
	}
}

func TestGateFailsClosedWhenFactsAreUnavailable(t *testing.T) {
	binder := &fakeBinder{err: errors.New("relation sage.facts does not exist")}
	d := newFactGate(t, gateFixture{}, binder).Authorize(context.Background(),
		validIndexRequest())
	assertDecision(t, d, VerdictBlocked, ReasonFactsUnavailable)
	if !strings.Contains(d.Detail, "sage.facts") {
		t.Fatalf("detail %q", d.Detail)
	}
}

func TestGateWithoutBindingsDecidesAsWithoutFacts(t *testing.T) {
	plain := newTestGate(t, gateFixture{})
	withFacts := newFactGate(t, gateFixture{}, &fakeBinder{})
	a := plain.Authorize(context.Background(), validIndexRequest())
	b := withFacts.Authorize(context.Background(), validIndexRequest())
	if a.Verdict != VerdictExecute || a.Verdict != b.Verdict || a.Reason != b.Reason ||
		a.Detail != b.Detail {
		t.Fatalf("without facts %+v, with no binding %+v", a, b)
	}
}

func TestGateHardStopsPrecedeFacts(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding()}}
	gate := newFactGate(t, gateFixture{runtime: RuntimeState{ExecutorEnabled: true,
		EmergencyStop: true}, runtimeSet: true}, binder)
	d := gate.Authorize(context.Background(), validIndexRequest())
	assertDecision(t, d, VerdictBlocked, ReasonEmergencyStop)
	if binder.calls != 0 {
		t.Fatal("facts consulted after an emergency stop")
	}
}

func TestGateExplainMatchesAuthorizeForFacts(t *testing.T) {
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding()}}
	gate := newFactGate(t, gateFixture{}, binder)
	explained := gate.(Explainer).Explain(context.Background(), validIndexRequest())
	authorized := gate.Authorize(context.Background(), validIndexRequest())
	if explained.Verdict != authorized.Verdict || explained.Reason != authorized.Reason ||
		explained.Detail != authorized.Detail {
		t.Fatalf("explain %+v authorize %+v", explained, authorized)
	}
}

// The never-widen property: for randomized runtimes, policy documents and
// requests, adding confirmed facts (a binder that answers bindings, or
// none) never turns a non-execute verdict into execute; a binding always
// yields a non-execute verdict; and no binding leaves the verdict and
// reason unchanged.
func TestFactsNeverWidenAuthority(t *testing.T) {
	rng := rand.New(rand.NewSource(20261004))
	for i := 0; i < 3000; i++ {
		fixture, req := randomGateCase(rng)
		base := newTestGate(t, fixture).(Explainer).Explain(context.Background(), req)
		for _, bindings := range [][]FactBinding{nil, {appOwnedBinding()}} {
			bound := newFactGate(t, fixture, &fakeBinder{bindings: bindings}).(Explainer).
				Explain(context.Background(), req)
			if bound.Verdict == VerdictExecute && base.Verdict != VerdictExecute {
				t.Fatalf("case %d: facts widened %s/%s to execute (req %+v runtime %+v)",
					i, base.Verdict, base.Reason, req, fixture.runtime)
			}
			// Read-only diagnostics and rollbacks are never bound by design
			// (TestGateDoesNotAskFactsForReadOnlyOrRollbacks).
			exempt := req.Rollback || req.Contract.RiskTier == RiskReadOnly
			if len(bindings) > 0 && !exempt && bound.Verdict == VerdictExecute {
				t.Fatalf("case %d: a binding fact executed (req %+v)", i, req)
			}
			if len(bindings) == 0 && (bound.Verdict != base.Verdict ||
				bound.Reason != base.Reason) {
				t.Fatalf("case %d: no binding changed %s/%s to %s/%s", i, base.Verdict,
					base.Reason, bound.Verdict, bound.Reason)
			}
		}
	}
}

func randomGateCase(rng *rand.Rand) (gateFixture, ActionRequest) {
	pick := func(n int) int { return rng.Intn(n) }
	now := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	trust := []string{TrustObservation, TrustAdvisory, TrustAutonomous, "bogus"}
	modes := []string{ExecutionAuto, ExecutionApproval, ExecutionManual}
	runtime := RuntimeState{
		ExecutorEnabled: pick(8) != 0, EmergencyStop: pick(10) == 0,
		IsReplica: pick(12) == 0, TrustLevel: trust[pick(len(trust))],
		ExecutionMode: modes[pick(len(modes))], Tier3Safe: pick(2) == 0,
		Tier3Moderate: pick(2) == 0, InConfiguredWindow: pick(2) == 0,
		RampStart: now.Add(-time.Duration(pick(60*24)) * time.Hour),
	}
	doc := UnattendedProfile()
	if pick(2) == 0 {
		doc.MaintenanceWindows = []string{"always"}
	}
	tiers := []RiskTier{RiskReadOnly, RiskSafe, RiskModerate, RiskHigh, "weird"}
	types := []string{"create_index_concurrently", "drop_unused_index", "vacuum_table",
		"cancel_backend"}
	req := validIndexRequest()
	req.Contract = &ActionContract{ActionType: types[pick(len(types))],
		RiskTier: tiers[pick(len(tiers))]}
	if pick(4) == 0 {
		req.Contract.Guardrails = []Guardrail{GuardrailApprovalRequired}
	}
	req.OperatorApproved = pick(3) == 0
	req.Rollback = pick(6) == 0
	return gateFixture{runtime: runtime, runtimeSet: true, policy: doc, now: now}, req
}
