package schemaguard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These fixtures are local to each test; no shared concurrent state is used.
func TestRouteFailureRecordsParkedDecision(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
		ProposedSQL: "CREATE INDEX orders_customer_idx ON orders(customer_id)",
	})
	fixture.policy.AllowFKIndexApply = true
	routeErr := errors.New("verification unavailable")
	custodian := fixture.custodian()
	custodian.router = failingRoute{routeErr}
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, routeErr) || result.Routed != 0 || result.Recorded != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(fixture.records.items) != 1 {
		t.Fatalf("records=%+v", fixture.records.items)
	}
	record := fixture.records.items[0]
	if record.Decision.Disposition != DispositionPark || record.Decision.AutoApply ||
		record.Decision.MayDeleteData || record.Decision.Route != RouteVerifyIndex ||
		!strings.Contains(record.Decision.Reason, "routing failed") ||
		record.Invariant.ProposedSQL != fixture.detector.items[0].ProposedSQL {
		t.Fatalf("record=%+v", record)
	}
}

func TestRouteAndRecordFailuresBothPropagate(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
	})
	fixture.policy.AllowFKIndexApply = true
	custodian := fixture.custodian()
	routeErr, recordErr := errors.New("route failure"), errors.New("record failure")
	custodian.router = failingRoute{routeErr}
	custodian.recorder = failingRecord{recordErr}
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, routeErr) || !errors.Is(err, recordErr) ||
		result.Recorded != 0 || result.Routed != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type failingRoute struct{ err error }

func (f failingRoute) Route(context.Context, Remediation) error { return f.err }

type failingRecord struct{ err error }

func (f failingRecord) Record(context.Context, Remediation) error { return f.err }
