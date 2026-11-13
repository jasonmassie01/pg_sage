package notify

import "testing"

// alerting-06 M3: finding_opened and finding_resolved carry the finding's
// own severity, so a rule's min_severity routes them like a YAML route.
// No concurrent access tests: pure constructors.

func TestFindingOpenedEvent_CarriesSeverityAndKey(t *testing.T) {
	evt := FindingOpenedEvent("Table bloat", "{}", "orders", "bloat", "public.t", "warning")
	if evt.Type != "finding_opened" || evt.Severity != "warning" || evt.Resolve {
		t.Fatalf("event = %+v, want a warning finding_opened", evt)
	}
	if evt.DedupKey != "finding:orders:bloat:public.t" {
		t.Fatalf("dedup key = %q", evt.DedupKey)
	}
	if evt.Data["database"] != "orders" || evt.Data["category"] != "bloat" {
		t.Fatalf("data = %v", evt.Data)
	}
}

func TestFindingResolvedEvent_ClosesTheOpenedKey(t *testing.T) {
	opened := FindingOpenedEvent("Table bloat", "{}", "orders", "bloat", "public.t", "info")
	res := FindingResolvedEvent("Table bloat", "orders", "bloat", "public.t", "info")
	if res.Type != "finding_resolved" || !res.Resolve || res.Severity != "info" {
		t.Fatalf("event = %+v, want a resolving info finding_resolved", res)
	}
	if EventDedupKey(res) != EventDedupKey(opened) {
		t.Fatalf("resolve key %q != opened key %q", EventDedupKey(res), EventDedupKey(opened))
	}
}

func TestFindingOpenedEvent_UnknownSeverityIsInfo(t *testing.T) {
	if evt := FindingOpenedEvent("x", "", "db", "c", "o", "bogus"); evt.Severity != "info" {
		t.Fatalf("severity = %q, want info", evt.Severity)
	}
}

func TestFindingAlertEvents_AreValidRuleEvents(t *testing.T) {
	for _, typ := range []string{"finding_opened", "finding_resolved"} {
		if !ValidEventTypes[typ] {
			t.Errorf("%s is not a valid rule event", typ)
		}
		if !RuleCanFire(typ, "info") || !RuleCanFire(typ, "critical") {
			t.Errorf("%s rules cannot fire at every min_severity", typ)
		}
		if DefaultMinSeverity(typ) != "warning" {
			t.Errorf("%s default min_severity = %q, want warning", typ, DefaultMinSeverity(typ))
		}
	}
}

func TestAlertsFindings_FollowsTheOutbox(t *testing.T) {
	d := NewDefaultDispatcher(NewMemoryStore(), TargetPolicy{}, nil)
	if d.AlertsFindings() {
		t.Fatal("a dispatcher without an outbox claims finding alerts")
	}
	d.UseOutbox(NewMemoryStore())
	if !d.AlertsFindings() {
		t.Fatal("an outbox dispatcher does not take finding alerts")
	}
}
