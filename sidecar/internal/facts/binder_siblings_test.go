package facts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-02: a slot is a cluster object, so the confirmed slot_consumer
// facts of the sibling databases on the same cluster bind a request that
// touches slots. Other fact types never cross databases.

const walBoundSQL = "ALTER SYSTEM SET max_slot_wal_keep_size = '1024MB'"

func walBoundRequest(sql string, targets ...string) policy.ActionRequest {
	return policy.ActionRequest{SQL: sql, TargetObjs: targets}
}

type siblingList struct {
	dbs   []SiblingDatabase
	calls int
}

func (s *siblingList) source() SiblingSource {
	return func() []SiblingDatabase {
		s.calls++
		return s.dbs
	}
}

func siblingFacts() *fakeConfirmed {
	return &fakeConfirmed{facts: []Fact{
		confirmed(7, TypeSlotConsumer, KindSlot, "cdc_*", map[string]string{
			"consumer": "debezium"}),
		confirmed(8, TypeAppMigrations, KindTable, "public.orders", nil),
		confirmed(6, TypeTestFixture, KindSchema, "test_*", nil),
	}}
}

func siblingBinder(local *fakeConfirmed, sibs *siblingList) *Binder {
	return NewBinder(local, &fakeResolver{}, func() time.Time { return bindNow }).
		WithSiblings(sibs.source())
}

func TestBinderSiblingSlotConsumerFactBindsClusterWideBound(t *testing.T) {
	sibs := &siblingList{dbs: []SiblingDatabase{{Database: "orders_a",
		Facts: siblingFacts()}}}
	b := siblingBinder(&fakeConfirmed{}, sibs)
	for _, targets := range [][]string{{"slot:old_replica"}, {"instance"}, nil} {
		got, err := b.Bind(context.Background(), walBoundRequest(walBoundSQL, targets...))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].FactID != 7 || got[0].Database != "orders_a" ||
			got[0].Route != string(RouteAlert) || got[0].Object != "slot:*" ||
			got[0].ConfirmedBy != "alice@example.com" {
			t.Fatalf("targets %v: bindings %+v, want sibling fact 7 of orders_a", targets, got)
		}
	}
	got, err := b.Bind(context.Background(), walBoundRequest(
		"ALTER SYSTEM SET max_slot_wal_keep_size = -1", "slot:old_replica"))
	if err != nil || len(got) != 0 {
		t.Fatalf("an unbound is not blocked by a sibling: %+v %v", got, err)
	}
}

func TestBinderSiblingSlotConsumerOtherTypesNeverCross(t *testing.T) {
	sibs := &siblingList{dbs: []SiblingDatabase{{Database: "orders_a",
		Facts: siblingFacts()}}}
	b := siblingBinder(&fakeConfirmed{}, sibs)
	for _, req := range []policy.ActionRequest{
		policyRequest("DROP INDEX CONCURRENTLY public.idx_orders", "public.orders"),
		policyRequest("VACUUM test_abc.events", "test_abc.events"),
	} {
		got, err := b.Bind(context.Background(), req)
		if err != nil || len(got) != 0 {
			t.Fatalf("%q: a sibling's %+v bound here (%v)", req.SQL, got, err)
		}
	}
	if sibs.calls != 0 {
		t.Fatalf("siblings read %d times for requests touching no slot", sibs.calls)
	}
	// A slot-touching request reads only the sibling's slot facts.
	got, err := b.Bind(context.Background(), walBoundRequest(walBoundSQL, "public.orders"))
	if err != nil || len(got) != 1 || got[0].FactID != 7 {
		t.Fatalf("slot request with a table target: %+v %v", got, err)
	}
}

