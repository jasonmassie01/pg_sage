package notify

import (
	"strings"
	"testing"
)

// vacuum-05b: the escalation of a failing emergency VACUUM streak names its
// profile; a normal streak carries none. No concurrent access tests: the
// constructor is a pure function.

func TestCustodianEscalationEventNamesTheEmergencyProfile(t *testing.T) {
	evt := CustodianEscalationEvent(CustodianEscalation{Database: "orders",
		Feature: "freeze", Target: "public.events",
		SQL:       `VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."events"`,
		SQLSHA256: "e1", Failures: 3, Window: "6h0m0s", DecisionID: 9,
		Profile: "emergency"})
	if evt.Data["profile"] != "emergency" {
		t.Fatalf("data profile = %#v, want emergency", evt.Data["profile"])
	}
	if !strings.Contains(evt.Body, "Profile: emergency") {
		t.Fatalf("body %q does not name the emergency profile", evt.Body)
	}
	if evt.DedupKey != "custodian_backoff:orders:e1" {
		t.Fatalf("DedupKey = %q, want the statement's own key", evt.DedupKey)
	}
}

func TestCustodianEscalationEventWithoutProfileOmitsIt(t *testing.T) {
	evt := CustodianEscalationEvent(CustodianEscalation{Database: "orders",
		Target: "public.events", SQLSHA256: "e2", Failures: 3})
	if _, ok := evt.Data["profile"]; ok || strings.Contains(evt.Body, "Profile:") {
		t.Fatalf("normal escalation names a profile: %#v / %q", evt.Data, evt.Body)
	}
}
