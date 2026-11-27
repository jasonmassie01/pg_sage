package notify

import "fmt"

// FindingOpenedEvent is a finding that opened, reopened or escalated, at
// its own severity, so rules route it by min_severity like an alerting:
// route (alerting-06). Its key is the finding's identity on its database.
func FindingOpenedEvent(title, detail, database, category, object, severity string) Event {
	if _, ok := ValidSeverities[severity]; !ok {
		severity = "info"
	}
	return Event{
		Type:     "finding_opened",
		Severity: severity,
		Subject:  fmt.Sprintf("%s finding: %s", severityLabel[severity], title),
		Body:     fmt.Sprintf("Database: %s\nDetail: %s", database, detail),
		Data: map[string]any{"title": title, "detail": detail, "database": database,
			"category": category, "object": object},
		DedupKey: findingAlertKey(database, category, object),
	}
}

// FindingResolvedEvent closes the finding_opened alert of the same finding.
func FindingResolvedEvent(title, database, category, object, severity string) Event {
	evt := FindingOpenedEvent(title, "no longer detected", database, category, object,
		severity)
	evt.Type = "finding_resolved"
	evt.Resolve = true
	evt.Subject = "Resolved: " + title
	return evt
}

var severityLabel = map[string]string{
	"critical": "Critical", "warning": "Warning", "info": "Info"}

func findingAlertKey(database, category, object string) string {
	return fmt.Sprintf("finding:%s:%s:%s", database, category, object)
}

// AlertsFindings reports whether this dispatcher takes finding_opened and
// finding_resolved: only with the outbox, whose alert state throttles
// them. The analyzer sends them only then.
func (d *Dispatcher) AlertsFindings() bool { return d.outbox != nil }
