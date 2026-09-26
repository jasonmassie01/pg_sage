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
