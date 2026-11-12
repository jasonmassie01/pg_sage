package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
)

// sourceIncidentFromMap keeps the previous_incident_id incidentsBaseSQL
// selects (cases-07); it was dropped, so recurrences were unlinked.
func TestSourceIncidentFromMap_KeepsThePreviousIncident(t *testing.T) {
	got := sourceIncidentFromMap(map[string]any{"id": "b", "database_name": "prod",
		"severity": "warning", "previous_incident_id": "a"})
	if got.PreviousIncidentID != "a" {
		t.Fatalf("PreviousIncidentID = %q, want a", got.PreviousIncidentID)
	}
	for _, v := range []any{nil, ""} {
		got = sourceIncidentFromMap(map[string]any{"id": "b", "previous_incident_id": v})
		if got.PreviousIncidentID != "" {
			t.Fatalf("previous %v gave %q, want empty", v, got.PreviousIncidentID)
		}
	}
}

func TestEnrichIncidentRecurrence_NoLinksRunsNoQuery(t *testing.T) {
	// A nil pool would panic if a lookup were attempted.
	enrichIncidentRecurrence(t.Context(), nil, nil)
	list := []cases.Case{{ID: "finding:prod:x"}, {ID: "incident:prod:y"}}
	enrichIncidentRecurrence(t.Context(), nil, list)
	for _, c := range list {
		if c.Recurrence != nil {
			t.Fatalf("case %s gained recurrence %+v without a link", c.ID, c.Recurrence)
		}
	}
}
