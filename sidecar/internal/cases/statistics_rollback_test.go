package cases

import "testing"

// Owner decision 2026-10-04 (PR #110): the rollback shipped with a
// CREATE STATISTICS candidate is the drop of exactly the created object,
// derived for the pg_sage form only. Any other form gets no rollback
// script (the executor refuses to run it).
func TestCreateStatisticsCandidateCarriesItsDrop(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE STATISTICS public.sage_stx_orders_ab (dependencies) ON a, b " +
			"FROM public.orders;": "DROP STATISTICS IF EXISTS public.sage_stx_orders_ab;",
		"CREATE STATISTICS stats_orders ON a, b FROM public.orders": "",
		"CREATE STATISTICS public.st_x ON a, b FROM public.orders":  "",
	} {
		candidate := createStatisticsCandidate(SourceFinding{RecommendedSQL: sql})
		if candidate.ScriptOutput == nil {
			t.Fatalf("%q: no script output", sql)
		}
		if got := candidate.ScriptOutput.RollbackSQL; got != want {
			t.Errorf("%q: rollback = %q, want %q", sql, got, want)
		}
	}
}
