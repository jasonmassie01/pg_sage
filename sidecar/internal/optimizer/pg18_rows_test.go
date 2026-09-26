package optimizer

import "testing"

// PG18 reports "Actual Rows" with two decimals; the summary must still parse.
func TestSummarizePlanAcceptsPG18FractionalActualRows(t *testing.T) {
	plan := []byte(`[{"Plan":{"Node Type":"Seq Scan","Total Cost":12.5,
		"Plan Rows":100,"Actual Rows":37.50,"Rows Removed by Filter":900}}]`)
	ps := summarizePlan(plan, 42)
	if ps.ScanType != "Seq Scan" || ps.RowsRemoved != 900 {
		t.Fatalf("summary = %+v, want Seq Scan with 900 rows removed", ps)
	}
}
