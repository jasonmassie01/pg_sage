package schemaguard

import (
	"context"
	"testing"
	"time"
)

func TestRetentionRequiresDryRunBeforeConsentBackedEnforcement(t *testing.T) {
	request := Request{
		Invariant: Invariant{
			Kind: InvariantUnboundedAppend, Schema: "public", Table: "events",
		},
		Contract: TableContract{
			AppendOnly: true, RetentionWindow: 30 * 24 * time.Hour,
		},
		Policy: Policy{AllowRetentionApply: true},
	}

	first, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("first Plan: %v", err)
	}
	if first.Disposition != DispositionDryRun || first.MayDeleteData || first.AutoApply {
		t.Fatalf("first retention decision = %#v", first)
	}

	request.History.SuccessfulRetentionDryRuns = 1
	second, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("second Plan: %v", err)
	}
	if second.Disposition != DispositionApply || !second.MayDeleteData ||
		!second.AutoApply {
		t.Fatalf("second retention decision = %#v", second)
	}
}

func TestStructuralPathologiesRouteOnlyToCloneRehearsal(t *testing.T) {
	for _, kind := range []InvariantKind{InvariantEverythingText, InvariantTypeTightening} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := newCustodianFixture(Invariant{
				Kind: kind, Schema: "public", Table: "agent_events",
				ProposedSQL: "ALTER TABLE public.agent_events ALTER COLUMN id TYPE bigint",
			})
			result, err := fixture.custodian().Scan(context.Background())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if result.Routed != 1 || len(fixture.router.items) != 1 {
				t.Fatalf("result=%#v routes=%#v", result, fixture.router.items)
			}
			decision := fixture.router.items[0].Decision
			if decision.Route != RouteCloneRehearsal ||
				decision.Disposition != DispositionRecommend ||
				!decision.RequiresRehearsal || decision.AutoApply {
				t.Fatalf("structural decision = %#v", decision)
			}
		})
	}
}
