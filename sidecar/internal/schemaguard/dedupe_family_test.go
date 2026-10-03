package schemaguard

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Clone families in the change-only ledger: a live family is remediated
// per member and recorded once per identity; an idle leftover is parked.

func familyMembers(n int) []string {
	members := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		members = append(members, fmt.Sprintf("tenant_%06d", i))
	}
	return members
}

func familyInvariants(family *Family, table, constraint string) []Invariant {
	items := make([]Invariant, 0, len(family.Members))
	for _, schema := range family.Members {
		item := fkInvariant(schema, table, constraint)
		item.Family = family
		items = append(items, item)
	}
	return items
}

func TestLiveFamilyRecordsOneFannedOutDecisionAndRoutesEveryMember(t *testing.T) {
	family := &Family{Key: "tenant_*:0a1b2c3d", Members: familyMembers(5)}
	f := newDedupeFixture(familyInvariants(family, "orders", "orders_customer_fk")...)
	first := f.scan(t)
	if first.Routed != 5 || first.Recorded != 1 || f.router.count() != 5 {
		t.Fatalf("live family scan = %+v routes=%d, want 5 routes and 1 record",
			first, f.router.count())
	}
	record := f.ledger.since(0)[0]
	want := []string{"tenant_000001.orders", "tenant_000002.orders", "tenant_000003.orders",
		"tenant_000004.orders", "tenant_000005.orders"}
	if strings.Join(record.Targets, ",") != strings.Join(want, ",") {
		t.Fatalf("fanned-out targets = %v, want %v", record.Targets, want)
	}
	if record.Invariant.Family == nil || record.Invariant.Family.Key != family.Key ||
		record.Decision.Disposition != DispositionApply {
		t.Fatalf("family record = %+v", record)
	}
	if second := f.scan(t); second.Recorded != 0 || second.Routed != 5 {
		t.Fatalf("unchanged live family = %+v, want 0 recorded, 5 routed", second)
	}
}

func TestLiveFamilyWithMixedOutcomesRecordsEveryMember(t *testing.T) {
	family := &Family{Key: "tenant_*:0a1b2c3d", Members: familyMembers(5)}
	f := newDedupeFixture(familyInvariants(family, "orders", "orders_customer_fk")...)
	f.contracts.contracts["tenant_000003.orders"] = TableContract{
		Exemptions: []InvariantKind{InvariantMissingFKIndex}}
	first := f.scan(t)
	rows := f.ledger.since(0)
	if first.Recorded != 2 || len(rows) != 2 || first.Routed != 4 {
		t.Fatalf("mixed family = %+v rows=%d, want 2 rows and 4 routes", first, len(rows))
	}
	covered := map[string]Disposition{}
	for _, row := range rows {
		if row.Identity != rows[0].Identity || row.Hash != rows[0].Hash {
			t.Fatalf("rows of one identity disagree: %+v", rows)
		}
		for _, target := range row.Targets {
			covered[target] = row.Decision.Disposition
		}
	}
	if len(covered) != 5 || covered["tenant_000003.orders"] != DispositionPark ||
		covered["tenant_000001.orders"] != DispositionApply {
		t.Fatalf("covered = %v, want 4 apply and the exempted member parked", covered)
	}
	if second := f.scan(t); second.Recorded != 0 {
		t.Fatalf("unchanged mixed family re-recorded: %+v", second)
	}
}

func TestFamilyMembershipChangeIsRecorded(t *testing.T) {
	family := &Family{Key: "tenant_*:0a1b2c3d", Members: familyMembers(5)}
	f := newDedupeFixture(familyInvariants(family, "orders", "orders_customer_fk")...)
	f.scan(t)
	grown := &Family{Key: family.Key, Members: familyMembers(6)}
	f.detector.items = familyInvariants(grown, "orders", "orders_customer_fk")
	result := f.scan(t)
	rows := f.ledger.since(1)
	if result.Recorded != 1 || len(rows) != 1 || len(rows[0].Targets) != 6 {
		t.Fatalf("grown family = %+v rows=%+v, want one record covering 6 schemas",
			result, rows)
	}
}

func TestIdleFamilyIsParkedOnceAndNeverRouted(t *testing.T) {
	family := &Family{Key: "test_memory_*:99aa00bb", Members: familyMembers(5), Idle: true,
		Reason: "no scans or writes since the statistics reset"}
	items := familyInvariants(family, "orders", "orders_customer_fk")
	for _, schema := range family.Members {
		items = append(items, Invariant{Kind: InvariantTypeTightening, Schema: schema,
			Table: "users", Subject: "account_id", Family: family})
	}
	f := newDedupeFixture(items...)
	first := f.scan(t)
	if first.Routed != 0 || f.router.count() != 0 || first.Skipped != 10 ||
		first.Recorded != 2 {
		t.Fatalf("idle family = %+v routes=%d, want 0 routes, 10 skipped, 2 records",
			first, f.router.count())
	}
	for _, row := range f.ledger.since(0) {
		if row.Decision.Disposition != DispositionPark || len(row.Targets) != 5 ||
			!strings.Contains(row.Decision.Reason, "idle") ||
			!strings.Contains(row.Decision.Reason, family.Key) {
			t.Fatalf("idle record = %+v, want a park naming the idle family", row)
		}
	}
	if second := f.scan(t); second.Recorded != 0 || second.Unchanged != 2 {
		t.Fatalf("unchanged idle family = %+v, want nothing recorded", second)
	}
}

// An owner's declared retention contract is explicit intent: the family
// being idle never overrides it.
func TestIdleFamilyStillPlansDeclaredRetention(t *testing.T) {
	family := &Family{Key: "test_memory_*:99aa00bb", Members: familyMembers(5), Idle: true}
	item := Invariant{Kind: InvariantUnboundedAppend, Schema: family.Members[0],
		Table: "events", RetentionColumn: "created_at", Family: family}
	f := newDedupeFixture(item)
	f.contracts.contracts[item.Target()] = TableContract{AppendOnly: true,
		RetentionWindow: 30 * 24 * time.Hour, RetentionColumn: "created_at"}
	result := f.scan(t)
	if result.Routed != 1 || result.Skipped != 0 ||
		f.router.items[0].Decision.Disposition != DispositionDryRun {
		t.Fatalf("contracted retention in idle family = %+v routes=%+v", result,
			f.router.items)
	}
}