func TestBinderSiblingSlotConsumerLocalAndSiblingBothReported(t *testing.T) {
	local := &fakeConfirmed{facts: []Fact{confirmed(7, TypeSlotConsumer, KindSlot,
		"cdc_orders", map[string]string{"consumer": "fivetran"})}}
	sibs := &siblingList{dbs: []SiblingDatabase{
		{Database: "orders_b", Facts: siblingFacts()},
		{Database: "orders_a", Facts: &fakeConfirmed{facts: []Fact{confirmed(2,
			TypeSlotConsumer, KindSlot, "airbyte_*", nil)}}},
	}}
	got, err := siblingBinder(local, sibs).Bind(context.Background(),
		walBoundRequest("SELECT pg_drop_replication_slot('cdc_orders')", "slot:cdc_orders"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].FactID != 7 || got[0].Database != "" ||
		got[1].FactID != 7 || got[1].Database != "orders_b" {
		t.Fatalf("bindings %+v, want local fact 7 then orders_b's fact 7", got)
	}
	detail := policy.FactDetail(got)
	if !strings.Contains(detail, `fact #7 of database "orders_b"`) ||
		!strings.HasPrefix(detail, "fact #7: ") {
		t.Fatalf("detail %q must tell the local fact from the sibling's", detail)
	}
}

func TestBinderSiblingSlotConsumerErrorNamesTheSibling(t *testing.T) {
	boom := errors.New("connection refused")
	sibs := &siblingList{dbs: []SiblingDatabase{
		{Database: "orders_a", Facts: siblingFacts()},
		{Database: "orders_c", Facts: &fakeConfirmed{err: boom}},
	}}
	_, err := siblingBinder(&fakeConfirmed{}, sibs).Bind(context.Background(),
		walBoundRequest(walBoundSQL, "instance"))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `"orders_c"`) {
		t.Fatalf("err = %v, want the wrapped sibling error naming orders_c", err)
	}
	// A request touching no slot never reads the siblings, so it is not
	// failed by them.
	got, err := siblingBinder(&fakeConfirmed{}, sibs).Bind(context.Background(),
		policyRequest("DROP INDEX public.i", "public.i"))
	if err != nil || got != nil {
		t.Fatalf("no slot ref: %+v %v", got, err)
	}
}

func TestBinderSiblingSlotConsumerNilAndEmpty(t *testing.T) {
	for name, b := range map[string]*Binder{
		"nil source": NewBinder(&fakeConfirmed{}, nil, nil).WithSiblings(nil),
		"no siblings": NewBinder(&fakeConfirmed{}, nil, nil).WithSiblings(
			(&siblingList{}).source()),
		"never set": NewBinder(&fakeConfirmed{}, nil, nil),
		"sibling without facts": NewBinder(&fakeConfirmed{}, nil, nil).WithSiblings(
			(&siblingList{dbs: []SiblingDatabase{{Database: "x",
				Facts: &fakeConfirmed{}}}}).source()),
	} {
		got, err := b.Bind(context.Background(), walBoundRequest(walBoundSQL, "instance"))
		if err != nil || got != nil {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	expired := siblingFacts()
	past := bindNow.Add(-time.Minute)
	expired.facts[0].ExpiresAt = &past
	b := siblingBinder(&fakeConfirmed{}, &siblingList{dbs: []SiblingDatabase{{
		Database: "orders_a", Facts: expired}}})
	if got, err := b.Bind(context.Background(), walBoundRequest(walBoundSQL)); err != nil ||
		len(got) != 0 {
		t.Fatalf("an expired sibling fact bound: %+v %v", got, err)
	}
}

// The sibling list is read on every request, so a database that joins or
// leaves the cluster is seen at once.
func TestBinderSiblingSlotConsumerListReadPerRequest(t *testing.T) {
	sibs := &siblingList{}
	b := siblingBinder(&fakeConfirmed{}, sibs)
	if got, _ := b.Bind(context.Background(), walBoundRequest(walBoundSQL)); len(got) != 0 {
		t.Fatalf("no sibling yet: %+v", got)
	}
	sibs.dbs = []SiblingDatabase{{Database: "orders_a", Facts: siblingFacts()}}
	if got, _ := b.Bind(context.Background(), walBoundRequest(walBoundSQL)); len(got) != 1 {
		t.Fatalf("after the sibling joined: %+v", got)
	}
	if sibs.calls != 2 {
		t.Fatalf("sibling list read %d times, want 2", sibs.calls)
	}
}
