package schemaguard

import (
	"context"
	"strings"
	"testing"
)

// migration-09: a structural invariant (everything_text, type_tightening)
// has no automated remediation. It is recorded as an honest recommendation,
// never sent to the router, and never counted as routed.

const structuralReasonFragment = "requires an online column migration"

func TestCustodianStructuralInvariantIsRecordedNotRouted(t *testing.T) {
	for _, kind := range []InvariantKind{InvariantTypeTightening, InvariantEverythingText} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := newCustodianFixture(Invariant{
				Kind: kind, Schema: "public", Table: "orders", Subject: "order_number",
			})
			result, err := fixture.custodian().Scan(context.Background())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if result.Routed != 0 || len(fixture.router.items) != 0 {
				t.Fatalf("result=%+v routes=%#v, want nothing routed", result,
					fixture.router.items)
			}
			if result.Detected != 1 || result.Recorded != 1 ||
				len(fixture.records.items) != 1 {
				t.Fatalf("result=%+v records=%#v, want one recorded decision", result,
					fixture.records.items)
			}
			decision := fixture.records.items[0].Decision
			if decision.Route != RouteRecommendation ||
				decision.Disposition != DispositionRecommend ||
				decision.Class != RemediationStructural || decision.AutoApply ||
				decision.MayDeleteData {
				t.Fatalf("structural decision = %#v", decision)
			}
			if !strings.Contains(decision.Reason, structuralReasonFragment) ||
				strings.Contains(decision.Reason, "authorization pending") {
				t.Fatalf("structural reason = %q", decision.Reason)
			}
		})
	}
}

// State transition: an unchanged structural decision is recorded once,
// then reported unchanged; it is never routed on any cycle.
func TestCustodianStructuralDecisionIsStableAcrossCycles(t *testing.T) {
	f := newDedupeFixture(Invariant{Kind: InvariantTypeTightening, Schema: "public",
		Table: "orders", Subject: "order_number"})
	first := f.scan(t)
	second := f.scan(t)
	if first.Recorded != 1 || first.Routed != 0 {
		t.Fatalf("first scan = %+v, want one record and no route", first)
	}
	if second.Recorded != 0 || second.Unchanged != 1 || second.Routed != 0 {
		t.Fatalf("second scan = %+v, want the decision unchanged", second)
	}
	if routes := len(f.router.items); routes != 0 {
		t.Fatalf("structural invariant routed %d times", routes)
	}
}

// Must not change: the exemption and oscillation parks still win over the
// structural recommendation.
func TestPlanStructuralParksStillApply(t *testing.T) {
	exempt := testRequest(InvariantTypeTightening)
	exempt.Contract.Exemptions = []InvariantKind{InvariantTypeTightening}
	oscillating := testRequest(InvariantEverythingText)
	oscillating.History.ExternalReversions = oscillating.Policy.OscillationLimit
	for name, test := range map[string]struct {
		request Request
		reason  string
	}{
		"exemption":   {exempt, "table contract exemption"},
		"oscillation": {oscillating, "oscillation limit reached"},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := Plan(context.Background(), test.request)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if decision.Disposition != DispositionPark || decision.Reason != test.reason ||
				ShouldRoute(decision) {
				t.Fatalf("decision = %#v, want park %q", decision, test.reason)
			}
		})
	}
}

// Boundary: one below the oscillation limit is still a recommendation.
func TestPlanStructuralBelowOscillationLimitRecommends(t *testing.T) {
	request := testRequest(InvariantTypeTightening)
	request.History.ExternalReversions = request.Policy.OscillationLimit - 1
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if decision.Route != RouteRecommendation || decision.Disposition != DispositionRecommend {
		t.Fatalf("decision = %#v, want a recommendation", decision)
	}
}

func TestShouldRouteOnlyActionableDispositions(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		want     bool
	}{
		{"apply", Decision{Disposition: DispositionApply, Route: RouteVerifyIndex}, true},
		{"dry run", Decision{Disposition: DispositionDryRun, Route: RouteRetention}, true},
		{"recommend index", Decision{Disposition: DispositionRecommend,
			Route: RouteVerifyIndex}, false},
		{"recommend structural", Decision{Disposition: DispositionRecommend,
			Route: RouteRecommendation, Class: RemediationStructural}, false},
		{"legacy clone route", Decision{Disposition: DispositionRecommend,
			Route: Route("clone_rehearsal")}, false},
		{"park", Decision{Disposition: DispositionPark, Route: RouteRetention}, false},
		{"zero", Decision{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ShouldRoute(test.decision); got != test.want {
				t.Fatalf("ShouldRoute(%#v) = %v, want %v", test.decision, got, test.want)
			}
		})
	}
}

// No concurrent-access test: Plan and ShouldRoute are pure functions; the
// custodian's scan serialization is covered by dedupe_test.go.
// No integration test here: the ledger write is autonomy's (see
// autonomy/schema_structural_ledger_db_test.go).
