package notify

import (
	"strings"
	"testing"
)

func sampleIncident() IncidentInfo {
	return IncidentInfo{
		ID:                 "8d3c0e1a-0000-4000-8000-000000000001",
		Database:           "orders",
		Severity:           "warning",
		RootCause:          "Connection pool saturated",
		Source:             "deterministic",
		SignalIDs:          []string{"connections_high"},
		PreviousIncidentID: "8d3c0e1a-0000-4000-8000-000000000000",
	}
}

func TestIncidentEventTypesAreValid(t *testing.T) {
	for _, typ := range []string{
		"incident_detected", "incident_escalated", "incident_resolved",
	} {
		if !ValidEventTypes[typ] {
			t.Errorf("ValidEventTypes[%q] = false, want true", typ)
		}
	}
}

func TestIncidentDetectedEvent(t *testing.T) {
	evt := IncidentDetectedEvent(sampleIncident())
	if evt.Type != "incident_detected" {
		t.Errorf("Type = %q", evt.Type)
	}
	if evt.Severity != "warning" {
		t.Errorf("Severity = %q, want incident severity warning",
			evt.Severity)
	}
	if evt.Data["incident_id"] != sampleIncident().ID ||
		evt.Data["database"] != "orders" ||
		evt.Data["previous_incident_id"] != sampleIncident().PreviousIncidentID {
		t.Errorf("Data = %v", evt.Data)
	}
	if !strings.Contains(evt.Subject, "Connection pool saturated") ||
		!strings.Contains(evt.Body, "orders") {
		t.Errorf("Subject/Body missing context: %q / %q",
			evt.Subject, evt.Body)
	}
}

func TestIncidentEscalatedEventIsCritical(t *testing.T) {
	evt := IncidentEscalatedEvent(sampleIncident())
	if evt.Type != "incident_escalated" || evt.Severity != "critical" {
		t.Errorf("Type/Severity = %q/%q, want incident_escalated/critical",
			evt.Type, evt.Severity)
	}
}

func TestIncidentResolvedEventKeepsSeverityAndActor(t *testing.T) {
	info := sampleIncident()
	info.Severity = "critical"
	info.ResolvedBy = "user:ops@example.com"
	info.Reason = "restarted pool"
	evt := IncidentResolvedEvent(info)
	if evt.Type != "incident_resolved" {
		t.Errorf("Type = %q", evt.Type)
	}
	// Resolution must route through the same min_severity rules as the
	// detection, or a critical page would never be resolved.
	if evt.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", evt.Severity)
	}
	if evt.Data["resolved_by"] != "user:ops@example.com" ||
		evt.Data["reason"] != "restarted pool" {
		t.Errorf("Data = %v", evt.Data)
	}
}

func TestIncidentEventInvalidSeverityFallsBackToWarning(t *testing.T) {
	info := sampleIncident()
	info.Severity = ""
	if evt := IncidentDetectedEvent(info); evt.Severity != "warning" {
		t.Errorf("Severity = %q, want warning for unknown", evt.Severity)
	}
}

// --- alerting-01: one PagerDuty incident per pg_sage incident ----------

func TestIncidentEventsShareDedupKeyAcrossLifecycle(t *testing.T) {
	info := sampleIncident()
	want := "incident:orders:" + info.ID
	for _, evt := range []Event{
		IncidentDetectedEvent(info),
		IncidentEscalatedEvent(info),
		IncidentResolvedEvent(info),
	} {
		if evt.DedupKey != want {
			t.Errorf("%s DedupKey = %q, want %q", evt.Type, evt.DedupKey, want)
		}
		if got := EventDedupKey(evt); got != want {
			t.Errorf("%s EventDedupKey = %q, want %q", evt.Type, got, want)
		}
	}
}

// The incident severity escalation must still read as an update, and the
// resolution as a resolve, or PagerDuty pages again on recovery.
func TestIncidentEventsResolveFlag(t *testing.T) {
	info := sampleIncident()
	if IncidentDetectedEvent(info).Resolve {
		t.Error("incident_detected has Resolve=true")
	}
	if IncidentEscalatedEvent(info).Resolve {
		t.Error("incident_escalated has Resolve=true")
	}
	resolved := IncidentResolvedEvent(info)
	if !resolved.Resolve {
		t.Fatal("incident_resolved has Resolve=false: recovery would page")
	}
	if resolved.Type != "incident_resolved" {
		t.Fatalf("Type = %q, want incident_resolved", resolved.Type)
	}
}

// Boundary: the subject embeds the root cause, which the RCA engine may
// rewrite between detection and resolution. The key must not follow it.
func TestIncidentDedupKeyIgnoresRootCauseChange(t *testing.T) {
	info := sampleIncident()
	detected := IncidentDetectedEvent(info)
	info.RootCause = "Idle-in-transaction session holds locks"
	resolved := IncidentResolvedEvent(info)
	if detected.Subject == resolved.Subject {
		t.Fatalf("test setup: subjects should differ, both %q", detected.Subject)
	}
	if EventDedupKey(detected) != EventDedupKey(resolved) {
		t.Fatalf("key changed with root cause: %q -> %q",
			EventDedupKey(detected), EventDedupKey(resolved))
	}
}

func TestIncidentRecurrenceHasDistinctKey(t *testing.T) {
	first := sampleIncident()
	again := sampleIncident()
	again.ID = "8d3c0e1a-0000-4000-8000-000000000002"
	if EventDedupKey(IncidentDetectedEvent(first)) ==
		EventDedupKey(IncidentDetectedEvent(again)) {
		t.Fatal("two incident ids share one dedup key")
	}
}

// Fleet: two databases that both raise an incident never share a key,
// even with a colon in the database name (keys are opaque to PagerDuty).
func TestIncidentDedupKeyIncludesDatabaseVerbatim(t *testing.T) {
	info := sampleIncident()
	info.Database = "tenant:eu-1"
	got := IncidentDetectedEvent(info).DedupKey
	if got != "incident:tenant:eu-1:"+info.ID {
		t.Fatalf("DedupKey = %q", got)
	}
	other := info
	other.Database = "tenant:us-1"
	if got == IncidentDetectedEvent(other).DedupKey {
		t.Fatal("two databases share one incident key")
	}
}

// Nil/empty: without an incident id there is no stable identity, so the
// event keeps the generic type+database+subject key instead of collapsing
// every id-less incident of a database into one PagerDuty incident.
func TestIncidentEmptyIDFallsBackToEventDedupKey(t *testing.T) {
	info := sampleIncident()
	info.ID = ""
	evt := IncidentDetectedEvent(info)
	if evt.DedupKey != "" {
		t.Fatalf("DedupKey = %q, want empty for an id-less incident",
			evt.DedupKey)
	}
	got := EventDedupKey(evt)
	if !strings.HasPrefix(got, "incident_detected:orders:") ||
		len(got) <= len("incident_detected:orders:") {
		t.Fatalf("EventDedupKey = %q, want incident_detected:orders:<hash>", got)
	}
	zero := IncidentResolvedEvent(IncidentInfo{})
	if zero.DedupKey != "" || !zero.Resolve || zero.Severity != "warning" {
		t.Fatalf("zero-value resolved event = %+v", zero)
	}
}
