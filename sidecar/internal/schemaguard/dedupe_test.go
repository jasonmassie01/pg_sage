package schemaguard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLedger is the history source and the decision recorder at once, the
// way sage.decision is: what one scan records is what the next one reads.
type fakeLedger struct {
	mu      sync.Mutex
	records []DecisionRecord
	calls   int
	err     error
}

func (l *fakeLedger) Record(_ context.Context, record DecisionRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	record.Targets = append([]string(nil), record.Targets...)
	l.records = append(l.records, record)
	return nil
}

func (l *fakeLedger) History(_ context.Context, _ []Invariant) (HistoryIndex, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	index := HistoryIndex{ByTarget: map[HistoryKey]History{}, LastHash: map[string]string{}}
	for _, record := range l.records {
		index.LastHash[record.Identity] = record.Hash
		if record.Decision.Disposition != DispositionDryRun {
			continue
		}
		for _, target := range record.Targets {
			key := HistoryKey{Kind: record.Invariant.Kind, Target: target}
			history := index.ByTarget[key]
			history.SuccessfulRetentionDryRuns++
			index.ByTarget[key] = history
		}
	}
	return index, nil
}

func (l *fakeLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

func (l *fakeLedger) since(start int) []DecisionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]DecisionRecord(nil), l.records[start:]...)
}

// targetContracts answers per target; a missing target has no contract.
type targetContracts struct {
	mu        sync.Mutex
	contracts map[string]TableContract
	calls     int
}

func (s *targetContracts) Contracts(
	_ context.Context, items []Invariant,
) (map[string]TableContract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	result := map[string]TableContract{}
	for _, item := range items {
		if contract, ok := s.contracts[item.Target()]; ok {
			result[item.Target()] = contract
		}
	}
	return result, nil
}

type countingRouter struct {
	mu    sync.Mutex
	items []Remediation
}

func (r *countingRouter) Route(_ context.Context, item Remediation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
	return nil
}

