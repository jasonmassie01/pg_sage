package tuner

import (
	"strings"
	"testing"
)

func TestFilterSelfMonitoringCandidates(t *testing.T) {
	candidates := []candidate{
		{
			QueryID: 1,
			Query:   "SELECT data FROM sage.snapshots WHERE category = $1",
			Calls:   100,
		},
		{
			QueryID: 2,
			Query:   "SELECT * FROM public.orders WHERE id = $1",
			Calls:   100,
		},
	}

	got := filterSelfMonitoringCandidates(candidates)
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1", len(got))
	}
	if got[0].QueryID != 2 {
		t.Fatalf("queryid = %d, want 2", got[0].QueryID)
	}
}

func TestCandidateSQLFiltersSelfMonitoringQueriesBeforeLimit(t *testing.T) {
	if !strings.Contains(qualifiedCandidateSQL, "current_database()") {
		t.Fatal("candidateSQL must scope candidates to current database")
	}
	if !strings.Contains(qualifiedCandidateSQL, "NOT ILIKE '%pg_sage%'") {
		t.Fatal("candidateSQL must filter pg_sage text before LIMIT")
	}
	if !strings.Contains(qualifiedCandidateSQL, `"?sage"?`) {
		t.Fatal("candidateSQL must filter sage schema references before LIMIT")
	}
}

// qualifiedCandidateSQL is candidateSQL over a qualified view
// (security-01c-q made the statement a builder).
var qualifiedCandidateSQL = candidateSQL(`"ext"."pg_stat_statements"`)
