package optimizer

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// indexes-05 review: a table's workload holds its UPDATE, INSERT and
// DELETE statements too. The what-if never plans them, so they must not
// take any of its 25 slots: hot writes would otherwise crowd out the
// SELECTs the candidate is for.

func TestExplainableQueriesKeepsOnlyPlannableStatements(t *testing.T) {
	qs := []QueryInfo{
		{QueryID: 1, Text: "UPDATE t SET a = 1 WHERE b = $1"},
		{QueryID: 2, Text: "  select a FROM t WHERE b = $1"},
		{QueryID: 3, Text: "INSERT INTO t VALUES ($1)"},
		{QueryID: 4, Text: "WITH x AS (SELECT 1) SELECT * FROM x"},
		{QueryID: 5, Text: "DELETE FROM t WHERE b = $1"},
		{QueryID: 6, Text: ""},
	}
	got := sampleIDs(explainableQueries(qs))
	if !slices.Equal(got, []int64{2, 4}) {
		t.Fatalf("explainable = %v, want [2 4]", got)
	}
	if len(qs) != 6 || qs[0].QueryID != 1 {
		t.Fatalf("filtering modified the caller's slice: %v", sampleIDs(qs))
	}
}

func TestExplainableQueriesNilAndAllWrites(t *testing.T) {
	if got := explainableQueries(nil); len(got) != 0 {
		t.Fatalf("nil workload filtered to %v", got)
	}
	writes := []QueryInfo{{QueryID: 1, Text: "UPDATE t SET a = 1"}}
	if got := explainableQueries(writes); len(got) != 0 {
		t.Fatalf("a write-only workload kept %v", sampleIDs(got))
	}
}

// Integration: 30 UPDATEs hotter than the one SELECT the candidate helps.
// Every UPDATE is skipped by the what-if, so the SELECT is still planned
// and the candidate measured, instead of "no workload query could be
// measured".
func TestHypoPGWritesDoNotCrowdOutReads(t *testing.T) {
	pool := hypopgSessionPool(t)
	settleItems(t)
	h := NewHypoPG(pool, noopLog2)
	var queries []QueryInfo
	for i := 1; i <= 30; i++ {
		queries = append(queries, QueryInfo{QueryID: int64(i), Calls: 1000,
			TotalTimeMs: float64(100000 - i), Text: fmt.Sprintf(
				"UPDATE hypopg_session_test.items SET id = id WHERE category=%d", i)})
	}
	queries = append(queries, QueryInfo{QueryID: 500, Calls: 10, TotalTimeMs: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	res, err := h.Validate(ctx, Recommendation{DDL: hypoCandidate}, queries)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Measured != 1 || res.Failed != 0 {
		t.Fatalf("result %+v: want the one SELECT measured and nothing failed", res)
	}
	if res.Improvement < 50 || res.SizeBytes <= 0 {
		t.Fatalf("result %+v: want a large gain on the SELECT and a size", res)
	}
	assertHypoPGSessionClean(t, pool)
}