func (r *countingRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

type dedupeFixture struct {
	detector  *fakeDetector
	contracts *targetContracts
	ledger    *fakeLedger
	router    *countingRouter
	custodian *Custodian
}

func newDedupeFixture(items ...Invariant) *dedupeFixture {
	f := &dedupeFixture{
		detector:  &fakeDetector{items: items},
		contracts: &targetContracts{contracts: map[string]TableContract{}},
		ledger:    &fakeLedger{}, router: &countingRouter{},
	}
	f.custodian = f.restart()
	return f
}

// restart builds a fresh custodian (a sidecar restart) on the same ledger.
func (f *dedupeFixture) restart() *Custodian {
	return NewCustodian(f.detector, f.contracts, f.ledger, f.router, f.ledger, testPolicy())
}

func (f *dedupeFixture) scan(t *testing.T) CycleResult {
	t.Helper()
	result, err := f.custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return result
}

func fkInvariant(schema, table, constraint string) Invariant {
	return Invariant{
		Kind: InvariantMissingFKIndex, Schema: schema, Table: table, Subject: constraint,
		ProposedSQL: fmt.Sprintf(`CREATE INDEX CONCURRENTLY "%s_idx" ON "%s"."%s" (c)`,
			constraint, schema, table),
	}
}

func TestUnchangedDecisionIsRecordedOnceButStillRouted(t *testing.T) {
	f := newDedupeFixture(
		fkInvariant("public", "orders", "orders_customer_fk"),
		fkInvariant("public", "orders", "orders_store_fk"),
		Invariant{Kind: InvariantTypeTightening, Schema: "public", Table: "users",
			Subject: "account_id", ProposedSQL: "ALTER TABLE users ALTER COLUMN account_id"},
	)
	first := f.scan(t)
	if first.Detected != 3 || first.Recorded != 3 || first.Unchanged != 0 || first.Routed != 3 {
		t.Fatalf("first scan = %+v, want 3 detected, routed and recorded", first)
	}
	second := f.scan(t)
	if second.Recorded != 0 || second.Unchanged != 3 || second.Routed != 3 {
		t.Fatalf("second scan = %+v, want 0 recorded, 3 unchanged, 3 routed", second)
	}
	if f.ledger.count() != 3 || f.router.count() != 6 {
		t.Fatalf("ledger rows=%d routes=%d, want 3 rows and 6 routes",
			f.ledger.count(), f.router.count())
	}
}

func TestSubjectDistinguishesInvariantsOnOneTable(t *testing.T) {
	left := fkInvariant("public", "orders", "orders_customer_fk")
	right := fkInvariant("public", "orders", "orders_store_fk")
	if InvariantIdentity(left) == InvariantIdentity(right) {
		t.Fatal("two foreign keys on one table share an identity")
	}
	other := left
	other.Schema = "archive"
	if InvariantIdentity(left) == InvariantIdentity(other) {
		t.Fatal("the same constraint in two unrelated schemas shares an identity")
	}
	kind := left
	kind.Kind = InvariantRedundantIndex
	if InvariantIdentity(left) == InvariantIdentity(kind) {
		t.Fatal("two invariant kinds share an identity")
	}
	if InvariantIdentity(left) != InvariantIdentity(fkInvariant("public", "orders",
		"orders_customer_fk")) {
		t.Fatal("identity is not stable for the same invariant")
	}
}

func TestChangedDecisionIsRecordedAndRoutingFollowsIt(t *testing.T) {
	item := fkInvariant("public", "orders", "orders_customer_fk")
	f := newDedupeFixture(item)
	f.scan(t)
	f.contracts.contracts["public.orders"] = TableContract{
		Exemptions: []InvariantKind{InvariantMissingFKIndex}}
	parked := f.scan(t)
	if parked.Recorded != 1 || parked.Routed != 0 {
		t.Fatalf("exempted scan = %+v, want the park recorded and nothing routed", parked)
	}
	last := f.ledger.since(1)[0]
	if last.Decision.Disposition != DispositionPark ||
		last.Decision.Reason != "table contract exemption" {
		t.Fatalf("recorded change = %+v, want the exemption park", last.Decision)
	}
	delete(f.contracts.contracts, "public.orders")
	back := f.scan(t)
	if back.Recorded != 1 || back.Routed != 1 {
		t.Fatalf("un-exempted scan = %+v, want the apply recorded and routed again", back)
	}
	if got := f.ledger.since(2)[0].Decision.Disposition; got != DispositionApply {
		t.Fatalf("re-recorded disposition = %q, want apply", got)
	}
}

func TestReappearedInvariantIsRecordedAgain(t *testing.T) {
	item := fkInvariant("public", "orders", "orders_customer_fk")
	f := newDedupeFixture(item)
	f.scan(t)
	f.detector.items = nil
	if gone := f.scan(t); gone.Recorded != 0 || gone.Detected != 0 {
		t.Fatalf("scan without the invariant = %+v", gone)
	}
	f.detector.items = []Invariant{item}
	back := f.scan(t)
	if back.Recorded != 1 || f.ledger.count() != 2 {
		t.Fatalf("reappeared scan = %+v rows=%d, want a fresh record", back, f.ledger.count())
	}
}

func TestRestartDoesNotRerecordUnchangedDecisions(t *testing.T) {
	f := newDedupeFixture(fkInvariant("public", "orders", "orders_customer_fk"),
		fkInvariant("public", "items", "items_order_fk"))
	f.scan(t)
	f.custodian = f.restart()
	result := f.scan(t)
	if result.Recorded != 0 || result.Unchanged != 2 || f.ledger.count() != 2 {
		t.Fatalf("after restart = %+v rows=%d, want nothing re-recorded",
			result, f.ledger.count())
	}
}

func TestRetentionDryRunThenApplyAreRecordedChanges(t *testing.T) {
	item := Invariant{Kind: InvariantUnboundedAppend, Schema: "public", Table: "events",
		RetentionColumn: "created_at"}
	f := newDedupeFixture(item)
	f.contracts.contracts["public.events"] = TableContract{AppendOnly: true,
		RetentionWindow: 30 * 24 * time.Hour, RetentionColumn: "created_at"}
	f.scan(t)
	f.scan(t)
	third := f.scan(t)
	want := []Disposition{DispositionDryRun, DispositionApply, DispositionApply}
	for i, item := range f.router.items {
		if i >= len(want) || item.Decision.Disposition != want[i] {
			t.Fatalf("routes = %+v, want dry run then apply each cycle", f.router.items)
		}
	}
	if len(f.router.items) != 3 || f.ledger.count() != 2 || third.Recorded != 0 {
		t.Fatalf("routes=%d rows=%d third=%+v, want 3 routes and 2 recorded changes",
			len(f.router.items), f.ledger.count(), third)
	}
}

func TestLookupsAreBatchedOncePerScan(t *testing.T) {
	items := make([]Invariant, 0, 50)
	for i := 0; i < 50; i++ {
		items = append(items, fkInvariant("public", fmt.Sprintf("t%02d", i), "fk"))
	}
	f := newDedupeFixture(items...)
	f.scan(t)
	if f.contracts.calls != 1 || f.ledger.calls != 1 {
		t.Fatalf("contract lookups=%d history lookups=%d for 50 invariants, want 1 each",
			f.contracts.calls, f.ledger.calls)
	}
}

func TestEmptyScanDoesNoLookupsOrRecords(t *testing.T) {
	f := newDedupeFixture()
	result := f.scan(t)
	if result != (CycleResult{}) || f.contracts.calls != 0 || f.ledger.calls != 0 {
		t.Fatalf("empty scan = %+v contracts=%d history=%d", result, f.contracts.calls,
			f.ledger.calls)
	}
}

type failingContracts struct{ err error }

func (f failingContracts) Contracts(context.Context, []Invariant) (
	map[string]TableContract, error,
) {
	return nil, f.err
}

type failingHistory struct{ err error }

func (f failingHistory) History(context.Context, []Invariant) (HistoryIndex, error) {
	return HistoryIndex{}, f.err
}

func TestBatchedLookupErrorsAreDistinguishable(t *testing.T) {
	cause := errors.New("connection refused")
	detector := &fakeDetector{items: []Invariant{fkInvariant("public", "orders", "fk")}}
	contracts := NewCustodian(detector, failingContracts{cause}, &fakeLedger{},
		&countingRouter{}, &fakeLedger{}, testPolicy())
	_, err := contracts.Scan(context.Background())
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "table contracts") {
		t.Fatalf("contract failure = %v, want it named and wrapped", err)
	}
	history := NewCustodian(detector, &targetContracts{}, failingHistory{cause},
		&countingRouter{}, &fakeLedger{}, testPolicy())
	_, err = history.Scan(context.Background())
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "remediation history") {
		t.Fatalf("history failure = %v, want it named and wrapped", err)
	}
}

