package cases

import "testing"

// D1: set_table_autovacuum ships ALTER TABLE ... RESET (...) as its
// rollback, so case candidates report it as reversible, matching the
// executor contract.
func TestAutovacuumTuningRollbackClassIsReversible(t *testing.T) {
	if got := rollbackClassForAction("set_table_autovacuum"); got != "reversible" {
		t.Fatalf("rollbackClassForAction = %q, want reversible", got)
	}
	candidate := autovacuumTuningCandidate(SourceFinding{
		RecommendedSQL: "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)",
	})
	if candidate.RollbackClass != "reversible" {
		t.Fatalf("candidate RollbackClass = %q, want reversible", candidate.RollbackClass)
	}
}
