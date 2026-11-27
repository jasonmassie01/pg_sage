package optimizer

import (
	"slices"
	"testing"
)

// collection-10: the what-if plans what workload.Plannable says is
// read-only. A leading comment no longer hides a SELECT (the removed copy
// was prefix-only), and a writing CTE, a multi-statement string, a CALL
// and a utility statement take no slot.
func TestExplainableQueriesUsesTheWorkloadPlannableRule(t *testing.T) {
	qs := []QueryInfo{
		{QueryID: 1, Text: "/* app:report */ SELECT a FROM t WHERE b = $1"},
		{QueryID: 2, Text: "WITH x AS (SELECT 1) UPDATE t SET a = 1"},
		{QueryID: 3, Text: "SELECT 1; SELECT 2"},
		{QueryID: 4, Text: "-- nightly\nWITH x AS (SELECT 1) SELECT * FROM x"},
		{QueryID: 5, Text: "CALL p($1)"},
		{QueryID: 6, Text: "COMMIT"},
		{QueryID: 7, Text: "VALUES ($1)"},
		{QueryID: 8, Text: "MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE"},
	}
	got := sampleIDs(explainableQueries(qs))
	if !slices.Equal(got, []int64{1, 4, 7}) {
		t.Fatalf("explainable = %v, want [1 4 7]", got)
	}
}
