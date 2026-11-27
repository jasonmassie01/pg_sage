package siem

import (
	"strings"
	"testing"
)

// facts-07: a fact history event is an entity change naming the fact, the
// verb and the actor; revoking or expiring a confirmed fact (lifting a
// guard) is raised to medium severity, other decisions stay informational.
func TestFactEventMapping(t *testing.T) {
	cases := []struct {
		row      map[string]any
		severity int
		words    []string
	}{
		{map[string]any{"fact_id": 12, "verb": "rejected", "actor": "bob@example.com",
			"from_status": "confirmed", "to_status": "rejected"}, SeverityMedium,
			[]string{"fact #12", "rejected", "bob@example.com", "confirmed -> rejected"}},
		{map[string]any{"fact_id": 12, "verb": "expired", "actor": "pg_sage:verifier",
			"from_status": "confirmed", "to_status": "expired"}, SeverityMedium,
			[]string{"expired", "pg_sage:verifier"}},
		{map[string]any{"fact_id": 3, "verb": "confirmed", "actor": "alice",
			"from_status": "proposed", "to_status": "confirmed"}, SeverityInformational,
			[]string{"fact #3", "confirmed by alice"}},
		{map[string]any{"fact_id": 4, "verb": "rejected", "actor": "alice",
			"from_status": "proposed", "to_status": "rejected"}, SeverityInformational,
			[]string{"fact #4"}},
		{nil, SeverityInformational, []string{"fact #"}},
	}
	for _, c := range cases {
		e := Map(Record{Source: "orders", Chain: "fact_events",
			Link: link("fact_events", 1, "I"), Row: c.row})
		msg, _ := e["message"].(string)
		if num(t, e, "class_uid") != ClassEntityManagement || num(t, e, "activity_id") != 3 ||
			num(t, e, "severity_id") != c.severity {
			t.Fatalf("row %v: class %v activity %v severity %v", c.row, e["class_uid"],
				e["activity_id"], e["severity_id"])
		}
		for _, w := range c.words {
			if !strings.Contains(msg, w) {
				t.Fatalf("row %v: message %q lacks %q", c.row, msg, w)
			}
		}
	}
}
