package schemaguard

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// migration-08: after routeFailureLimit consecutive failures of one
// identity its route is paused for routePauseScans scans; a success resets
// the count, and a restart forgets it.

const lockTimeoutMessage = "canceling statement due to lock timeout"

func lockTimeout(message string) error {
	return &pgconn.PgError{Code: "55P03", Message: message}
}

// scriptRouter fails the route of each table in fail with its error and
// counts every call per table.
type scriptRouter struct {
	mu    sync.Mutex
	fail  map[string]error
	calls map[string]int
	delay time.Duration
}

func newScriptRouter() *scriptRouter {
	return &scriptRouter{fail: map[string]error{}, calls: map[string]int{}}
}

func (r *scriptRouter) Route(_ context.Context, item Remediation) error {
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[item.Invariant.Table]++
	return r.fail[item.Invariant.Table]
}

func (r *scriptRouter) set(table string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail[table] = err
}

func (r *scriptRouter) callsFor(table string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[table]
}

func (r *scriptRouter) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]int, len(r.calls))
	for table, n := range r.calls {
		result[table] = n
	}
	return result
}

func (r *scriptRouter) total() int {
	total := 0
	for _, n := range r.snapshot() {
		total += n
	}
	return total
}

func newScriptCustodian(router *scriptRouter, tables ...string) (*Custodian, *fakeLedger) {
	items := make([]Invariant, 0, len(tables))
	for _, table := range tables {
		items = append(items, fkInvariant("public", table, table+"_fk"))
	}
	ledger := &fakeLedger{}
	return NewCustodian(&fakeDetector{items: items}, &targetContracts{}, ledger, router,
		ledger, testPolicy()), ledger
}

func rowsFor(ledger *fakeLedger, table string) []DecisionRecord {
	var rows []DecisionRecord
	for _, row := range ledger.since(0) {
		if row.Invariant.Table == table {
			rows = append(rows, row)
		}
	}
	return rows
}

// scanTimes runs n scans and returns the last one's error.
func scanTimes(t *testing.T, custodian *Custodian, n int) error {
	t.Helper()
	var err error
	for i := 0; i < n; i++ {
		_, err = custodian.Scan(context.Background())
	}
	return err
}

func TestRoutingPausesAfterThreeConsecutiveFailures(t *testing.T) {
	router := newScriptRouter()
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, ledger := newScriptCustodian(router, "orders", "items")
	if err := scanTimes(t, custodian, 3); err == nil {
		t.Fatal("third failing scan: want the lock timeout returned")
	}
	result, err := custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("paused scan returned %v; a pause is not a failure", err)
	}
	if router.callsFor("orders") != 3 || router.callsFor("items") != 4 {
		t.Fatalf("calls = %v, want orders paused and items routed", router.snapshot())
	}
	if result.Routed != 1 || result.Recorded != 1 {
		t.Fatalf("paused scan = %+v, want items routed and the pause recorded", result)
	}
	rows := rowsFor(ledger, "orders")
	last := rows[len(rows)-1].Decision
	if len(rows) != 2 || last.Disposition != DispositionPark || last.AutoApply ||
		last.Reason != "routing paused after 3 failures (lock_timeout)" {
		t.Fatalf("orders rows = %+v, want the failure then the pause", rows)
	}
	if err := scanTimes(t, custodian, routePauseScans-1); err != nil {
		t.Fatalf("paused scans: %v", err)
	}
	if router.callsFor("orders") != 3 || len(rowsFor(ledger, "orders")) != 2 {
		t.Fatalf("orders routed %d times with %d rows during the pause, want 3 and 2",
			router.callsFor("orders"), len(rowsFor(ledger, "orders")))
	}
	if err := scanTimes(t, custodian, 1); err == nil || router.callsFor("orders") != 4 {
		t.Fatalf("scan after the pause: err=%v calls=%d, want orders routed and failing",
			err, router.callsFor("orders"))
	}
	// Still failing after the pause: paused again at once.
	if err := scanTimes(t, custodian, 1); err != nil || router.callsFor("orders") != 4 {
		t.Fatalf("scan after a failed retry: err=%v calls=%d, want paused again",
			err, router.callsFor("orders"))
	}
}

func TestSuccessAfterThePauseResetsTheFailureCount(t *testing.T) {
	router := newScriptRouter()
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, ledger := newScriptCustodian(router, "orders")
	scanTimes(t, custodian, routeFailureLimit+routePauseScans)
	router.set("orders", nil)
	if err := scanTimes(t, custodian, 1); err != nil || router.callsFor("orders") != 4 {
		t.Fatalf("healed scan: err=%v calls=%d, want routed once more", err,
			router.callsFor("orders"))
	}
	rows := rowsFor(ledger, "orders")
	if last := rows[len(rows)-1].Decision; last.Disposition != DispositionApply {
		t.Fatalf("last orders row = %+v, want the apply recorded", last)
	}
	router.set("orders", lockTimeout(lockTimeoutMessage))
	// Two failures after a success are below the limit: still routed.
	scanTimes(t, custodian, routeFailureLimit-1)
	if err := scanTimes(t, custodian, 1); err == nil ||
		router.callsFor("orders") != 4+routeFailureLimit {
		t.Fatalf("calls=%d err=%v, want %d routes with the count reset",
			router.callsFor("orders"), err, 4+routeFailureLimit)
	}
	if err := scanTimes(t, custodian, 1); err != nil ||
		router.callsFor("orders") != 4+routeFailureLimit {
		t.Fatalf("calls=%d err=%v, want paused after %d new failures",
			router.callsFor("orders"), err, routeFailureLimit)
	}
}

