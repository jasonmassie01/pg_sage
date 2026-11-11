package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// footprint-08: pg_sage's database time per hour by component, over a
// fixed component set (anything else is "other"), per database. A new
// family: pg_sage_self_db_time_ms_per_hour{database} keeps its meaning, so
// sum by (database) over it never counts a component twice.

func TestWriteSelfComponentMetrics(t *testing.T) {
	var b strings.Builder
	writeSelfComponentMetrics(&b, map[string][]selfcost.ComponentCost{
		"zeta": {
			{Component: "sre:probe", DBTimeMsPerHour: 600},
			{Component: "sre:slo", DBTimeMsPerHour: 400},
			{Component: "collector", DBTimeMsPerHour: 1800.5},
			{Component: "audit_chain", DBTimeMsPerHour: 7},
			{Component: "guard_kill", DBTimeMsPerHour: 3},
			{Component: selfcost.Unattributed, DBTimeMsPerHour: 90},
		},
		"alpha": {{Component: "executor", DBTimeMsPerHour: 12}},
		"beta":  nil, // no window yet: nothing exported
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_self_component_db_time_ms_per_hour gauge",
		`pg_sage_self_component_db_time_ms_per_hour{database="zeta",component="collector"} 1800.5`,
		`pg_sage_self_component_db_time_ms_per_hour{database="zeta",component="sre"} 1000`,
		`pg_sage_self_component_db_time_ms_per_hour{database="zeta",component="other"} 10`,
		`pg_sage_self_component_db_time_ms_per_hour{database="zeta",component="unattributed"} 90`,
		`pg_sage_self_component_db_time_ms_per_hour{database="alpha",component="executor"} 12`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{`database="beta"`, `component="sre:probe"`,
		`component="audit_chain"`, `{database="alpha",component="collector"}`} {
		if strings.Contains(out, absent) {
			t.Errorf("metrics carry %q:\n%s", absent, out)
		}
	}
	if strings.Index(out, `database="alpha"`) > strings.Index(out, `database="zeta"`) {
		t.Error("databases are not in a stable sorted order")
	}
	if strings.Count(out, "# HELP pg_sage_self_component_db_time_ms_per_hour") != 1 {
		t.Errorf("HELP written more than once:\n%s", out)
	}
}

// Nil/empty: no database with a window writes no family at all.
func TestWriteSelfComponentMetrics_EmptyWritesNothing(t *testing.T) {
	for name, in := range map[string]map[string][]selfcost.ComponentCost{
		"nil": nil, "no rows": {"a": nil, "b": {}},
	} {
		var b strings.Builder
		writeSelfComponentMetrics(&b, in)
		if b.Len() != 0 {
			t.Errorf("%s wrote %q", name, b.String())
		}
	}
}

// The label set is bounded: only MetricComponents values appear.
func TestWriteSelfComponentMetrics_BoundedLabels(t *testing.T) {
	rows := []selfcost.ComponentCost{}
	for _, c := range []string{"a", "b:c", "d.e", "x-y", "collector:z", "sre"} {
		rows = append(rows, selfcost.ComponentCost{Component: c, DBTimeMsPerHour: 1})
	}
	var b strings.Builder
	writeSelfComponentMetrics(&b, map[string][]selfcost.ComponentCost{"db": rows})
	allowed := map[string]bool{}
	for _, c := range selfcost.MetricComponents {
		allowed[`component="`+c+`"`] = true
	}
	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.HasPrefix(line, "pg_sage_self_component") {
			continue
		}
		i := strings.Index(line, `component="`)
		j := strings.Index(line[i:], `"}`)
		if i < 0 || j < 0 || !allowed[line[i:i+j+1]] {
			t.Errorf("label outside the fixed set: %s", line)
		}
	}
}
