package cases

import "testing"

// A recurring incident's case carries the link to its predecessor; the
// API fills the count and the predecessor's resolution (cases-07).

func TestProjectIncident_RecurrenceCarriesThePredecessor(t *testing.T) {
	c := ProjectIncident(SourceIncident{ID: "b", DatabaseName: "prod",
		Severity: SeverityWarning, Source: "deterministic", PreviousIncidentID: "a"})
	if c.Recurrence == nil || c.Recurrence.PreviousID != "a" || c.Recurrence.Count != 0 ||
		c.Recurrence.PreviousResolvedAt != nil {
		t.Fatalf("recurrence = %+v, want a link to a with nothing else", c.Recurrence)
	}
	if c.ID != "incident:prod:deterministic:b" {
		t.Fatalf("identity = %q, want the incident UUID kept", c.ID)
	}
}

func TestProjectIncident_FirstOccurrenceHasNoRecurrence(t *testing.T) {
	c := ProjectIncident(SourceIncident{ID: "a", DatabaseName: "prod",
		Severity: SeverityWarning, Source: "deterministic"})
	if c.Recurrence != nil {
		t.Fatalf("recurrence = %+v, want nil", c.Recurrence)
	}
	if c.Urgency.Assessed {
		t.Fatalf("an incident invented urgency: %+v", c.Urgency)
	}
}
