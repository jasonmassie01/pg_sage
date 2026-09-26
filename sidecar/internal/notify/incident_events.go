package notify

import (
	"fmt"
	"strings"
)

// IncidentInfo is the incident payload carried by incident events. It is
// a plain struct so notify stays independent of the rca package.
type IncidentInfo struct {
	ID                 string
	Database           string
	Severity           string
	RootCause          string
	Source             string
	SignalIDs          []string
	PreviousIncidentID string
	ResolvedBy         string
	Reason             string
}

// IncidentDetectedEvent is emitted once when a new incident is durably
// recorded. Severity follows the incident.
func IncidentDetectedEvent(info IncidentInfo) Event {
	return incidentEvent("incident_detected", "Incident detected",
		incidentSeverity(info.Severity), info)
}

// IncidentEscalatedEvent is emitted once when an incident escalates to
// critical.
func IncidentEscalatedEvent(info IncidentInfo) Event {
	return incidentEvent("incident_escalated", "Incident escalated",
		"critical", info)
}

// IncidentResolvedEvent is emitted once when an incident is resolved,
// automatically or by an operator. It keeps the incident severity so the
// same min_severity rule that delivered the detection also delivers the
// resolution.
func IncidentResolvedEvent(info IncidentInfo) Event {
	return incidentEvent("incident_resolved", "Incident resolved",
		incidentSeverity(info.Severity), info)
}

func incidentSeverity(sev string) string {
	if _, ok := ValidSeverities[sev]; ok {
		return sev
	}
	return "warning"
}

func incidentEvent(typ, verb, severity string, info IncidentInfo) Event {
	body := fmt.Sprintf(
		"Database: %s\nIncident: %s\nSeverity: %s\nSource: %s\n"+
			"Signals: %s\nRoot cause: %s",
		info.Database, info.ID, info.Severity, info.Source,
		strings.Join(info.SignalIDs, ", "), info.RootCause)
	if info.PreviousIncidentID != "" {
		body += "\nRecurrence of: " + info.PreviousIncidentID
	}
	if info.ResolvedBy != "" {
		body += fmt.Sprintf("\nResolved by: %s\nReason: %s",
			info.ResolvedBy, info.Reason)
	}
	return Event{
		Type:     typ,
		Severity: severity,
		Subject:  fmt.Sprintf("%s: %s", verb, info.RootCause),
		Body:     body,
		Data: map[string]any{
			"incident_id":          info.ID,
			"database":             info.Database,
			"severity":             info.Severity,
			"root_cause":           info.RootCause,
			"source":               info.Source,
			"signal_ids":           info.SignalIDs,
			"previous_incident_id": info.PreviousIncidentID,
			"resolved_by":          info.ResolvedBy,
			"reason":               info.Reason,
		},
	}
}
