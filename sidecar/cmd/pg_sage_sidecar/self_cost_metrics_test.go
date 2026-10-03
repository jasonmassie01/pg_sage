package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// pg_sage reports its own bill: DB time, statements, blocks and sage-table
// rows per collector cycle, the sage schema's size and the budget, per
// database. Unknown rates are not exported (no fake zeros).
func TestWriteSelfCostMetrics(t *testing.T) {
	var b strings.Builder
	writeSelfCostMetrics(&b, map[string]selfcost.Cost{
		"zeta": {Known: true, DBTimeKnown: true, DBTimeMsPerCycle: 1234.5,
			CallsPerCycle: 40, BlocksPerCycle: 900, RowsReadPerCycle: 1200,
			RowsWrittenPerCycle: 35, SchemaBytes: 1048576},
		"alpha": {SchemaBytes: 2048}, // first cycle: size only
		"beta":  {Known: true, RowsReadPerCycle: 7, SchemaBytes: 10},
	}, 3000)
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_self_db_time_ms_per_cycle gauge",
		`pg_sage_self_db_time_ms_per_cycle{database="zeta"} 1234.5`,
		`pg_sage_self_statements_per_cycle{database="zeta"} 40`,
		`pg_sage_self_blocks_per_cycle{database="zeta"} 900`,
		`pg_sage_self_rows_read_per_cycle{database="zeta"} 1200`,
		`pg_sage_self_rows_written_per_cycle{database="zeta"} 35`,
		`pg_sage_self_schema_bytes{database="zeta"} 1048576`,
		`pg_sage_self_schema_bytes{database="alpha"} 2048`,
		`pg_sage_self_rows_read_per_cycle{database="beta"} 7`,
		"pg_sage_self_db_time_budget_ms 3000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		`pg_sage_self_db_time_ms_per_cycle{database="alpha"}`,
		`pg_sage_self_rows_read_per_cycle{database="alpha"}`,
		`pg_sage_self_db_time_ms_per_cycle{database="beta"}`,
	} {
		if strings.Contains(out, absent) {
			t.Errorf("unknown value exported: %q", absent)
		}
	}
	if strings.Index(out, `database="alpha"`) > strings.Index(out, `database="zeta"`) {
		t.Error("databases are not exported in a stable sorted order")
	}
}

func TestWriteSelfCostMetrics_EmptyWritesNothing(t *testing.T) {
	var b strings.Builder
	writeSelfCostMetrics(&b, nil, 3000)
	if b.Len() != 0 {
		t.Fatalf("no databases wrote %q", b.String())
	}
}

func TestWriteSelfCostMetrics_EscapesDatabaseLabel(t *testing.T) {
	var b strings.Builder
	writeSelfCostMetrics(&b, map[string]selfcost.Cost{`we"ird`: {SchemaBytes: 1}}, 0)
	if !strings.Contains(b.String(), `pg_sage_self_schema_bytes{database="we\"ird"} 1`) {
		t.Fatalf("label not escaped:\n%s", b.String())
	}
	if strings.Contains(b.String(), "pg_sage_self_db_time_budget_ms") {
		t.Fatal("a disabled budget (0) must not be exported as a budget")
	}
}