func TestTwoFailuresThenSuccessNeverPause(t *testing.T) {
	router := newScriptRouter()
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, _ := newScriptCustodian(router, "orders")
	scanTimes(t, custodian, routeFailureLimit-1)
	router.set("orders", nil)
	scanTimes(t, custodian, 1)
	router.set("orders", lockTimeout(lockTimeoutMessage))
	scanTimes(t, custodian, routeFailureLimit-1)
	if got, want := router.callsFor("orders"), 2*routeFailureLimit-1; got != want {
		t.Fatalf("orders routed %d times, want %d: the count is consecutive", got, want)
	}
}

func TestRestartForgetsTheRoutePause(t *testing.T) {
	router := newScriptRouter()
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, ledger := newScriptCustodian(router, "orders")
	scanTimes(t, custodian, routeFailureLimit+1)
	restarted := NewCustodian(custodian.detector, custodian.contracts, ledger, router,
		ledger, testPolicy())
	if err := scanTimes(t, restarted, 1); err == nil ||
		router.callsFor("orders") != routeFailureLimit+1 {
		t.Fatalf("restarted scan: err=%v calls=%d, want routed again",
			err, router.callsFor("orders"))
	}
	rows := rowsFor(ledger, "orders")
	if last := rows[len(rows)-1].Decision; !strings.Contains(last.Reason, "routing failed") {
		t.Fatalf("last row = %+v, want the failure recorded again after the pause", last)
	}
}

// Two parallel scans must not both record from one stale history.
func TestParallelScansRecordNoDuplicateRows(t *testing.T) {
	router := newScriptRouter()
	router.delay = 20 * time.Millisecond
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, ledger := newScriptCustodian(router, "orders", "items")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = custodian.Scan(context.Background())
		}()
	}
	wg.Wait()
	for _, table := range []string{"orders", "items"} {
		if n := len(rowsFor(ledger, table)); n != 1 {
			t.Fatalf("%s has %d rows after two parallel scans, want 1", table, n)
		}
	}
	if router.callsFor("orders") != 2 || router.callsFor("items") != 2 {
		t.Fatalf("calls = %v, want each routed by both scans", router.snapshot())
	}
}

// An invariant that stops being detected is forgotten: when it comes back
// it is routed at once, not still paused.
func TestDisappearedInvariantForgetsItsPause(t *testing.T) {
	router := newScriptRouter()
	router.set("orders", lockTimeout(lockTimeoutMessage))
	custodian, _ := newScriptCustodian(router, "orders")
	detector := custodian.detector.(*fakeDetector)
	scanTimes(t, custodian, routeFailureLimit)
	items := detector.items
	detector.items = nil
	scanTimes(t, custodian, 1)
	detector.items = items
	if err := scanTimes(t, custodian, 1); err == nil ||
		router.callsFor("orders") != routeFailureLimit+1 {
		t.Fatalf("reappeared scan: err=%v calls=%d, want routed again",
			err, router.callsFor("orders"))
	}
}

// A connection failure inside a clone family leaves members unreached: the
// family's partial outcome is not recorded, so no false change is written.
func TestConnectionFailureSkipsAPartlyReachedFamily(t *testing.T) {
	family := &Family{Key: "tenant", Members: []string{"tenant_a", "tenant_b"}}
	first := fkInvariant("tenant_a", "orders", "orders_fk")
	second := fkInvariant("tenant_b", "orders", "orders_fk")
	first.Family, second.Family = family, family
	router := &schemaFailRouter{fail: map[string]error{"tenant_a": &pgconn.PgError{
		Code: "08006", Message: "connection failure"}}}
	ledger := &fakeLedger{}
	custodian := NewCustodian(&fakeDetector{items: []Invariant{first, second}},
		&targetContracts{}, ledger, router, ledger, testPolicy())
	result, err := custodian.Scan(context.Background())
	if err == nil || result.Recorded != 0 || ledger.count() != 0 || router.calls != 1 {
		t.Fatalf("result=%+v err=%v rows=%d calls=%d, want the family unrecorded",
			result, err, ledger.count(), router.calls)
	}
	router.fail = nil
	healed, err := custodian.Scan(context.Background())
	if err != nil || healed.Recorded != 1 || ledger.count() != 1 ||
		len(ledger.since(0)[0].Targets) != 2 {
		t.Fatalf("healed=%+v err=%v rows=%+v, want one row covering both members",
			healed, err, ledger.since(0))
	}
}

// schemaFailRouter fails the routes of the schemas in fail.
type schemaFailRouter struct {
	fail  map[string]error
	calls int
}

func (r *schemaFailRouter) Route(_ context.Context, item Remediation) error {
	r.calls++
	return r.fail[item.Invariant.Schema]
}
