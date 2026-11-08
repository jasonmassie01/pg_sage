package schemaguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

func (f failingRecord) Record(context.Context, DecisionRecord) error { return f.err }

// migration-08: a failed route parks its item and the scan continues; the
// park is recorded only when its class changes; a connection failure stops
// the scan without forgetting the invariants it did not reach.

func TestRouteFailureParksTheItemAndTheScanContinues(t *testing.T) {
	routeErr := errors.New("verification unavailable")
	router := newScriptRouter()
	router.set("second", routeErr)
	custodian, ledger := newScriptCustodian(router, "first", "second", "third")
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, routeErr) {
		t.Fatalf("Scan error = %v, want the route error", err)
	}
	if router.total() != 3 || router.callsFor("third") != 1 {
		t.Fatalf("router calls = %v, want all three invariants routed", router.snapshot())
	}
	if result.Detected != 3 || result.Routed != 2 || result.Recorded != 3 {
		t.Fatalf("result = %+v, want 3 detected, 2 routed, 3 recorded", result)
	}
	rows := rowsFor(ledger, "second")
	want := "schema remediation routing failed (other); review runtime error before retrying"
	if len(rows) != 1 || rows[0].Decision.Disposition != DispositionPark ||
		rows[0].Decision.Reason != want || rows[0].Decision.AutoApply ||
		rows[0].Decision.MayDeleteData {
		t.Fatalf("second's rows = %+v, want one park with reason %q", rows, want)
	}
	for _, table := range []string{"first", "third"} {
		rows := rowsFor(ledger, table)
		if len(rows) != 1 || rows[0].Decision.Disposition != DispositionApply {
			t.Fatalf("%s rows = %+v, want one apply", table, rows)
		}
	}
}

func TestRepeatedFailureOfOneClassIsRecordedOnce(t *testing.T) {
	router := newScriptRouter()
	custodian, ledger := newScriptCustodian(router, "orders")
	for i, message := range []string{lockTimeoutMessage, lockTimeoutMessage,
		"canceling statement due to lock timeout on relation orders"} {
		router.set("orders", lockTimeout(message))
		result, err := custodian.Scan(context.Background())
		if err == nil {
			t.Fatalf("scan %d: want the lock timeout returned", i+1)
		}
		if rows := len(ledger.since(0)); rows != 1 {
			t.Fatalf("scan %d: %d ledger rows, want 1 (result %+v)", i+1, rows, result)
		}
		if router.callsFor("orders") != i+1 {
			t.Fatalf("scan %d: %d routes, want %d", i+1, router.callsFor("orders"), i+1)
		}
	}
	reason := ledger.since(0)[0].Decision.Reason
	if !strings.Contains(reason, "(lock_timeout)") {
		t.Fatalf("reason = %q, want the lock_timeout class", reason)
	}
}

func TestFailureClassChangeIsRecorded(t *testing.T) {
	router := newScriptRouter()
	custodian, ledger := newScriptCustodian(router, "orders")
	router.set("orders", lockTimeout(lockTimeoutMessage))
	if _, err := custodian.Scan(context.Background()); err == nil {
		t.Fatal("first scan: want the lock timeout returned")
	}
	connErr := &pgconn.PgError{Code: "08006", Message: "connection failure"}
	router.set("orders", connErr)
	_, err := custodian.Scan(context.Background())
	if !errors.Is(err, connErr) {
		t.Fatalf("second scan error = %v, want the connection failure", err)
	}
	rows := ledger.since(0)
	if len(rows) != 2 || !strings.Contains(rows[0].Decision.Reason, "(lock_timeout)") ||
		!strings.Contains(rows[1].Decision.Reason, "(connection)") {
		t.Fatalf("rows = %+v, want lock_timeout then connection", rows)
	}
	if rows[0].Hash == rows[1].Hash || rows[0].Identity != rows[1].Identity {
		t.Fatalf("rows share a hash or differ in identity: %+v", rows)
	}
}