type failSecondRoute struct {
	countingRouter
	err error
}

func (r *failSecondRoute) Route(ctx context.Context, item Remediation) error {
	if item.Invariant.Table == "second" {
		return r.err
	}
	return r.countingRouter.Route(ctx, item)
}

func TestRouteFailureStillRecordsEarlierOutcomes(t *testing.T) {
	routeErr := errors.New("verification unavailable")
	ledger := &fakeLedger{}
	detector := &fakeDetector{items: []Invariant{fkInvariant("public", "first", "fk"),
		fkInvariant("public", "second", "fk"), fkInvariant("public", "third", "fk")}}
	custodian := NewCustodian(detector, &targetContracts{}, ledger,
		&failSecondRoute{err: routeErr}, ledger, testPolicy())
	result, err := custodian.Scan(context.Background())
	if !errors.Is(err, routeErr) || result.Recorded != 2 || ledger.count() != 2 {
		t.Fatalf("result=%+v rows=%d err=%v, want the first outcome and the failure",
			result, ledger.count(), err)
	}
	tables := []string{}
	for _, row := range ledger.since(0) {
		tables = append(tables, row.Invariant.Table)
	}
	sort.Strings(tables)
	if strings.Join(tables, ",") != "first,second" {
		t.Fatalf("recorded tables = %v, want first and second", tables)
	}
}

func TestRecordsKeepDetectionOrder(t *testing.T) {
	f := newDedupeFixture(fkInvariant("public", "zeta", "fk"),
		fkInvariant("public", "alpha", "fk"), fkInvariant("public", "mid", "fk"))
	f.scan(t)
	got := []string{}
	for _, row := range f.ledger.since(0) {
		got = append(got, row.Invariant.Table)
	}
	if strings.Join(got, ",") != "zeta,alpha,mid" {
		t.Fatalf("record order = %v, want detection order", got)
	}
}

func TestConcurrentScansShareStateSafely(t *testing.T) {
	f := newDedupeFixture(fkInvariant("public", "orders", "fk"),
		fkInvariant("public", "items", "fk"))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.custodian.Scan(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Scan: %v", err)
	}
	identities := map[string]bool{}
	for _, row := range f.ledger.since(0) {
		identities[row.Identity] = true
	}
	if len(identities) != 2 || f.router.count() != 16 {
		t.Fatalf("identities=%d routes=%d, want 2 identities and 16 routes",
			len(identities), f.router.count())
	}
	if result := f.scan(t); result.Recorded != 0 {
		t.Fatalf("scan after concurrent scans re-recorded: %+v", result)
	}
}

// reasonRouter parks every route with a reason the test sets.
type reasonRouter struct{ reason string }

func (r *reasonRouter) Route(context.Context, Remediation) error {
	return &ParkedRoute{Reason: r.reason}
}

// A park whose reason changes (a new review deadline, a new blocker) is a
// changed decision even when route and disposition are the same.
func TestChangedReasonAloneIsRecorded(t *testing.T) {
	ledger := &fakeLedger{}
	router := &reasonRouter{reason: "retention dry run in review until 10:00"}
	detector := &fakeDetector{items: []Invariant{fkInvariant("public", "orders", "fk")}}
	custodian := NewCustodian(detector, &targetContracts{}, ledger, router, ledger,
		testPolicy())
	for i, reason := range []string{"", "", "retention dry run in review until 11:00"} {
		if reason != "" {
			router.reason = reason
		}
		if _, err := custodian.Scan(context.Background()); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	rows := ledger.since(0)
	if len(rows) != 2 || rows[1].Decision.Reason != "retention dry run in review until 11:00" {
		t.Fatalf("rows = %+v, want the first park and the changed reason", rows)
	}
}
