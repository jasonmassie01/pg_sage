package schemaguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A router that declines to act yet (a retention dry run still in review)
// parks the remediation: it is recorded with the router's reason, is not a
// route failure, and the scan continues. Fixtures are local to each test.

const reviewReason = "retention dry run in review until 2026-09-28T20:00:00Z"

var errStillReviewing = errors.New("retention dry run pending review")

type reviewPendingRouter struct{ routed []Remediation }

func (r *reviewPendingRouter) Route(_ context.Context, item Remediation) error {
	if item.Decision.Route == RouteRetention {
		return &ParkedRoute{Reason: reviewReason, Err: errStillReviewing}
	}
	r.routed = append(r.routed, item)
	return nil
}

func TestScanParksPendingRouteAndContinues(t *testing.T) {
	detector := &fakeDetector{items: []Invariant{
		{Kind: InvariantUnboundedAppend, Schema: "public", Table: "events",
			RetentionColumn: "created_at"},
		{Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
			ProposedSQL: "CREATE INDEX CONCURRENTLY orders_customer_idx ON orders(customer_id)"},
	}}
	contracts := &fakeContractSource{contract: TableContract{AppendOnly: true,
		RetentionWindow: 30 * 24 * time.Hour, RetentionColumn: "created_at"}}
	history := &fakeHistorySource{history: History{SuccessfulRetentionDryRuns: 1}}
	router := &reviewPendingRouter{}
	records := &fakeDecisionRecorder{}
	custodian := NewCustodian(detector, contracts, history, router, records, testPolicy())

	result, err := custodian.Scan(context.Background())

	if err != nil {
		t.Fatalf("Scan returned %v; a pending review must park, not fail", err)
	}
	if result.Detected != 2 || result.Routed != 1 || result.Recorded != 2 {
		t.Fatalf("result=%+v, want 2 detected, 1 routed, 2 recorded", result)
	}
	if len(router.routed) != 1 || router.routed[0].Invariant.Kind != InvariantMissingFKIndex {
		t.Fatalf("routed=%+v, want the FK invariant after the parked retention", router.routed)
	}
	park := records.items[0].Decision
	if park.Disposition != DispositionPark || park.Reason != reviewReason ||
		park.AutoApply || park.MayDeleteData || park.Route != RouteRetention {
		t.Fatalf("retention record=%+v, want park with the review reason", park)
	}
}

func TestParkedRouteUnwrapsCause(t *testing.T) {
	var err error = &ParkedRoute{Reason: reviewReason, Err: errStillReviewing}
	var parked *ParkedRoute
	if !errors.As(err, &parked) || parked.Reason != reviewReason {
		t.Fatalf("errors.As(%v) did not yield the parked route", err)
	}
	if !errors.Is(err, errStillReviewing) {
		t.Fatalf("ParkedRoute must unwrap its cause: %v", err)
	}
	if err.Error() == "" {
		t.Fatal("ParkedRoute has an empty error message")
	}
}

// A genuine route failure is still a failure: parking must not swallow it.
func TestRouteFailureIsNotTreatedAsPark(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
	})
	fixture.policy.AllowFKIndexApply = true
	custodian := fixture.custodian()
	custodian.router = failingRoute{errStillReviewing}

	_, err := custodian.Scan(context.Background())

	if !errors.Is(err, errStillReviewing) {
		t.Fatalf("Scan = %v, want the route failure to propagate", err)
	}
}
