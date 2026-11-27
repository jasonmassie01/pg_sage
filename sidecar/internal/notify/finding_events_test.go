package notify

import (
	"strings"
	"testing"
)

// alerting-01: a critical finding's page and its resolve share the
// finding's identity (database, category, object), not its wording.

func TestFindingCriticalEventKeyedByIdentity(t *testing.T) {
	evt := FindingCriticalEvent("XID wraparound", "age 1.9B", "billing",
		"xid_wraparound", "current_database")
	want := "finding_critical:billing:xid_wraparound:current_database"
	if evt.DedupKey != want {
		t.Fatalf("DedupKey = %q, want %q", evt.DedupKey, want)
	}
	if evt.Data["category"] != "xid_wraparound" ||
		evt.Data["object"] != "current_database" ||
		evt.Data["database"] != "billing" {
		t.Fatalf("Data = %v, want category, object and database", evt.Data)
	}
	if evt.Type != "finding_critical" || evt.Severity != "critical" ||
		evt.Resolve {
		t.Fatalf("event = %+v", evt)
	}
}

// Boundary: the title and detail change between cycles (counts, ages);
// the key must not.
func TestFindingCriticalKeyIgnoresTitleAndDetail(t *testing.T) {
	a := FindingCriticalEvent("Slot s1 inactive for 2h", "2h", "db1",
		"replication_slot_inactive", "s1")
	b := FindingCriticalEvent("Slot s1 inactive for 9h", "9h", "db1",
		"replication_slot_inactive", "s1")
	if a.Subject == b.Subject {
		t.Fatal("test setup: subjects should differ")
	}
	if EventDedupKey(a) != EventDedupKey(b) {
		t.Fatalf("key followed the title: %q vs %q",
			EventDedupKey(a), EventDedupKey(b))
	}
}

func TestFindingCriticalDistinctIdentitiesDistinctKeys(t *testing.T) {
	base := FindingCriticalEvent("t", "d", "db1", "xid_wraparound", "orders")
	for _, other := range []Event{
		FindingCriticalEvent("t", "d", "db1", "xid_wraparound", "items"),
		FindingCriticalEvent("t", "d", "db1", "bloat", "orders"),
		FindingCriticalEvent("t", "d", "db2", "xid_wraparound", "orders"),
	} {
		if EventDedupKey(other) == EventDedupKey(base) {
			t.Errorf("%v shares key %q with base", other.Data, EventDedupKey(base))
		}
	}
}

func TestResolvedCriticalFindingKeepsKeyTypeAndSeverity(t *testing.T) {
	trigger := FindingCriticalEvent("XID wraparound", "d", "billing",
		"xid_wraparound", "current_database")
	resolved := ResolvedEvent(trigger)
	if !resolved.Resolve {
		t.Fatal("Resolve = false")
	}
	if resolved.DedupKey != trigger.DedupKey {
		t.Fatalf("resolve key %q != trigger key %q",
			resolved.DedupKey, trigger.DedupKey)
	}
	// Same type and severity, so the rule that paged also resolves.
	if resolved.Type != "finding_critical" || resolved.Severity != "critical" {
		t.Fatalf("Type/Severity = %q/%q", resolved.Type, resolved.Severity)
	}
	if !strings.HasPrefix(resolved.Subject, "Resolved: Critical finding: ") {
		t.Fatalf("Subject = %q", resolved.Subject)
	}
	if trigger.Resolve {
		t.Fatal("ResolvedEvent mutated the original event")
	}
}

// Nil/empty: with neither category nor object there is no identity, so
// the generic subject-hash key applies (the pre-alerting-01 behaviour).
func TestFindingCriticalEmptyIdentityFallsBack(t *testing.T) {
	evt := FindingCriticalEvent("Disk full", "90%", "prod", "", "")
	if evt.DedupKey != "" {
		t.Fatalf("DedupKey = %q, want empty", evt.DedupKey)
	}
	if got := EventDedupKey(evt); !strings.HasPrefix(got, "finding_critical:prod:") {
		t.Fatalf("EventDedupKey = %q", got)
	}
}

// A category-wide finding (empty object) is still one identity: category
// plus the empty object, exactly as sage.findings keys it.
func TestFindingCriticalEmptyObjectKeepsCategoryKey(t *testing.T) {
	evt := FindingCriticalEvent("t", "d", "prod", "self_cost", "")
	if evt.DedupKey != "finding_critical:prod:self_cost:" {
		t.Fatalf("DedupKey = %q", evt.DedupKey)
	}
}
