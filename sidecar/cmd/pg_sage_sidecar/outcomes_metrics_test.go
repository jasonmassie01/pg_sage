package main

import (
	"strings"
	"testing"
)

// pg_sage_outcomes_measured_ratio is rendered per database from the last
// default-window ledger read, and has no sample before one.
func TestWriteOutcomesMetrics(t *testing.T) {
	var empty strings.Builder
	writeOutcomesMetrics(&empty, nil)
	if !strings.Contains(empty.String(), "# TYPE pg_sage_outcomes_measured_ratio gauge") ||
		strings.Contains(empty.String(), "pg_sage_outcomes_measured_ratio{") {
		t.Fatalf("no reads yet:\n%s", empty.String())
	}
	var b strings.Builder
	writeOutcomesMetrics(&b, map[string]float64{"orders": 0.333, "billing": 1})
	out := b.String()
	for _, want := range []string{`pg_sage_outcomes_measured_ratio{database="billing"} 1`,
		`pg_sage_outcomes_measured_ratio{database="orders"} 0.333`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `"billing"`) > strings.Index(out, `"orders"`) {
		t.Fatalf("databases not sorted:\n%s", out)
	}
}