func TestRouteAndRecordErrorsOfAContinuedScanAreBothReturned(t *testing.T) {
	routeErr, recordErr := errors.New("route failure"), errors.New("record failure")
	router := newScriptRouter()
	router.set("first", routeErr)
	items := []Invariant{fkInvariant("public", "first", "fk"),
		fkInvariant("public", "second", "fk")}
	custodian := NewCustodian(&fakeDetector{items: items}, &targetContracts{},
		&fakeLedger{}, router, failingRecord{recordErr}, testPolicy())
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, routeErr) || !errors.Is(err, recordErr) {
		t.Fatalf("Scan error = %v, want both the route and the record error", err)
	}
	if router.callsFor("second") != 1 || result.Routed != 1 || result.Recorded != 0 {
		t.Fatalf("result=%+v calls=%v, want the second routed and nothing recorded",
			result, router.snapshot())
	}
	if !strings.Contains(err.Error(), "public.first") {
		t.Fatalf("Scan error = %q, want the failed target named", err)
	}
}

func TestConnectionFailureStopsTheScanAndKeepsSeen(t *testing.T) {
	router := newScriptRouter()
	custodian, ledger := newScriptCustodian(router, "first", "second", "third")
	if _, err := custodian.Scan(context.Background()); err != nil {
		t.Fatalf("healthy scan: %v", err)
	}
	connErr := &pgconn.PgError{Code: "08006", Message: "connection failure"}
	router.set("first", connErr)
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, connErr) || router.total() != 4 {
		t.Fatalf("err=%v calls=%v, want the scan stopped after the first route",
			err, router.snapshot())
	}
	if result.Recorded != 1 || len(rowsFor(ledger, "first")) != 2 {
		t.Fatalf("result=%+v, want only the failure recorded", result)
	}
	router.set("first", nil)
	healed, err := custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("healed scan: %v", err)
	}
	if healed.Recorded != 1 || healed.Unchanged != 2 || healed.Routed != 3 {
		t.Fatalf("healed scan = %+v, want first re-recorded, second and third unchanged",
			healed)
	}
	for _, table := range []string{"second", "third"} {
		if n := len(rowsFor(ledger, table)); n != 1 {
			t.Fatalf("%s has %d rows, want 1", table, n)
		}
	}
}

// After a restart (seen unknown) a connection failure on the first route
// must not make the unreached invariants look new on the next scan.
func TestConnectionFailureAfterRestartKeepsUnreachedTrusted(t *testing.T) {
	router := newScriptRouter()
	custodian, ledger := newScriptCustodian(router, "first", "second")
	if _, err := custodian.Scan(context.Background()); err != nil {
		t.Fatalf("healthy scan: %v", err)
	}
	restarted := NewCustodian(custodian.detector, custodian.contracts, ledger, router,
		ledger, testPolicy())
	router.set("first", &pgconn.PgError{Code: "08001", Message: "unable to connect"})
	if _, err := restarted.Scan(context.Background()); err == nil {
		t.Fatal("want the connection failure returned")
	}
	router.set("first", nil)
	healed, err := restarted.Scan(context.Background())
	if err != nil || healed.Unchanged != 1 || len(rowsFor(ledger, "second")) != 1 {
		t.Fatalf("healed=%+v err=%v rows(second)=%d, want second unchanged",
			healed, err, len(rowsFor(ledger, "second")))
	}
}

func TestRouteErrorClass(t *testing.T) {
	_, dialErr := pgconn.Connect(context.Background(),
		"host=127.0.0.1 port=1 user=nobody dbname=none connect_timeout=2 sslmode=disable")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"lock timeout", lockTimeout(lockTimeoutMessage), "lock_timeout"},
		{"wrapped lock timeout", fmt.Errorf("route: %w", lockTimeout("x")), "lock_timeout"},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, "statement_timeout"},
		{"connection failure", &pgconn.PgError{Code: "08006"}, "connection"},
		{"cannot connect", &pgconn.PgError{Code: "08001"}, "connection"},
		{"dial refused", dialErr, "connection"},
		{"net error", &net.OpError{Op: "read", Net: "tcp",
			Err: errors.New("connection reset by peer")}, "connection"},
		{"permission", &pgconn.PgError{Code: "42501"}, "permission"},
		{"unique violation", &pgconn.PgError{Code: "23505"}, "other"},
		{"deadline", context.DeadlineExceeded, "other"},
		{"plain", errors.New("verification unavailable"), "other"},
		{"nil", nil, "other"},
	}
	for _, tc := range cases {
		if got := routeErrorClass(tc.err); got != tc.want {
			t.Errorf("%s: routeErrorClass(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
	var connect *pgconn.ConnectError
	if !errors.As(dialErr, &connect) {
		t.Fatalf("dial to a closed port returned %T %v, want *pgconn.ConnectError",
			dialErr, dialErr)
	}
}
