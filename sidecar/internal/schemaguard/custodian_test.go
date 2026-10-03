package schemaguard

import (
	"context"
	"testing"
	"time"
)

func TestCustodianRoutesMissingFKIndexThroughVerifiedIndex(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
		ProposedSQL: "CREATE INDEX CONCURRENTLY orders_customer_idx ON orders(customer_id)",
	})
	fixture.policy.AllowFKIndexApply = true
	result, err := fixture.custodian().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Routed != 1 || len(fixture.router.items) != 1 {
		t.Fatalf("result=%#v routes=%#v", result, fixture.router.items)
	}
	item := fixture.router.items[0]
	if item.Decision.Route != RouteVerifyIndex || !item.Decision.RequiresVerification {
		t.Fatalf("route=%#v, want verified index", item)
	}
}

func TestCustodianRetentionFirstCycleIsDryRun(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantUnboundedAppend, Schema: "public", Table: "events",
		RetentionColumn: "created_at",
	})
	fixture.contract = TableContract{AppendOnly: true, RetentionWindow: 30 * 24 * time.Hour,
		RetentionColumn: "created_at"}
	fixture.policy.AllowRetentionApply = true
	_, err := fixture.custodian().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(fixture.router.items) != 1 ||
		fixture.router.items[0].Decision.Disposition != DispositionDryRun {
		t.Fatalf("routes=%#v, want exactly one dry run", fixture.router.items)
	}
	if fixture.router.items[0].Decision.MayDeleteData {
		t.Fatal("first retention cycle may delete data")
	}
}

func TestCustodianStructuralChangeNeverAutoApplies(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantTypeTightening, Schema: "public", Table: "users",
	})
	_, err := fixture.custodian().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(fixture.router.items) != 1 || len(fixture.records.items) != 1 {
		t.Fatalf("routes=%#v records=%#v", fixture.router.items, fixture.records.items)
	}
	decision := fixture.records.items[0].Decision
	if decision.Disposition != DispositionRecommend || !decision.RequiresRehearsal {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestCustodianExemptionParksWithoutRouting(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
	})
	fixture.contract.Exemptions = []InvariantKind{InvariantMissingFKIndex}
	_, err := fixture.custodian().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(fixture.router.items) != 0 ||
		fixture.records.items[0].Decision.Disposition != DispositionPark {
		t.Fatalf("routes=%#v records=%#v", fixture.router.items, fixture.records.items)
	}
}

func TestCustodianCancellationStopsBeforeRouting(t *testing.T) {
	fixture := newCustodianFixture(Invariant{
		Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fixture.custodian().Scan(ctx)
	if err == nil || len(fixture.router.items) != 0 {
		t.Fatalf("err=%v routes=%#v", err, fixture.router.items)
	}
}

type custodianFixture struct {
	detector  *fakeDetector
	contracts *fakeContractSource
	history   *fakeHistorySource
	router    *fakeRemediationRouter
	records   *fakeDecisionRecorder
	contract  TableContract
	policy    Policy
}

func newCustodianFixture(invariant Invariant) *custodianFixture {
	return &custodianFixture{
		detector:  &fakeDetector{items: []Invariant{invariant}},
		contracts: &fakeContractSource{}, history: &fakeHistorySource{},
		router: &fakeRemediationRouter{}, records: &fakeDecisionRecorder{},
		policy: DefaultPolicy(),
	}
}

func (f *custodianFixture) custodian() *Custodian {
	f.contracts.contract = f.contract
	return NewCustodian(f.detector, f.contracts, f.history, f.router, f.records, f.policy)
}

type fakeDetector struct{ items []Invariant }

func (d *fakeDetector) Detect(context.Context) ([]Invariant, error) {
	return append([]Invariant(nil), d.items...), nil
}

// fakeContractSource answers every batched lookup with one contract.
type fakeContractSource struct {
	contract TableContract
	calls    int
}

func (s *fakeContractSource) Contracts(
	_ context.Context, items []Invariant,
) (map[string]TableContract, error) {
	s.calls++
	result := make(map[string]TableContract, len(items))
	for _, item := range items {
		result[item.Target()] = s.contract
	}
	return result, nil
}

// fakeHistorySource answers every batched lookup with one history and no
// previously recorded decisions.
type fakeHistorySource struct {
	history History
	calls   int
}

func (s *fakeHistorySource) History(
	_ context.Context, items []Invariant,
) (HistoryIndex, error) {
	s.calls++
	index := HistoryIndex{ByTarget: map[HistoryKey]History{}, LastHash: map[string]string{}}
	for _, item := range items {
		index.ByTarget[HistoryKey{Kind: item.Kind, Target: item.Target()}] = s.history
	}
	return index, nil
}

type fakeRemediationRouter struct{ items []Remediation }

func (r *fakeRemediationRouter) Route(_ context.Context, item Remediation) error {
	r.items = append(r.items, item)
	return nil
}

type fakeDecisionRecorder struct {
	items   []Remediation
	records []DecisionRecord
}

func (r *fakeDecisionRecorder) Record(_ context.Context, record DecisionRecord) error {
	r.items = append(r.items, record.Remediation)
	r.records = append(r.records, record)
	return nil
}
