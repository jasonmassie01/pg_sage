package notify

import (
	"strings"
	"testing"
)

// vacuum-05: an exhausted custodian backoff raises one critical
// custodian_escalation event an operator can route.

// No concurrent access tests: the constructor is a pure function.

func TestCustodianEscalationEventCarriesTheIncidentIdentity(t *testing.T) {
	evt := CustodianEscalationEvent(CustodianEscalation{Database: "orders",
		Feature: "freeze", Target: "public.events", SQL: `VACUUM "public"."events"`,
		SQLSHA256: "abc123", Failures: 3, Window: "6h0m0s", DecisionID: 41})
	if evt.Type != "custodian_escalation" || evt.Severity != "critical" {
		t.Fatalf("event type/severity = %s/%s", evt.Type, evt.Severity)
	}
	if evt.DedupKey != "custodian_backoff:orders:abc123" {
		t.Fatalf("DedupKey = %q", evt.DedupKey)
	}
	if EventDedupKey(evt) != evt.DedupKey {
		t.Fatalf("EventDedupKey = %q, want the explicit key", EventDedupKey(evt))
	}
	for _, want := range []string{"public.events", "3 failures", "6h0m0s"} {
		if !strings.Contains(evt.Subject+evt.Body, want) {
			t.Fatalf("subject/body lack %q: %q / %q", want, evt.Subject, evt.Body)
		}
	}
	if !strings.Contains(evt.Body, `VACUUM "public"."events"`) ||
		!strings.Contains(evt.Body, "decision 41") {
		t.Fatalf("body %q lacks the SQL or the decision", evt.Body)
	}
	if evt.Data["database"] != "orders" || evt.Data["failures"] != 3 ||
		evt.Data["target"] != "public.events" || evt.Data["decision_id"] != int64(41) {
		t.Fatalf("data = %v", evt.Data)
	}
}

func TestCustodianEscalationIsARoutableCriticalEvent(t *testing.T) {
	if !ValidEventTypes["custodian_escalation"] {
		t.Fatal("custodian_escalation is not a valid rule event")
	}
	if EventSeverity["custodian_escalation"] != "critical" {
		t.Fatalf("severity = %q, want critical", EventSeverity["custodian_escalation"])
	}
	if !RuleCanFire("custodian_escalation", "critical") ||
		DefaultMinSeverity("custodian_escalation") != "critical" {
		t.Fatal("a critical rule for custodian_escalation can never fire")
	}
}

func TestCustodianEscalationEventWithoutDatabaseKeepsAKey(t *testing.T) {
	evt := CustodianEscalationEvent(CustodianEscalation{Target: "public.t",
		SQLSHA256: "ff", Failures: 3})
	if evt.DedupKey != "custodian_backoff::ff" {
		t.Fatalf("DedupKey = %q, want the empty database kept in place", evt.DedupKey)
	}
	if !strings.Contains(evt.Body, "decision none") {
		t.Fatalf("body %q, want no decision named", evt.Body)
	}
}
