package autonomy

import (
	"strings"
	"testing"
)

// The performance gate judges one schema guard statement against its own
// mean ceiling instead of the 100 ms budget, by its tag: the structural
// pass's column summary, which on the first pass after startup and on
// the daily full pass reads every column of every user table. The tag
// must lead the statement (the wire tagger moves it after the first
// keyword, where pg_stat_statements keeps it), and no other schema guard
// statement may carry it: the table listing, the text types, the column
// versions and the other scans stay judged by the plain budget.
func TestStructuralColumnSummaryCarriesTheGateTag(t *testing.T) {
	const tag = "/* pg_sage structural:columns */"
	if !strings.HasPrefix(structuralColumnsSQL, tag+"\n") {
		t.Fatalf("column summary does not lead with %s:\n%s", tag, structuralColumnsSQL)
	}
	for name, sql := range map[string]string{
		"table listing": structuralTablesSQL, "text types": structuralTextTypesSQL,
		"column versions": structuralVersionsSQL, "change counters": catalogChangeSQL,
		"missing FK indexes": missingFKIndexSQL, "unbounded append": unboundedAppendSQL,
		"schema shapes": schemaShapesSQL, "sessions": sessionsSQL,
		"table contracts": tableContractsSQL, "schema history": schemaHistorySQL,
		"statements": statementsSQL,
	} {
		if strings.Contains(sql, "structural:columns") {
			t.Fatalf("%s statement carries the column summary's gate tag:\n%s", name, sql)
		}
	}
}
