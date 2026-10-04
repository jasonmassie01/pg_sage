package tuner

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: the tuner prescribes hints for workload, never for an
// EXPLAIN, a maintenance statement or a backup COPY.
func TestFilterCandidatesDropsDiagnosticStatements(t *testing.T) {
	got := filterSelfMonitoringCandidates([]candidate{
		{QueryID: 1, Query: "EXPLAIN (ANALYZE) SELECT * FROM orders", Calls: 100},
		{QueryID: 2, Query: "ANALYZE public.orders", Calls: 100},
		{QueryID: 3, Query: "COPY public.orders TO stdout", Calls: 100},
		{QueryID: 4, Query: "SELECT * FROM public.orders WHERE id = $1", Calls: 100},
	})
	if len(got) != 1 || got[0].QueryID != 4 {
		t.Fatalf("candidates = %+v, want only the application query 4", got)
	}
}

// The filter must run before LIMIT 50, in SQL.
func TestCandidateSQLAppliesTheWorkloadRuleBeforeLimit(t *testing.T) {
	if !strings.Contains(candidateSQL, workload.AdviceSQL("query")) {
		t.Fatalf("candidateSQL lacks the workload predicate:\n%s", candidateSQL)
	}
}
